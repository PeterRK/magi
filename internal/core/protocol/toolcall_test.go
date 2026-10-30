package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func validateContract(name string, body []byte) error {
	schema, err := Contract(name)
	if err != nil {
		return err
	}
	_, err = decodeValidatedJSON(schema, body)
	return err
}

func TestDisallowedToolFeedbackNamesAvailableTools(t *testing.T) {
	for _, tc := range []struct {
		names []string
		hint  string
	}{
		{[]string{"history_read", "show_user"}, "allowed tools: history_read, show_user"},
		{[]string{"finish_job"}, "allowed tools: finish_job"},
		{nil, "no tools are available in this request"},
	} {
		var tools []Tool
		for _, name := range tc.names {
			schema, err := Contract(name)
			if err != nil {
				t.Fatal(err)
			}
			tools = append(tools, Tool{Type: "function", Function: Function{Name: name, Parameters: schema}})
		}
		call := ToolCall{ID: "rejected", Type: "function", Function: FunctionCall{Name: "private-name", Arguments: `{"private-key":"private-value"}`}}
		_, err := validateToolCalls(tools, []ToolCall{call}, "tool_calls")
		var refusal ToolCallBatchError
		if !errors.As(err, &refusal) || !refusal.Locatable || refusal.Hint != tc.hint || strings.Contains(refusal.FeedbackMessage(), "private-") {
			t.Fatalf("unavailable tool feedback lost scope or exposed generated content: %+v", err)
		}
	}
}

func TestSchemaFeedbackNamesMissingOrderInstruction(t *testing.T) {
	schema, err := Contract("submit_plan")
	if err != nil {
		t.Fatal(err)
	}
	args := `{"orders":[{}],"memo":""}`
	tools := []Tool{{Type: "function", Function: Function{Name: "submit_plan", Parameters: schema}}}
	call := ToolCall{ID: "original", Type: "function", Function: FunctionCall{Name: "submit_plan", Arguments: args}}
	calls := []ToolCall{call}
	_, err = validateToolCalls(tools, calls, "tool_calls")
	var refusal ToolCallBatchError
	if !errors.As(err, &refusal) || !refusal.Locatable || !strings.Contains(refusal.FeedbackMessage(), "$.orders[0].instruction: missing required property") {
		t.Fatalf("missing actionable order feedback: %+v", err)
	}
	if refusal.Message != "tool arguments do not satisfy the frozen schema" || strings.Contains(refusal.FeedbackMessage(), "private instruction") {
		t.Fatal("stable protocol identity changed or argument content was exposed")
	}
	calls[0].Function.Arguments = `{"orders":[{"instruction":"private instruction"}],"memo":""}`
	if _, err := validateToolCalls(tools, calls, "tool_calls"); err != nil {
		t.Fatal(err)
	}
}

func TestControlBatchRejectsBeforeArgumentValidation(t *testing.T) {
	var tools []Tool
	for _, name := range []string{"history_read", "submit_review"} {
		schema, err := Contract(name)
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, Tool{Type: "function", Function: Function{Name: name, Parameters: schema}})
	}
	response := Response{Message: assistantMessage(
		ToolCall{ID: "read", Type: "function", Function: FunctionCall{Name: "history_read", Arguments: "{invalid"}},
		ToolCall{ID: "review", Type: "function", Function: FunctionCall{Name: "submit_review", Arguments: "{invalid"}},
	), FinishReason: "tool_calls"}
	validated, err := ValidateToolResponse(tools, response)
	var refusal ToolCallBatchError
	if validated != nil || !errors.As(err, &refusal) || !refusal.Locatable || refusal.Message != "control call must be the only call in the batch" {
		t.Fatal("mixed control batch did not receive structural feedback", validated, err)
	}
}

