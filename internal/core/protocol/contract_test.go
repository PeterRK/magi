package protocol

import (
	"encoding/json"
	"testing"
)

func assistantMessage(calls ...ToolCall) Message {
	reasoning := "私人推理\r\n原文"
	return Message{Role: "assistant", ReasoningContent: &reasoning, Content: "正文", ToolCalls: calls}
}

func toolCall(id string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "history_read", Arguments: "{ \"ref\": \"原文\" }"}}
}

func TestMessageGroups(t *testing.T) {
	first := assistantMessage(toolCall("same"), toolCall("second"))
	chain := []Message{{Role: "system", Content: "rules"}, first, {Role: "tool", ToolCallID: "second", Content: "receipt2"}, {Role: "tool", ToolCallID: "same", Content: "receipt1"}, assistantMessage(toolCall("same")), {Role: "tool", ToolCallID: "same", Content: "receipt3"}}
	// Thinking and non-thinking assistant messages may share a history.
	chain[4].ReasoningContent = nil
	if err := ValidateMessages(chain); err != nil {
		t.Fatal(err)
	}
	for name, messages := range map[string][]Message{
		"unsettled":         chain[:3],
		"unknown role":      {{Role: "unknown", Content: "x"}},
		"wrong receipt":     {first, {Role: "tool", ToolCallID: "other", Content: "x"}},
		"duplicate receipt": {first, chain[2], chain[2]},
		"orphan":            {{Role: "tool", ToolCallID: "missing", Content: "x"}},
		"duplicate ID":      {assistantMessage(toolCall("x"), toolCall("x"))},
		"interleaved user":  {first, {Role: "user", Content: "x"}},
		"public reasoning":  {{Role: "user", Content: "x", ReasoningContent: first.ReasoningContent}},
		"invalid UTF8":      {{Role: "user", Content: string([]byte{0xff})}},
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateMessages(messages) == nil {
				t.Fatal("accepted malformed chain")
			}
		})
	}
	body, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Message
	if err = json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if *decoded.ReasoningContent != *first.ReasoningContent || decoded.ToolCalls[0].Function.Arguments != first.ToolCalls[0].Function.Arguments {
		t.Fatal("native material changed")
	}
}

func TestResponseValidation(t *testing.T) {
	for name, change := range map[string]func(*Response){
		"raw arguments": nil,
		"non-thinking":  func(r *Response) { r.Message.ReasoningContent = nil },
		"wrong role": func(r *Response) {
			r.Message.Role = "user"
			r.Message.ReasoningContent = nil
			r.Message.ToolCalls = nil
		},
		"receipt field":    func(r *Response) { r.Message.ToolCallID = "other" },
		"call type":        func(r *Response) { r.Message.ToolCalls[0].Type = "unknown" },
		"missing call ID":  func(r *Response) { r.Message.ToolCalls[0].ID = "" },
		"missing function": func(r *Response) { r.Message.ToolCalls[0].Function.Name = "" },
		"invalid usage":    func(r *Response) { r.Usage.InputTokens = -1 },
		"finish":           func(r *Response) { r.FinishReason = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			call := toolCall("a")
			call.Function.Arguments = "{invalid arguments}"
			response := Response{Message: assistantMessage(call), Usage: Usage{InputTokens: 1, OutputTokens: 2}, FinishReason: "tool_calls"}
			if change != nil {
				change(&response)
			}
			if err := ValidateResponse(response); (err == nil) != (change == nil || name == "non-thinking") {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if response.Message.ToolCalls != nil && response.Message.ToolCalls[0].Function.Arguments != "{invalid arguments}" {
				t.Fatal("validation rewrote raw arguments")
			}
		})
	}
}

func TestRequestToolDefinitions(t *testing.T) {
	for name, change := range map[string]func(*Request){
		"valid":                 nil,
		"null tools":            func(r *Request) { r.Tools = nil },
		"type":                  func(r *Request) { r.Tools[0].Type = "unknown" },
		"name":                  func(r *Request) { r.Tools[0].Function.Name = "" },
		"duplicate":             func(r *Request) { r.Tools = append(r.Tools, r.Tools[0]) },
		"parameters not object": func(r *Request) { r.Tools[0].Function.Parameters = json.RawMessage(`[]`) },
		"ambiguous parameters": func(r *Request) {
			r.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","type":"string"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := Request{Messages: []Message{{Role: "system", Content: "rules"}}, Tools: []Tool{{Type: "function", Function: Function{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}}
			if change != nil {
				change(&r)
			}
			if err := ValidateRequest(r); (err == nil) != (change == nil) {
				t.Fatal("unexpected request validation", err)
			}
		})
	}
}

func TestTokenDetailsValidationAndRoundTrip(t *testing.T) {
	ptr := func(n int) *int { return &n }
	thought := ""
	for _, tc := range []struct {
		name    string
		details *TokenDetails
		valid   bool
	}{
		{"historical", nil, true},
		{"zero_hit", &TokenDetails{CacheHitTokens: ptr(0), CacheMissTokens: ptr(100), ReasoningTokens: ptr(12)}, true},
		{"partial", &TokenDetails{CacheHitTokens: ptr(30)}, true},
		{"negative", &TokenDetails{CacheHitTokens: ptr(-1)}, false},
		{"too_many_cache", &TokenDetails{CacheMissTokens: ptr(101)}, false},
		{"inconsistent_cache", &TokenDetails{CacheHitTokens: ptr(20), CacheMissTokens: ptr(70)}, false},
		{"too_much_reasoning", &TokenDetails{ReasoningTokens: ptr(21)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := Response{Message: Message{Role: "assistant", Content: "ok", ReasoningContent: &thought}, Usage: Usage{InputTokens: 100, OutputTokens: 20}, FinishReason: "stop", TokenDetails: tc.details}
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateResponse(response); (err == nil) != tc.valid {
				t.Fatalf("envelope valid=%v err=%v", tc.valid, err)
			}
			if tc.valid {
				var decoded Response
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatal(err)
				}
				roundtrip, _ := json.Marshal(decoded)
				if string(body) != string(roundtrip) || decoded.Usage.Total() != 120 {
					t.Fatal("metadata changed accounting or nil/zero representation")
				}
			}
		})
	}
}
