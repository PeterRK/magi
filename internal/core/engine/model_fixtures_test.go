package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
)

// Each integration fixture owns its role/purpose queues. Compaction is answered
// independently and does not consume scripted role submissions.
type scriptedModel struct {
	mu    sync.Mutex
	lanes map[string][]protocol.Response
}

func (m *scriptedModel) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	request := characters.CurrentInvocation(ctx).Owner()
	request.Request = input
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if corecontext.IsCompacting(ctx) {
		return textResponse("Earlier completed work and source constraints retained."), nil
	}
	key := string(request.Role) + "/" + request.Purpose
	steps := m.lanes[key]
	if len(steps) == 0 {
		return protocol.Response{}, fmt.Errorf("%w: native script exhausted for %s", protocol.ErrModelUnavailable, key)
	}
	response := steps[0]
	m.lanes[key] = steps[1:]
	return response, nil
}

func (*scriptedModel) InputCapacity() int { return 23552 }

// Non-JSON instructions and source text have no engine fields.
func messageField(content, name string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &fields) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(fields[name], &value) == nil {
		return value
	}
	return string(fields[name])
}

type modelFunc func(context.Context, characters.InvocationRequest) (protocol.Response, error)

func (f modelFunc) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	r := characters.CurrentInvocation(ctx).Owner()
	r.Request = input
	return f(ctx, r)
}

func modelResponse(calls ...protocol.ToolCall) protocol.Response {
	reasoning := "private chain\r\n原文"
	return protocol.Response{Message: protocol.Message{Role: "assistant", Content: "assistant text", ReasoningContent: &reasoning, ToolCalls: calls}, Usage: protocol.Usage{InputTokens: 5, OutputTokens: 2}, FinishReason: "tool_calls"}
}

func toolCall(id, name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: args}}
}

func (modelFunc) InputCapacity() int { return 23552 }

func planResponse(value plannerPlanDraft) protocol.Response {
	if value.Orders == nil {
		value.Orders = []string{}
	}
	if len(value.Orders) != 0 && value.Memo == "" {
		value.Memo = "Dispatch intent: fulfill the assigned outcomes."
	}
	return controlResponse("submit_plan", value)
}

// controlResponse constructs a native control response for a typed fixture.
func controlResponse(name string, value any) protocol.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return controlCallResponse(name, string(body))
}

func textResponse(body string) protocol.Response {
	reasoning := ""
	return protocol.Response{Message: protocol.Message{Role: "assistant", Content: body, ReasoningContent: &reasoning}, FinishReason: "stop", Usage: protocol.Usage{InputTokens: 100, OutputTokens: 20}}
}

func reviewStep() protocol.Response {
	return controlResponse("submit_review", reviewDraft{Summary: "Review of the current demand."})
}

func showStep(message string, ask bool) protocol.Response {
	return controlResponse("show_user", showUserArgs{Message: message, WaitForReply: ask})
}

func controlCallResponse(name, args string) protocol.Response {
	reasoning := "native offline fixture"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "fixture-" + name, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: args}}}}, FinishReason: "tool_calls"}
}

// showUserArgs models the public show_user tool parameters.
type showUserArgs struct {
	Message      string `json:"message"`
	WaitForReply bool   `json:"wait_for_reply"`
}

type observedModel struct {
	protocol.Model
	mu          sync.Mutex
	requests    []characters.InvocationRequest
	compactions map[protocol.Role]int
}

func (m *observedModel) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	r := characters.CurrentInvocation(ctx).Owner()
	r.Request = input
	m.mu.Lock()
	if corecontext.IsCompacting(ctx) {
		if m.compactions == nil {
			m.compactions = map[protocol.Role]int{}
		}
		m.compactions[r.Role]++
	}
	frozen := r
	frozen.Request = protocol.CloneRequest(r.Request)
	m.requests = append(m.requests, frozen)
	m.mu.Unlock()
	return m.Model.Invoke(ctx, r.Request)
}
func (m *observedModel) snapshot() []characters.InvocationRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]characters.InvocationRequest(nil), m.requests...)
}