func TestControlSubmissionContracts(t *testing.T) {
	for _, tc := range []struct {
		tool, body string
		valid      bool
	}{
		{"submit_plan", `{"orders":[],"memo":""}`, true},
		{"submit_plan", `{"orders":[{"instruction":"Implement the request."}],"memo":"Dispatch intent."}`, true},
		{"submit_plan", `{"orders":[]}`, false},
		{"submit_plan", `{"memo":"Discuss the request."}`, false},
		{"submit_plan", `{"orders":null,"memo":""}`, false},
		{"submit_plan", `{"orders":[],"memo":null}`, false},
		{"submit_plan", `{"orders":[],"memo":"","extra":true}`, false},
		{"submit_plan", `{"orders":[{"instruction":"inspect","extra":true}],"memo":"Investigate."}`, false},
		{"finish_job", `{"report":"Partial work; a validation remains."}`, true},
		{"finish_job", `{"report":""}`, false},
		{"finish_job", `{"report":"done","extra":[]}`, false},
		{"submit_review", `{"summary":"A reported result needs correction."}`, true},
		{"submit_review", `{"summary":"Done","decision":"HIBERNATE"}`, false},
	} {
		if err := validateContract(tc.tool, []byte(tc.body)); (err == nil) != tc.valid {
			t.Fatalf("%s body=%s error=%v", tc.tool, tc.body, err)
		}
	}
}

func TestSemanticRefusalRequiresReviewedOrigin(t *testing.T) {
	for _, failure := range []error{
		InvalidCommand("private command diagnostic"),
		Fault{Code: CodePermissionDenied, Message: "private owner scope"},
		IntegrityFailure("private source corrupt"),
		errors.New("disk error private path"),
		WithCandidateFeedback(IntegrityFailure("internal"), "do not repair"),
		WithCandidateFeedback(InvalidCommand("private"), ""),
	} {
		wrapped := fmt.Errorf("private wrapper: %w", failure)
		mapped := SemanticToolCallError(wrapped)
		var receipt ToolCallError
		if mapped != wrapped || errors.As(mapped, &receipt) {
			t.Fatalf("unreviewed or internal error became repair: %T", failure)
		}
	}
	failure := WithCandidateFeedback(fmt.Errorf("credential: %w", Fault{Code: CodePermissionDenied, Message: "private scope"}), "use only the tools available in this role")
	var receipt ToolCallError
	if !errors.As(SemanticToolCallError(fmt.Errorf("private wrapper: %w", failure)), &receipt) || receipt.Detail != "ACCESS_DENIED" {
		t.Fatal("explicitly reviewed capability hint was lost")
	}
	text := string(receipt.Receipt())
	for _, private := range []string{"private scope", "credential", "private wrapper"} {
		if strings.Contains(text, private) {
			t.Fatal("underlying diagnostic leaked")
		}
	}
}

func TestSemanticRefusalPreservesJoinedFailures(t *testing.T) {
	candidate := InvalidCandidate("correct the argument")
	fatal := Fault{Code: CodeOutcomeUnknown, Message: "effects unknown"}
	for _, failure := range []error{
		errors.Join(candidate, fatal),
		errors.Join(fatal, candidate),
		fmt.Errorf("tool: %w", errors.Join(candidate, fatal)),
		WithCandidateFeedback(errors.Join(InvalidCommand("argument"), fatal), "correct the argument"),
	} {
		if got := SemanticToolCallError(failure); got != failure {
			t.Fatalf("joined failure became a repairable refusal: %v", got)
		}
	}
}

func TestSchemaFeedbackDoesNotEchoArgumentKeysOrValues(t *testing.T) {
	for _, tc := range []struct{ schema, args, want string }{
		{`{"type":"object","additionalProperties":false}`, `{"private-key":"private-value"}`, "unknown property"},
		{`{"type":"object","properties":{"mode":{"enum":["allowed"]}}}`, `{"mode":"private-value"}`, "$.mode: value is not in enum"},
		{`{"type":"object"}`, `{"private-key":`, "tool arguments do not satisfy the frozen schema"},
	} {
		tools := []Tool{{Type: "function", Function: Function{Name: "custom", Parameters: json.RawMessage(tc.schema)}}}
		calls := []ToolCall{{ID: "bad", Type: "function", Function: FunctionCall{Name: "custom", Arguments: tc.args}}}
		_, err := validateToolCalls(tools, calls, "tool_calls")
		var refusal ToolCallBatchError
		if !errors.As(err, &refusal) || !strings.Contains(refusal.FeedbackMessage(), tc.want) {
			t.Fatalf("wrong feedback: %+v", err)
		}
		if strings.Contains(refusal.FeedbackMessage(), "private-") {
			t.Fatal("argument text leaked into feedback")
		}
	}
}

