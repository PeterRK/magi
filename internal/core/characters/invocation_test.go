package characters

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

type providerFunc func(context.Context, protocol.Request) (protocol.Response, error)

func (f providerFunc) Invoke(ctx context.Context, r protocol.Request) (protocol.Response, error) {
	return f(ctx, r)
}
func (providerFunc) InputCapacity() int { return 23552 }

func TestRoleContextPolicyResolvesAbsoluteLimits(t *testing.T) {
	config := RoleContextConfig(providerFunc(nil), 1)
	if config.SoftTokenLimit != 14132 || config.HardTokenLimit != 17664 {
		t.Fatal(config)
	}
}

type diagnosticObservation struct{}

func (diagnosticObservation) NotifyReplyRequired() {}

func (diagnosticObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (diagnosticObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (diagnosticObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
func (diagnosticObservation) DumpContext(context.Context, string, []byte)                   {}

func TestDiagnosticIdentityIsOptIn(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			isolateBindings(t)
			protocol.Observer = nil
			if mode == "on" {
				protocol.Observer = diagnosticObservation{}
			}
			protocol.Storage = usageStorage{}
			var modelCtx context.Context
			ctx, window, owner := invocationFixture(t, context.Background(), func(ctx context.Context, _ protocol.Request) (protocol.Response, error) {
				modelCtx = ctx
				return scopeResponse("call", `{"ref":"P1"}`), nil
			})
			if _, err := window.CallModel(ctx, owner.Request); err != nil {
				t.Fatal(err)
			}
			record := CurrentInvocation(ctx)
			want := protocol.DiagnosticIdentity{
				InvocationID: record.invocationID, WorkflowID: owner.WorkflowID,
				Role: owner.Role, SessionID: owner.SessionID, Purpose: owner.Purpose,
				CycleKey: owner.CycleKey, PromptVersion: owner.PromptVersion,
			}
			if want.InvocationID == "" {
				t.Fatal("missing submitting invocation")
			}
			for _, observation := range []struct {
				ctx     context.Context
				enabled bool
				callID  string
			}{{modelCtx, protocol.Observer != nil, ""}, {ToolContext(ctx, "first"), protocol.Observer != nil, "first"}, {ToolContext(ctx, "second"), protocol.Observer != nil, "second"}} {
				got := protocol.DiagnosticIdentityFromContext(observation.ctx)
				if observation.enabled {
					want.ToolCallID = observation.callID
					if got != want {
						t.Fatalf("identity differs from submitting call: %+v", got)
					}
				} else if observation.ctx != ctx || got != (protocol.DiagnosticIdentity{}) {
					t.Fatal("disabled observation added diagnostic context")
				}
			}
		})
	}
}

func TestCompactionUsesSelectedProviderAndOwner(t *testing.T) {
	isolateBindings(t)
	protocol.Storage = usageStorage{}
	for _, role := range []protocol.Role{protocol.RolePlanner, protocol.RoleReviewer, protocol.RoleWorker} {
		var seen InvocationRequest
		stop := errors.New("captured")
		backend := providerFunc(func(ctx context.Context, request protocol.Request) (protocol.Response, error) {
			seen = CurrentInvocation(ctx).Owner()
			seen.Request = request
			return protocol.Response{}, stop
		})
		protocol.ModelPro = ContextModel(backend)
		protocol.ModelFlash = protocol.ModelPro
		purpose := map[protocol.Role]string{protocol.RolePlanner: "planner", protocol.RoleReviewer: "review", protocol.RoleWorker: "worker"}[role]
		owner := InvocationRequest{
			WorkflowID: "task", Role: role, Purpose: purpose,
			SessionID: "session", CycleKey: "cycle", PromptVersion: prompt.Version,
		}
		ctx := InvocationContext(context.Background(), owner)
		window, err := corecontext.New(corecontext.CompactionConfig{HeadReservedMessages: 1, SoftTokenLimit: 1, HardTokenLimit: 1000}, role != protocol.RoleWorker)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []protocol.Message{{Role: "system", Content: "owner"}, {Role: "user", Content: "Known findings at [[W1]]."}} {
			if err := window.Append(ctx, m); err != nil {
				t.Fatal(err)
			}
		}
		if err := window.TryCompact(ctx); !errors.Is(err, stop) || seen.Role != role || seen.Purpose != purpose || len(seen.Tools) != 0 {
			t.Fatal("Full lost its provider or owner", seen, err)
		}
	}
}

type usageStorage struct{ protocol.StorageBackend }

func (usageStorage) ObserveUsage(string, protocol.TokenUsage) error { return nil }

type observedUsage struct {
	protocol.StorageBackend
	err   error
	count protocol.TokenUsage
	calls int
}

func (s *observedUsage) ObserveUsage(_ string, count protocol.TokenUsage) error {
	s.calls++
	s.count.Miss += count.Miss
	s.count.Hit += count.Hit
	s.count.Output += count.Output
	return s.err
}

