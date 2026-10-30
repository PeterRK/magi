package worker_test

import (
	"context"
	"encoding/json"
	"testing"

	"magi/internal/core/characters"
	"magi/internal/core/protocol"
	"magi/internal/storage/mock"
)

type workerTool struct{}

func (f toolFunc) Execute(ctx context.Context, input protocol.ToolRequest) (protocol.ToolResult, error) {
	return f(ctx, input)
}

func order(instruction string) characters.WorkOrder {
	return characters.WorkOrder{WorkflowID: "task", Instruction: instruction}
}

func configureWorker(model protocol.Model, providers ...toolFunc) {
	var provider toolFunc = workerTool{}.Execute
	if len(providers) > 0 {
		provider = providers[0]
	}
	bindWorker(model, testTools{provider})
}

type toolFunc func(context.Context, protocol.ToolRequest) (protocol.ToolResult, error)

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

// controlResponse constructs a native control response for a typed fixture.
func controlResponse(name string, value any) protocol.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	reasoning := "native offline fixture"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "fixture-" + name, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: string(body)}}}}, FinishReason: "tool_calls"}
}

func mustWorker(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (workerTool) Execute(_ context.Context, input protocol.ToolRequest) (protocol.ToolResult, error) {
	return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte("tool-output:" + string(input.Arguments))}, nil
}

// Role execution needs protocol bindings, not an initialized workspace.
func resetWorkerHistory() {
	protocol.Storage = &mock.Storage{}
	characters.ResetHistory()
}
func bindWorker(model protocol.Model, executor protocol.ToolExecutor) {
	protocol.ModelPro = nil
	protocol.ModelFlash = characters.ContextModel(model)
	protocol.Tools = executor
}
func cacheOrders(t *testing.T, orders ...*characters.WorkOrder) {
	t.Helper()
	for _, order := range orders {
		ref, err := characters.CacheHistoryOutput(context.Background(), protocol.RolePlanner, order.Instruction)
		mustWorker(t, err)
		order.OrderID = ref
	}
}

// Global protocol bindings model the single active workspace. These tests must
// stay serial, and any goroutines must finish before test cleanup restores them.
func isolateBindings(t *testing.T) {
	t.Helper()
	pro, flash := protocol.ModelPro, protocol.ModelFlash
	storage, tools := protocol.Storage, protocol.Tools
	userMessages, showUser, observer := protocol.UserMessages, protocol.ShowUser, protocol.Observer
	characters.ResetHistory()
	t.Cleanup(func() {
		protocol.ModelPro, protocol.ModelFlash = pro, flash
		protocol.Storage, protocol.Tools = storage, tools
		protocol.UserMessages, protocol.ShowUser, protocol.Observer = userMessages, showUser, observer
		characters.ResetHistory()
	})
}

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

// Role tests exercise scheduling and forwarding; tools tests own execution details.
type testTools struct{ toolFunc }

func (testTools) PrepareRound(context.Context) error { panic("Worker cannot prepare a round") }

type workerJobFinish struct {
	Report string `json:"report"`
}