func TestSchemaFeedbackExplainsSyntaxAndPackaging(t *testing.T) {
	schema, err := Contract("submit_plan")
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"orders":[],"memo":""}`
	encoded, _ := json.Marshal(valid)
	for _, tc := range []struct{ name, args, want string }{
		{"extra brace", valid + `}`, "extra data after the complete JSON value"},
		{"extra closing tokens", valid + `]}`, "extra data after the complete JSON value"},
		{"missing separator", `{"orders":[] "completion":{}}`, "expected ',' between properties or '}'"},
		{"missing colon", `{"orders" []}`, "expected ':'"},
		{"incomplete", `{"orders":[],"completion":`, "incomplete JSON value"},
		{"root string", string(encoded), "expected object; pass tool parameters directly"},
		{"missing root property", `{"orders":[]}`, "$.memo: missing required property"},
		{"arguments wrapper", `{"arguments":` + string(encoded) + `}`, "without an arguments wrapper"},
		{"duplicate", `{"orders":[],"orders":[],"memo":""}`, "duplicate object property"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := []Tool{{Type: "function", Function: Function{Name: "submit_plan", Parameters: schema}}}
			calls := []ToolCall{{ID: "original", Type: "function", Function: FunctionCall{Name: "submit_plan", Arguments: tc.args}}}
			validated, err := validateToolCalls(tools, calls, "tool_calls")
			var refusal ToolCallBatchError
			if validated != nil || !errors.As(err, &refusal) || !refusal.Locatable || !strings.Contains(refusal.FeedbackMessage(), tc.want) || len(refusal.Hint) > 512 {
				t.Fatalf("wrong rejection: calls=%v error=%+v", validated, err)
			}
			if tc.name == "missing root property" && strings.Contains(refusal.Hint, "wrapper") {
				t.Fatal("ordinary missing property was diagnosed as an arguments wrapper")
			}
			if calls[0].Function.Arguments != tc.args {
				t.Fatal("rejected request was rewritten")
			}
			calls[0].Function.Arguments = valid
			if _, err := validateToolCalls(tools, calls, "tool_calls"); err != nil {
				t.Fatalf("corrected request rejected: %v", err)
			}
		})
	}
}

func TestSchemaFeedbackKeepsNestedLocationsAndSchemaOnlyHints(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","minItems":1,"items":{"type":"object","properties":{"mode":{"type":"string","enum":["allowed"]},"text":{"type":"string","minLength":1,"pattern":"^[a-z]+$"}},"required":["mode"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)
	for _, tc := range []struct{ args, path, want string }{
		{`{"items":[{"mode":"allowed" "text":"private-value"}]}`, "$.items[0]", "expected ','"},
		{`{"items":[{"mode":"allowed"} {"mode":"allowed"}]}`, "$.items", "expected ',' between elements"},
		{`{"items":[{"mode":"private-value"}]}`, "$.items[0].mode", `allowed values: ["allowed"]`},
		{`{"items":[{"mode":"allowed","private-key":"private-value"}]}`, "$.items[0]", `allowed properties: ["mode","text"]`},
		{`{"items":[]}`, "$.items", "minItems (1)"},
		{`{"items":[{"mode":"allowed","text":"PRIVATE"}]}`, "$.items[0].text", `required pattern: "^[a-z]+$"`},
		{`{"items":[{"mode":"allowed","text":"\qprivate-value"}]}`, "$.items[0].text", "invalid string escape"},
		{`{"items":[{"mode":"allowed","text":"\ud800private-value"}]}`, "$.items[0].text", "invalid string escape"},
		{`{"items":[{"private-key":"private-value","private-key":0}]}`, "$.items[0].<additional property>", "duplicate object property"},
	} {
		tools := []Tool{{Type: "function", Function: Function{Name: "custom", Parameters: schema}}}
		calls := []ToolCall{{ID: "bad", Type: "function", Function: FunctionCall{Name: "custom", Arguments: tc.args}}}
		_, err := validateToolCalls(tools, calls, "tool_calls")
		var refusal ToolCallBatchError
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Hint, tc.path+": ") || !strings.Contains(refusal.Hint, tc.want) || len(refusal.Hint) > 512 {
			t.Fatalf("missing location or guidance for %q: %v / %s", tc.args, err, refusal.Hint)
		}
		if strings.Contains(refusal.Hint, "private-") || strings.Contains(refusal.Hint, "PRIVATE") {
			t.Fatalf("argument text leaked: %s", refusal.Hint)
		}
	}
}

func TestShowUserRequiresMessageAndExplicitReplyMode(t *testing.T) {
	for _, body := range []string{`{"message":"Question?","wait_for_reply":true}`, `{"message":"Result.","wait_for_reply":false}`} {
		if err := validateContract("show_user", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"message":"missing mode"}`, `{"wait_for_reply":true}`, `{"message":"x","wait_for_reply":"yes"}`, `{"message":"x","wait_for_reply":true,"source":{}}`} {
		if validateContract("show_user", []byte(body)) == nil {
			t.Fatalf("invalid interaction accepted: %s", body)
		}
	}
}