func invocationFixture(t *testing.T, ctx context.Context, invoke providerFunc) (context.Context, *corecontext.Context, InvocationRequest) {
	t.Helper()

	owner := validInvocationRequest(t)
	var err error
	owner.Tools, err = ModelTools("history_read", "submit_review")
	if err != nil {
		t.Fatal(err)
	}
	ctx = InvocationContext(ctx, owner)
	previous := protocol.ModelPro
	protocol.ModelPro = ContextModel(invoke)
	t.Cleanup(func() { protocol.ModelPro = previous })
	window, err := corecontext.New(RoleContextConfig(invoke, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range owner.Messages {
		if err := window.Append(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, window, owner
}

func scopeResponse(id, args string) protocol.Response {
	reasoning := "private reasoning"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: id, Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: args}}}}, FinishReason: "tool_calls", Usage: protocol.Usage{InputTokens: 5, OutputTokens: 2}}
}

func TestInvocationUsageFailureStopsBeforeEffects(t *testing.T) {
	isolateBindings(t)
	auditErr := errors.New("usage unavailable")
	accounting := &observedUsage{err: auditErr}
	protocol.Storage = accounting
	calls := 0
	ctx, window, owner := invocationFixture(t, context.Background(), func(context.Context, protocol.Request) (protocol.Response, error) {
		calls++
		return scopeResponse("call", `{"ref":"P1"}`), nil
	})
	response, err := window.CallModel(ctx, owner.Request)
	if !errors.Is(err, auditErr) || calls != 1 || accounting.calls != 1 || accounting.count.Miss != 5 {
		t.Fatalf("calls=%d audit=%+v err=%v", calls, accounting, err)
	}
	if CurrentInvocation(ctx).message != nil {
		t.Fatal("unrecorded generation reached role state", response)
	}
}

