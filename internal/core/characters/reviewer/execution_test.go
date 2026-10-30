package reviewer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"magi/internal/core/characters"
	reviewerrole "magi/internal/core/characters/reviewer"
	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
	"magi/internal/storage/mock"
)

func TestReviewInputEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input reviewerrole.Input
		empty bool
	}{
		{"empty", reviewerrole.Input{}, true},
		{"order", reviewerrole.Input{Orders: []characters.WorkOrder{{Instruction: "implement"}}}, false},
		{"memo", reviewerrole.Input{Memo: &characters.PlannerMemo{Body: "本回合没有可供审阅的素材，任务结束"}}, false},
		{"user input", reviewerrole.Input{UserInput: &characters.UserInput{Body: "New demand"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.input.Empty() != tc.empty {
				t.Fatal("idle classification differs from round input")
			}
		})
	}
}

func TestReviewerRetainsReviewHistoryAcrossRounds(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	ctx := context.Background()
	reviewer := &reviewerrole.Reviewer{}
	previousSize := 1 // The initial system message.
	for round := 1; round <= 2; round++ {
		previous := reviewer.Context
		input := reviewerrole.Input{
			WorkflowID:    "task",
			Orders:        []characters.WorkOrder{{OrderID: "P1", Instruction: "First order"}, {OrderID: "P2", Instruction: "Second order"}},
			WorkerReports: []characters.WorkerReport{{ReportID: "W1", OrderID: "P1", Body: "First report"}, {ReportID: "W2", OrderID: "P2", Body: "Second report"}},
		}
		if round == 1 {
			input.Memo = &characters.PlannerMemo{MemoID: "P3", Body: "Dispatch intent"}
			input.UserInput = &characters.UserInput{InputID: "P4", Body: "User clarification. [[U1]]"}
		}
		wantInput := []protocol.Message{input.Orders[0].Message(), input.Orders[1].Message()}
		if input.Memo != nil {
			wantInput = append(wantInput, input.Memo.Message())
		}
		if input.UserInput != nil {
			wantInput = append(wantInput, input.UserInput.Message())
		}
		wantInput = append(wantInput, input.WorkerReports[0].Message(), input.WorkerReports[1].Message())

		bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("Reviewer acquired an unexpected total deadline")
			}
			if request.Effort != protocol.EffortLow || request.LongOutput {
				t.Fatal("ordinary review must use low effort and the ordinary output tier")
			}
			if len(request.Messages) != previousSize+len(wantInput) || !reflect.DeepEqual(request.Messages[previousSize:], wantInput) {
				t.Fatal("review changed this round's inputs or lost prior history")
			}
			if round > 1 {
				prior := request.Messages[previousSize-2 : previousSize]
				if len(prior[0].ToolCalls) != 1 || prior[0].ToolCalls[0].Function.Name != "submit_review" || messageField(prior[0].ToolCalls[0].Function.Arguments, "summary") != "public judgment" || prior[1].ToolCallID != prior[0].ToolCalls[0].ID {
					t.Fatal("next review lost the preceding review")
				}
			}
			return controlResponse("submit_review", reviewDraft{Summary: "public judgment"}), nil
		}))
		report, err := reviewer.Review(ctx, input)
		noError(t, err)
		if previous != nil && reviewer.Context != previous {
			t.Fatal("review forked its continuing Context")
		}
		if report.Summary != "public judgment" || report.ReviewID == "" {
			t.Fatal("review did not return its numbered report", report)
		}
		previousSize += len(wantInput) + 2 // Submission and its receipt.
	}
}

