package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/engine"
	"magi/internal/core/protocol"
	"magi/internal/storage"
)

func openTestProject(t *testing.T, ctx context.Context, root string) *workspaceState {
	t.Helper()
	disk, err := storage.InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := engine.Open(ctx, disk.Disk(), disk.Metadata().ProjectID)
	if err != nil {
		_ = disk.Close()
		t.Fatal(err)
	}
	return &workspaceState{Project: disk, state: journal}
}

func workflowModel(t *testing.T) protocol.Model {
	t.Helper()

	model := newScriptedModel(map[string][]protocol.Response{
		"PLANNER/planner": {
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
			controlCallResponse("submit_plan", `{"orders":[{"instruction":"Implement the reviewed user request."}],"memo":"Dispatch intent: fulfill the assigned outcomes."}`),
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
		},
		"PLANNER/interaction": {
			controlResponse("show_user", showUserArgs{Message: "What would you like me to do?", WaitForReply: true}),
			controlResponse("show_user", showUserArgs{Message: "Request received."}),

			controlResponse("show_user", showUserArgs{Message: "Implementing the requested work."}),

			controlResponse("show_user", showUserArgs{Message: "The reviewed work is ready. Preparing to sleep."}),
		},

		"PLANNER/interaction_summary": {textResponse("User requested the task described in their reply. [[U1]]")},
		"WORKER/worker": {
			controlCallResponse("write", `{"path":"work.txt","content":"done\n"}`),
			controlResponse("finish_job", workerJobFinish{Report: "implementation complete"}),
		},
		"REVIEWER/review": {
			controlResponse("submit_review", reviewDraft{Summary: "Review user request"}),
			controlResponse("submit_review", reviewDraft{Summary: "Implementation reviewed"}),
		},
	})
	return model
}

func textResponse(body string) protocol.Response {
	reasoning := ""
	return protocol.Response{Message: protocol.Message{Role: "assistant", Content: body, ReasoningContent: &reasoning}, FinishReason: "stop", Usage: protocol.Usage{InputTokens: 100, OutputTokens: 20}}
}

func TestMain(m *testing.M) { _ = os.Unsetenv("DEEPSEEK_API_KEY"); os.Exit(m.Run()) }

type entryObserver struct {
	protocol.Model
	first []protocol.Message
}

func (m *entryObserver) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	if m.first == nil && characters.CurrentInvocation(ctx).Owner().Role == protocol.RolePlanner {
		m.first = protocol.CloneMessages(input.Messages)
	}
	return m.Model.Invoke(ctx, input)
}

func controlCallResponse(name, args string) protocol.Response {
	reasoning := "native offline fixture"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "fixture-" + name, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: args}}}}, FinishReason: "tool_calls"}
}

// controlResponse constructs a native control response for a typed fixture.
func controlResponse(name string, value any) protocol.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return controlCallResponse(name, string(body))
}

// noError reports setup failures at the calling test, preserving Fatal semantics.
func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
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

// Script lanes name ROLE/purpose. CLI tests do not simulate compaction.
type scriptedModel struct {
	mu    sync.Mutex
	lanes map[string][]protocol.Response
}

func newScriptedModel(script map[string][]protocol.Response) *scriptedModel {
	lanes := make(map[string][]protocol.Response, len(script))
	for key, steps := range script {
		lanes[key] = make([]protocol.Response, len(steps))
		for i, response := range steps {
			response.Message = protocol.CloneMessage(response.Message)
			lanes[key][i] = response
		}
	}
	return &scriptedModel{lanes: lanes}
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
		return protocol.Response{}, fmt.Errorf("%w: scripted context compaction is disabled", protocol.ErrModelUnavailable)
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

func (*scriptedModel) InputCapacity() int { return 933440 }

type reviewDraft struct {
	Summary string `json:"summary"`
}

type workerJobFinish struct {
	Report string `json:"report"`
}

type directModel func(context.Context, protocol.Request) (protocol.Response, error)

func (f directModel) Invoke(ctx context.Context, r protocol.Request) (protocol.Response, error) {
	return f(ctx, r)
}
func (directModel) InputCapacity() int { return 23552 }

// showUserArgs models the public show_user tool parameters.
type showUserArgs struct {
	Message      string `json:"message"`
	WaitForReply bool   `json:"wait_for_reply"`
}