func TestCanceledOutputCannotReplaceSettledContinuation(t *testing.T) {
	isolateBindings(t)
	accounting := &observedUsage{}
	protocol.Storage = accounting
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	ctx, window, owner := invocationFixture(t, root, func(context.Context, protocol.Request) (protocol.Response, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return scopeResponse("call", `{"ref":"P1"}`), nil
	})
	response, err := window.CallModel(ctx, owner.Request)
	if err != nil {
		t.Fatal(err)
	}
	record := CurrentInvocation(ctx)
	original := record.invocationID
	validated, err := record.Batch()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"text":"settled"}`)
	err = record.SaveReceipt(validated[0], body, nil)
	if err != nil {
		t.Fatal(err)
	}
	body[0] = '!'
	*response.Message.ReasoningContent = "provider mutation"
	if _, err := window.CallModel(ctx, owner.Request); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	pending, err := record.pendingMessages()
	if err != nil || len(pending) != 2 || pending[0].ReasoningContent == nil || *pending[0].ReasoningContent != "private reasoning" || pending[1].Content[0] != '{' {
		t.Fatal("settled continuation changed", pending, err)
	}
	if record.invocationID != original || accounting.calls != 1 || accounting.count.Output != 2 {
		t.Fatal("canceled output replaced role state or added usage", accounting)
	}
}

func TestInvocationProtocolErrorStreakIsLocal(t *testing.T) {
	isolateBindings(t)
	for _, reset := range []bool{false, true} {
		protocol.Storage = &observedUsage{}
		calls := 0
		ctx, window, owner := invocationFixture(t, context.Background(), func(context.Context, protocol.Request) (protocol.Response, error) {
			calls++
			args := `{}`
			if reset && calls == 3 {
				args = `{"ref":"P1"}`
			}
			return scopeResponse("call", args), nil
		})
		count := 5
		if reset {
			count = 8
		}
		for i := 0; i < count; i++ {
			if _, err := window.CallModel(ctx, owner.Request); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := window.CallModel(ctx, owner.Request); !protocol.IsCode(err, protocol.CodeCapacityLimited) || calls != count {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	}
}

func TestInvocationRebindPreservesErrorStateAndPendingReceipts(t *testing.T) {
	isolateBindings(t)
	accounting := &observedUsage{}
	protocol.Storage = accounting
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	ctx, window, owner := invocationFixture(t, parent, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		if calls == 2 && (len(input.Messages) != 3 || input.Messages[1].Role != "assistant" || input.Messages[2].ToolCallID != "read" || input.Messages[2].Content != `{"text":"settled"}`) {
			t.Fatal("resumed delivery lost or duplicated messages", input.Messages)
		}
		return scopeResponse("read", `{"ref":"P1"}`), nil
	})
	_, err := window.CallModel(ctx, owner.Request)
	if err != nil {
		t.Fatal(err)
	}
	record := CurrentInvocation(ctx)
	validated, err := record.Batch()
	if err != nil {
		t.Fatal(err)
	}
	if err := record.SaveReceipt(validated[0], []byte(`{"text":"settled"}`), nil); err != nil {
		t.Fatal(err)
	}
	// Model a cancellation between the assistant message and its tool receipt.
	if err := window.Append(ctx, *record.message); err != nil {
		t.Fatal(err)
	}
	record.appended++
	pending, err := record.pendingMessages()
	if err != nil || len(pending) != 1 {
		t.Fatal("missing pending receipt", pending, err)
	}
	record.errorStreak = 2
	cancel()
	resumed := record.Context(context.Background())
	if resumed.Err() != nil || !errors.Is(ctx.Err(), context.Canceled) || CurrentInvocation(resumed) != record || record.errorStreak != 2 {
		t.Fatal("rebind reset scope or retained the old parent cancellation")
	}
	remaining, err := CurrentInvocation(resumed).pendingMessages()
	if err != nil || !reflect.DeepEqual(remaining, pending) || accounting.calls != 1 {
		t.Fatal("rebind lost or recounted the pending receipt", remaining, err)
	}
	if record.owner.Messages != nil {
		t.Fatal("invocation retained input messages")
	}
	if err := record.AppendPending(resumed, window); err != nil {
		t.Fatal(err)
	}
	if record.message != nil || record.receipts != nil || record.batch != nil || record.appended != 0 || record.errorStreak != 2 {
		t.Fatal("delivery retained output or reset the invocation error state")
	}
	if _, err := window.CallModel(resumed, owner.Request); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || accounting.calls != 2 || accounting.count.Output != 4 || record.errorStreak != 0 {
		t.Fatal("rebind lost usage or valid response did not reset errors", calls, accounting)
	}
}

func TestFullPreservesPendingReceiptsAndOwnerUsage(t *testing.T) {
	for _, tc := range []struct {
		name, summary string
		wantFailure   bool
	}{{"accepted", "accepted summary", false}, {"rejected", " ", true}} {
		t.Run(tc.name, func(t *testing.T) {
			isolateBindings(t)
			accounting := &observedUsage{}
			protocol.Storage = accounting
			ctx, ordinary, owner := invocationFixture(t, context.Background(), func(ctx context.Context, request protocol.Request) (protocol.Response, error) {
				if !corecontext.IsCompacting(ctx) {
					return scopeResponse("pending", `{"ref":"P1"}`), nil
				}
				reasoning := ""
				return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, Content: tc.summary}, FinishReason: "stop", Usage: protocol.Usage{InputTokens: 5, OutputTokens: 2}}, nil
			})
			record := CurrentInvocation(ctx)
			if _, err := ordinary.CallModel(ctx, owner.Request); err != nil {
				t.Fatal(err)
			}
			validated, err := record.Batch()
			if err != nil {
				t.Fatal(err)
			}
			if err := record.SaveReceipt(validated[0], []byte(`{"text":"settled"}`), nil); err != nil {
				t.Fatal(err)
			}
			pending, err := record.pendingMessages()
			if err != nil {
				t.Fatal(err)
			}
			invocationID := record.invocationID
			window, err := corecontext.New(corecontext.CompactionConfig{HeadReservedMessages: 1, SoftTokenLimit: 600, HardTokenLimit: 1000}, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range []protocol.Message{{Role: "system", Content: "original head"}, {Role: "user", Content: strings.Repeat("x", 3000)}} {
				if err := window.Append(ctx, message); err != nil {
					t.Fatal(err)
				}
			}
			if err := window.TryCompact(ctx); (err != nil) != tc.wantFailure {
				t.Fatal("unexpected Full result", err)
			}
			batch, batchErr := record.Batch()
			remaining, err := record.pendingMessages()
			if err != nil || batchErr != nil || !reflect.DeepEqual(batch, validated) || record.invocationID != invocationID || !reflect.DeepEqual(remaining, pending) {
				t.Fatal("private Full replaced pending role response or receipts", err)
			}
			if accounting.calls != 2 || accounting.count.Output != 4 {
				t.Fatal("Full lost or duplicated owner usage", accounting)
			}
		})
	}
}

// Global protocol bindings model the single active workspace. These tests must
// stay serial, and any goroutines must finish before test cleanup restores them.
func isolateBindings(t *testing.T) {
	t.Helper()
	pro, flash := protocol.ModelPro, protocol.ModelFlash
	storage, tools := protocol.Storage, protocol.Tools
	userMessages, showUser, observer := protocol.UserMessages, protocol.ShowUser, protocol.Observer
	ResetHistory()
	t.Cleanup(func() {
		protocol.ModelPro, protocol.ModelFlash = pro, flash
		protocol.Storage, protocol.Tools = storage, tools
		protocol.UserMessages, protocol.ShowUser, protocol.Observer = userMessages, showUser, observer
		ResetHistory()
	})
}

func TestCallModelPreservesDeadlineAndFailure(t *testing.T) {
	isolateBindings(t)
	failure := protocol.ErrModelUnavailable
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	wantDeadline, _ := ctx.Deadline()
	var expected protocol.Request
	ctx, window, owner := invocationFixture(t, ctx, func(callCtx context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		if deadline, ok := callCtx.Deadline(); !ok || !deadline.Equal(wantDeadline) {
			t.Fatal("lost caller deadline")
		}
		if !reflect.DeepEqual(input, expected) {
			t.Fatal("provider input changed")
		}
		return protocol.Response{}, failure
	})
	expected = owner.Request
	_, err := window.CallModel(ctx, owner.Request)
	if !errors.Is(err, protocol.ErrModelUnavailable) || calls != 1 {
		t.Fatal("core retried or changed provider failure", calls, err)
	}
}