func TestReviewerRejectsOtherProjectTools(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	protocol.Tools = queryingTools{}
	turns := 0
	model := modelFunc(func(_ context.Context, r characters.InvocationRequest) (protocol.Response, error) {
		var names []string
		for _, tool := range r.Tools {
			names = append(names, tool.Function.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"git_diff", "git_log", "git_status", "glob", "grep", "history_read", "read", "submit_review"}) {
			t.Fatalf("Reviewer capabilities: %v", names)
		}
		turns++
		if turns == 1 {
			return modelResponse(toolCall("query", "bash", `{"command":"true"}`)), nil
		}
		last := r.Messages[len(r.Messages)-1]
		if last.ToolCallID != "query" || !strings.Contains(last.Content, "tool is not allowed") {
			t.Fatalf("ungranted project tool was not rejected: %+v", last)
		}
		return controlResponse("submit_review", reviewDraft{Summary: "Reported behavior leaves a required effect unresolved."}), nil
	})
	bindModel(model)
	_, err := (&reviewerrole.Reviewer{}).Review(context.Background(), reviewerrole.Input{WorkflowID: "task", Orders: []characters.WorkOrder{{OrderID: "P1", Instruction: "Implement the order"}}})
	noError(t, err)
	if turns != 2 {
		t.Fatalf("review turns: %d", turns)
	}
}

func TestReviewerHandlesQueryOutcomes(t *testing.T) {
	failure := errors.New("tool infrastructure unavailable")
	for _, tc := range []struct {
		name    string
		result  protocol.ToolResult
		err     error
		receipt string
		fatal   bool
	}{
		{"settled failure", protocol.ToolResult{Status: protocol.ToolFailed, Body: []byte(`{"diagnostic":"known failure"}`)}, failure, `{"diagnostic":"known failure"}`, false},
		{"fatal failure", protocol.ToolResult{}, failure, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateBindings(t)
			roleFixtureFor(t)
			executed := 0
			protocol.Tools = queryingTools{query: func(_ context.Context, name string, args json.RawMessage) (protocol.ToolResult, error) {
				executed++
				if name != "read" || string(args) != `{"path":"target"}` {
					return protocol.ToolResult{}, errors.New("query changed across role boundary")
				}
				return tc.result, tc.err
			}}
			turns := 0
			bindModel(modelFunc(func(_ context.Context, r characters.InvocationRequest) (protocol.Response, error) {
				turns++
				if turns == 1 {
					return modelResponse(toolCall("query", "read", `{"path":"target"}`)), nil
				}
				last := r.Messages[len(r.Messages)-1]
				if tc.fatal || turns != 2 || last.Role != "tool" || last.ToolCallID != "query" || last.Content != tc.receipt {
					t.Fatalf("query outcome was lost or rewritten: %+v", last)
				}
				return controlResponse("submit_review", reviewDraft{Summary: "reviewed"}), nil
			}))
			reviewer := &reviewerrole.Reviewer{}
			_, err := reviewer.Review(context.Background(), reviewerrole.Input{WorkflowID: "task", Orders: []characters.WorkOrder{{OrderID: "P1", Instruction: "Implement the order"}}})
			if tc.fatal {
				if turns != 1 || reviewer.Context != nil || !errors.Is(err, failure) {
					t.Fatal("fatal query failure continued review", err)
				}
			} else {
				noError(t, err)
			}
			if executed != 1 {
				t.Fatalf("executions=%d", executed)
			}
		})
	}
}

type queryingTools struct {
	query func(context.Context, string, json.RawMessage) (protocol.ToolResult, error)
}

// The role consumes opaque outcomes; tools owns implementations.

func (queryingTools) PrepareRound(context.Context) error { panic("Reviewer cannot prepare a round") }

func (r queryingTools) Execute(ctx context.Context, request protocol.ToolRequest) (protocol.ToolResult, error) {
	return r.query(ctx, request.Name, request.Arguments)
}