func TestHistoryReadAcceptsUserSource(t *testing.T) {
	for _, ref := range []string{"U1", "P2", "W3", "R4"} {
		body, _ := json.Marshal(map[string]string{"ref": ref})
		if err := validateContract("history_read", body); err != nil {
			t.Fatalf("history source %s rejected: %v", ref, err)
		}
	}
}

func TestToolArgumentsNormalizeForTypedDecodingWithoutPrecisionLoss(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"count":{"type":"integer"},"text":{"type":"string"}},"additionalProperties":false}`)
	tools := []Tool{{Type: "function", Function: Function{Name: "query", Parameters: schema}}}
	var canonical string
	for i, original := range []string{
		`{"n":9007199254740993.0,"count":1e2,"text":"é\r\n"}`,
		`{"text":"é\r\n","count":100.00,"n":9007199254740993}`,
		`{"n":9007199254740992,"count":100,"text":"é\r\n"}`,
	} {
		call := ToolCall{ID: "query", Type: "function", Function: FunctionCall{Name: "query", Arguments: original}}
		response := Response{Message: assistantMessage(call), FinishReason: "tool_calls"}
		validated, err := ValidateToolResponse(tools, response)
		if err != nil {
			t.Fatal(err)
		}
		var args struct {
			N     uint64 `json:"n"`
			Count int    `json:"count"`
			Text  string `json:"text"`
		}
		want := uint64(9007199254740993)
		if i == 2 {
			want--
		}
		if err := json.Unmarshal(validated[0].CanonicalArguments, &args); err != nil || args.N != want || args.Count != 100 || args.Text != "é\r\n" {
			t.Fatal("normalized arguments lost their meaning", args, err)
		}
		if response.Message.ToolCalls[0].Function.Arguments != original {
			t.Fatal("original model arguments were rewritten")
		}
		got := string(validated[0].CanonicalArguments)
		if i == 0 {
			canonical = got
		} else if (got == canonical) != (i == 1) {
			t.Fatal("normalization changed equivalence or collapsed distinct integers", canonical, got)
		}
	}
}

func TestFaultCodesSurviveErrorWrapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code ErrorCode
	}{
		{LifecycleConflict("invocation already active"), CodeLifecycleConflict},
		{IntegrityFailure("snapshot is corrupt"), CodeIntegrityFailure},
	} {
		for _, err := range []error{tc.err, fmt.Errorf("input drive: %w", tc.err), errors.Join(errors.New("receipt write failed"), tc.err)} {
			if !IsCode(err, tc.code) {
				t.Errorf("lost fault classification: %v", err)
			}
			if IsCode(err, CodePermissionDenied) {
				t.Errorf("incorrect classification: %v", err)
			}
		}
	}
	if IsCode(nil, CodeIntegrityFailure) || IsCode(errors.New("plain error"), CodeIntegrityFailure) {
		t.Fatal("non-fault classified")
	}
}

func TestFaultCodesMatchEveryJoinedBranch(t *testing.T) {
	limited := Fault{Code: CodeCapacityLimited, Message: "capacity exhausted"}
	unknown := Fault{Code: CodeOutcomeUnknown, Message: "effects unknown"}
	for _, err := range []error{
		errors.Join(limited, unknown),
		errors.Join(unknown, limited),
		fmt.Errorf("batch: %w", errors.Join(limited, fmt.Errorf("tool: %w", unknown))),
	} {
		if !IsCode(err, CodeCapacityLimited) || !IsCode(err, CodeOutcomeUnknown) || IsCode(err, CodePermissionDenied) {
			t.Fatalf("lost or invented a code in error tree: %v", err)
		}
	}
	if errors.Is(limited, Fault{Code: CodeCapacityLimited, Message: "another failure"}) {
		t.Fatal("code matching changed matching against a specific fault")
	}
}
