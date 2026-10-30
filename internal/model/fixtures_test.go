package model

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"magi/internal/core/protocol"
)

func requestFixture() protocol.Request {
	reasoning := "Synthetic private reasoning."
	request := protocol.Request{
		Messages: []protocol.Message{
			{Role: "system", Content: "Synthetic reviewer"},
			{Role: "user", Content: "Known order reference: [[P1]]"},
			{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{
				ID: "call-1", Type: "function",
				Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`},
			}}},
			{Role: "tool", ToolCallID: "call-1", Content: "订单一：验证边界。\n保留源内容：{\"enabled\":true}\n"},
		},
		// Synthetic schemas exercise transport without copying role contracts.
		Tools: []protocol.Tool{
			{Type: "function", Function: protocol.Function{
				Name: "history_read", Description: "Read a test reference.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{"ref":{"type":"string","minLength":1}},
					"required":["ref"],"additionalProperties":false
				}`),
			}},
			{Type: "function", Function: protocol.Function{
				Name: "submit_review", Description: "Submit a test result.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{"result":{"type":"string","enum":["ok","retry"]}},
					"required":["result"],"additionalProperties":false
				}`),
			}},
		},
	}
	return request
}

func event(data string) string { return "data: " + data + "\n\n" }

const toolCallStart = `{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"检查。","tool_calls":[{"index":0,"id":"call-next","type":"function","function":{"name":"history_read","arguments":"{\"ref\":"}}]}}]}`
const toolCallFinish = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"P1\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"completion_tokens_details":{"reasoning_tokens":12}}}`

func readStream(reader io.Reader) (protocol.Response, error) {
	return decodeStream(reader, nil)
}

func observeAttempts(t testing.TB, observation *attemptObservation) {
	previous := protocol.Observer
	t.Cleanup(func() { protocol.Observer = previous })
	protocol.Observer = observationFuncs{
		started: func(context.Context, string, []byte) error { return observation.pause },
		finished: func(_ context.Context, data *protocol.RequestDiagnostic, response *protocol.Response, err error) {
			observation.RequestDiagnostic = *data
			observation.completed++
			observation.result, observation.err = *response, err
		},
	}
}