func TestReviewerQueryBatchParallelAndOrdered(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	protocol.Observer = toolObservation{}
	ref, err := characters.CacheHistoryOutput(context.Background(), protocol.RoleWorker, "prior findings")
	noError(t, err)
	const count = 32
	queries := []string{"read", "glob", "grep", "git_status", "git_log", "git_diff"}
	var started atomic.Int32
	ready := make(chan struct{})
	lastDone := make(chan struct{})
	protocol.Tools = queryingTools{query: func(ctx context.Context, name string, args json.RawMessage) (protocol.ToolResult, error) {
		var arg struct{ Path, Ref string }
		if err := json.Unmarshal(args, &arg); err != nil {
			return protocol.ToolResult{}, err
		}
		identity := protocol.DiagnosticIdentityFromContext(ctx)
		if arg.Path == "" {
			arg.Path = arg.Ref
		}
		if name == "git_status" {
			arg.Path = identity.ToolCallID
		}
		if identity.ToolCallID != arg.Path || identity.InvocationID == "" {
			t.Errorf("tool diagnostic lost model call association: %+v", identity)
		}
		if started.Add(1) == count {
			close(ready)
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return protocol.ToolResult{}, ctx.Err()
		}
		if arg.Path == "31" {
			close(lastDone)
		} else {
			select {
			case <-lastDone:
			case <-ctx.Done():
				return protocol.ToolResult{}, ctx.Err()
			}
		}
		return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte(name + "-" + arg.Path)}, nil
	}}
	turns := 0
	bindModel(modelFunc(func(_ context.Context, r characters.InvocationRequest) (protocol.Response, error) {
		turns++
		if turns == 1 {
			calls := []protocol.ToolCall{
				toolCall("history", "history_read", `{"ref":"`+ref+`"}`),
				toolCall("missing", "history_read", `{"ref":"W999"}`),
			}
			for i := range count {
				name := queries[i%len(queries)]
				args := fmt.Sprintf(`{"path":"%d"}`, i)
				if name == "grep" || name == "glob" {
					args = fmt.Sprintf(`{"path":"%d","pattern":"x"}`, i)
				}
				if name == "git_status" {
					args = `{}`
				}
				if name == "git_log" || name == "git_diff" {
					args = fmt.Sprintf(`{"ref":"%d"}`, i)
				}
				calls = append(calls, toolCall(fmt.Sprint(i), name, args))
			}
			return modelResponse(calls...), nil
		}
		messages := r.Messages[len(r.Messages)-count-2:]
		if messages[0].ToolCallID != "history" || messages[0].Content != "prior findings" || messages[1].ToolCallID != "missing" || !strings.Contains(messages[1].Content, "source_unavailable") {
			t.Fatalf("history outcomes lost: %+v", messages[:2])
		}
		for i, message := range messages[2:] {
			if message.ToolCallID != fmt.Sprint(i) || !strings.Contains(message.Content, queries[i%len(queries)]+"-"+fmt.Sprint(i)) {
				t.Fatalf("read result out of order: %+v", message)
			}
		}
		return controlResponse("submit_review", reviewDraft{Summary: "Read batch complete."}), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = (&reviewerrole.Reviewer{}).Review(ctx, reviewerrole.Input{WorkflowID: "task", Orders: []characters.WorkOrder{{OrderID: "P1", Instruction: "Implement the order"}}})
	noError(t, err)
	if turns != 2 || started.Load() != count {
		t.Fatalf("turns=%d started=%d", turns, started.Load())
	}
}

func TestReviewerCancellationJoinsReads(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var started, finished atomic.Int32
	protocol.Tools = queryingTools{query: func(ctx context.Context, _ string, _ json.RawMessage) (protocol.ToolResult, error) {
		defer finished.Add(1)
		if started.Add(1) == 2 {
			cancel()
		}
		<-ctx.Done()
		return protocol.ToolResult{}, ctx.Err()
	}}
	turns := 0
	bindModel(modelFunc(func(context.Context, characters.InvocationRequest) (protocol.Response, error) {
		turns++
		return modelResponse(toolCall("a", "read", `{"path":"a"}`), toolCall("b", "read", `{"path":"b"}`)), nil
	}))
	r := &reviewerrole.Reviewer{}
	_, err := r.Review(ctx, reviewerrole.Input{WorkflowID: "task", Orders: []characters.WorkOrder{{OrderID: "P1", Instruction: "Implement the order"}}})
	if !errors.Is(err, context.Canceled) || started.Load() != 2 || finished.Load() != 2 || turns != 1 || r.Context != nil {
		t.Fatalf("review returned before cancellation settled: started=%d finished=%d turns=%d err=%v", started.Load(), finished.Load(), turns, err)
	}
}

func TestReviewerCompactsBeforeAssemblingInput(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	ctx := context.Background()
	window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 100, HardTokenLimit: 1000, HeadReservedMessages: 1}, true)
	noError(t, err)
	noError(t, window.Append(ctx, protocol.Message{Role: "system", Content: "fixed prefix"}))
	noError(t, window.Append(ctx, protocol.Message{Role: "user", Content: strings.Repeat("old source detail ", 100)}))
	calls := 0
	model := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		calls++
		if !corecontext.IsCompacting(ctx) || request.Role != protocol.RoleReviewer || request.WorkflowID != "task" {
			t.Fatalf("review skipped proactive compaction or lost audit metadata: %+v", request)
		}
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "new round report") {
				t.Fatal("new review material was appended before proactive compaction")
			}
		}
		return protocol.Response{}, protocol.ErrModelUnavailable
	})
	bindModel(model)
	reviewer := &reviewerrole.Reviewer{Context: window}
	_, err = reviewer.Review(ctx, reviewerrole.Input{WorkflowID: "task", Orders: []characters.WorkOrder{{OrderID: "P1", Instruction: "Implement the order"}}, WorkerReports: []characters.WorkerReport{{Body: "new round report"}}})
	if !errors.Is(err, protocol.ErrModelUnavailable) || calls != 1 {
		t.Fatalf("review bypassed compaction failure: calls=%d err=%v", calls, err)
	}
}

