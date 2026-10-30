package characters

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"magi/internal/core/protocol"
)

func validInvocationRequest(t *testing.T) InvocationRequest {
	t.Helper()

	return InvocationRequest{WorkflowID: "t", Role: protocol.RoleReviewer, SessionID: "s", Purpose: "review", CycleKey: "c", PromptVersion: "v1", Request: protocol.Request{Messages: []protocol.Message{{Role: "system", Content: "rules"}}, Tools: []protocol.Tool{}}}
}

func TestResponseValidationFollowsOfferedTools(t *testing.T) {
	empty := ""
	text := protocol.Response{Message: protocol.Message{Role: "assistant", Content: "summary", ReasoningContent: &empty}, FinishReason: "stop"}
	if _, err := validateBatch(nil, text); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body, finish string
		calls        []protocol.ToolCall
	}{
		{" ", "stop", nil}, {"partial", "length", nil},
		{"", "tool_calls", []protocol.ToolCall{{ID: "report", Type: "function", Function: protocol.FunctionCall{Name: "submit_review", Arguments: `{"summary":"report"}`}}}},
	} {
		response := text
		response.Message.Content, response.FinishReason, response.Message.ToolCalls = tc.body, tc.finish, tc.calls
		if _, err := validateBatch(nil, response); err == nil {
			t.Fatal("invalid text summary accepted", tc)
		}
	}
	tools, err := ModelTools("submit_review", "history_read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateBatch(tools, text); err == nil {
		t.Fatal("plain text accepted when a tool call is required")
	}
}

func TestReceiptsPreserveOrderAndRequireCompleteSettlement(t *testing.T) {
	calls := []protocol.ToolCall{
		{ID: "first", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}},
		{ID: "second", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P2"}`}},
	}
	reasoning := ""
	record := &Invocation{message: &protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: calls}}
	save := func(index int, body []byte) error {
		return record.SaveReceipt(protocol.ValidatedToolCall{Index: index, Call: calls[index]}, body, nil)
	}
	second := []byte("second result")
	if err := save(1, second); err != nil {
		t.Fatal(err)
	}
	second[0] = '!'
	if _, err := record.pendingMessages(); !protocol.IsCode(err, protocol.CodeOutcomeUnknown) {
		t.Fatal("missing earlier receipt accepted", err)
	}
	if err := save(1, []byte("replacement")); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("duplicate receipt replaced its result", err)
	}
	if err := save(0, []byte("first result")); err != nil {
		t.Fatal(err)
	}
	messages, err := record.pendingMessages()
	if err != nil || len(messages) != 3 || messages[1].ToolCallID != "first" || messages[2].Content != "second result" {
		t.Fatal("receipt order or ownership changed", messages, err)
	}
}

// Protocol owns error-tree classification; check receipt settlement at this boundary.
func TestReceiptConvertsRefusalsWithoutLosingJoinedFailures(t *testing.T) {
	refusal := protocol.ToolCallError{Code: protocol.CodeInvalidCommand, Detail: "ARGUMENT_INVALID", Message: "correct the argument"}
	candidate := protocol.InvalidCandidate("correct the argument")
	fatal := protocol.Fault{Code: protocol.CodeOutcomeUnknown, Message: "effects unknown"}
	for _, tc := range []struct {
		err  error
		save bool
	}{
		{candidate, true},
		{errors.Join(candidate, fatal), false},
	} {
		call := protocol.ValidatedToolCall{Call: protocol.ToolCall{ID: "read"}}
		record := &Invocation{message: &protocol.Message{ToolCalls: []protocol.ToolCall{call.Call}}}
		err := record.SaveReceipt(call, nil, tc.err)
		if !tc.save {
			if err != tc.err || len(record.receipts) != 0 {
				t.Fatalf("joined failure was swallowed or recorded: %v", err)
			}
		} else if err != nil || len(record.receipts) != 1 || record.receipts[0].Body != string(refusal.Receipt()) {
			t.Fatalf("pure refusal lost its receipt: %v", err)
		}
	}
}

func TestToolOutputClassifiesOutcomes(t *testing.T) {
	failure := errors.New("tool infrastructure unavailable")
	refusal := protocol.WithCandidateFeedback(protocol.InvalidCommand("rejected"), "choose another target")
	body := []byte("exact tool output")
	for _, status := range []protocol.ToolCallStatus{protocol.ToolSucceeded, protocol.ToolFailed, protocol.ToolTimedOut, protocol.ToolInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			out, err := ToolOutput(protocol.ToolResult{Status: status, Body: body}, failure)
			if err != nil || string(out) != string(body) {
				t.Fatal("settled output lost to provider error", string(out), err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		status protocol.ToolCallStatus
		err    error
		code   protocol.ErrorCode
	}{
		{"missing", "", nil, protocol.CodeIntegrityFailure},
		{"invalid", "INVALID", nil, protocol.CodeIntegrityFailure},
		{"infrastructure failure", "", failure, ""},
		{"repairable refusal", "", refusal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ToolOutput(protocol.ToolResult{Status: tc.status, Body: body}, tc.err)
			if out != nil || err == nil {
				t.Fatal("unsettled output admitted", string(out), err)
			}
			if tc.code != "" {
				if !protocol.IsCode(err, tc.code) {
					t.Fatal("wrong outcome classification", err)
				}
			} else if err != tc.err {
				t.Fatal("unsettled tool error changed before receipt handling", err)
			}
		})
	}
}

func TestHandoffMessageFields(t *testing.T) {
	text := "Exact \"source\" wording.\nNext line 汉🙂"
	for _, tc := range []struct {
		message protocol.Message
		role    string
		fields  map[string]any
	}{
		{(WorkOrder{OrderID: "P1", WorkflowID: "task", Instruction: text}).Message(), "user", map[string]any{"order_id": "P1", "instruction": text}},
		{(WorkflowEntry{Mode: "COLD_START"}).Message(), "system", map[string]any{"workflow_entry": "COLD_START"}},
		{(PlannerMemo{MemoID: "P1", Body: text}).Message(), "user", map[string]any{"ref": "P1", "memo": text}},
		{(UserInput{InputID: "P2", Body: text}).Message(), "user", map[string]any{"ref": "P2", "user_input": text}},
		{(WorkerReport{ReportID: "W1", OrderID: "P1", Body: text}).Message(), "user", map[string]any{"ref": "W1", "order_id": "P1", "report": text}},
		{(ReviewReport{ReviewID: "R1", Summary: text}).Message(), "user", map[string]any{"ref": "R1", "summary": text}},
	} {
		message := tc.message
		var fields map[string]any
		if err := json.Unmarshal([]byte(message.Content), &fields); err != nil {
			t.Fatal(err)
		}
		if message.Role != tc.role || !reflect.DeepEqual(fields, tc.fields) {
			t.Fatalf("%s lost role, metadata or source text: %+v", tc.role, message)
		}
	}
}