func TestEmptyReviewArchivesWithoutModelOrContextChanges(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			isolateBindings(t)
			roleFixtureFor(t)
			bindModel(modelFunc(func(context.Context, characters.InvocationRequest) (protocol.Response, error) {
				t.Fatal("empty review called the model")
				return protocol.Response{}, protocol.ErrModelUnavailable
			}))
			r := &reviewerrole.Reviewer{}
			var before []byte
			if retained {
				window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 100, HardTokenLimit: 10000, HeadReservedMessages: 1}, true)
				noError(t, err)
				noError(t, window.Append(context.Background(), protocol.Message{Role: "system", Content: "fixed head"}))
				noError(t, window.Append(context.Background(), protocol.Message{Role: "user", Content: strings.Repeat("old facts ", 100)}))
				r.Context = window
				before, err = window.Save()
				noError(t, err)
			}
			report, err := r.Review(context.Background(), reviewerrole.Input{WorkflowID: "task"})
			noError(t, err)
			if report.ReviewID != "R1" || report.Summary != "本回合没有可供审阅的素材，任务结束" {
				t.Fatal("empty review lost its fixed report", report)
			}
			body, err := readHistory(report.ReviewID)
			noError(t, err)
			if string(body) != report.Summary {
				t.Fatal("fixed report was not archived")
			}
			if retained {
				after, err := r.Context.Save()
				noError(t, err)
				if string(after) != string(before) {
					t.Fatal("empty review changed existing context")
				}
			} else if r.Context != nil {
				t.Fatal("empty review initialized a model context")
			}
		})
	}
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

func roleFixtureFor(t *testing.T) {
	t.Helper()
	protocol.Storage = &mock.Storage{}
	characters.ResetHistory()

}

// controlResponse constructs a native control response for a typed fixture.
func controlResponse(name string, value any) protocol.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	reasoning := "native offline fixture"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "fixture-" + name, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: string(body)}}}}, FinishReason: "tool_calls"}
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

func bindModel(model protocol.Model) {
	protocol.ModelPro = characters.ContextModel(model)
	protocol.ModelFlash = nil
}

// Non-JSON instructions and source text have no engine fields.
func messageField(content, name string) string {
	var fields map[string]any
	if json.Unmarshal([]byte(content), &fields) != nil {
		return ""
	}
	value, _ := fields[name].(string)
	return value
}

// Role fixtures propagate supplied failures without provider policy or retry waits.

type reviewDraft struct {
	Summary string `json:"summary"`
}

type toolObservation struct{}

func (toolObservation) NotifyReplyRequired() {}

func (toolObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (toolObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (toolObservation) DumpContext(context.Context, string, []byte) {}

func (toolObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}

// Read through the role-facing boundary; cache representation stays private.
func readHistory(ref string) ([]byte, error) {
	return characters.ReadHistory(context.Background(), []byte(`{"ref":"`+ref+`"}`))
}
