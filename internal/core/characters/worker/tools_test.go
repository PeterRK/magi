package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
)

// Exercise outcomes through batch handling, including receipt validation and
// whether a later write may start. No separate lifecycle state is needed.
func TestBatchHandlesToolOutcomes(t *testing.T) {
	for _, mode := range []string{"success", "provider-failure", "late-cancel", "before-start", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			old := protocol.Tools
			t.Cleanup(func() { protocol.Tools = old })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executed := 0
			protocol.Tools = batchTools{execute: func(context.Context, protocol.ToolRequest) (protocol.ToolResult, error) {
				executed++
				result := protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte("known output")}
				if executed != 1 {
					return result, nil
				}
				switch mode {
				case "provider-failure":
					return protocol.ToolResult{Status: protocol.ToolFailed, Body: []byte("provider failed")}, errors.New("provider failed")
				case "late-cancel":
					cancel()
				case "rejected":
					return protocol.ToolResult{}, protocol.InvalidCandidate("invalid path")
				}
				return result, nil
			}}
			ctx, calls := batchInvocation(t, ctx, []string{"write", "write"})
			if mode == "before-start" {
				cancel()
			}
			err := executeBatch(ctx, calls)
			messages, pendingErr := settledMessages(ctx)
			wantCalls := 2
			if mode == "late-cancel" {
				wantCalls = 1
			} else if mode == "before-start" {
				wantCalls = 0
			}
			if !errors.Is(err, ctx.Err()) || pendingErr != nil || len(messages) != 3 || executed != wantCalls {
				t.Fatalf("batch lost outcomes: executed=%d messages=%+v err=%v pending=%v", executed, messages, err, pendingErr)
			}
			switch mode {
			case "rejected":
				if !strings.Contains(messages[1].Content, "invalid path") {
					t.Fatal("repairable refusal was lost", messages[1])
				}
			case "before-start":
				var receipt struct{ Status protocol.ToolCallStatus }
				if json.Unmarshal([]byte(messages[1].Content), &receipt) != nil || receipt.Status != protocol.ToolInterrupted {
					t.Fatal("unstarted call has no interruption receipt", messages)
				}
			default:
				want := "known output"
				if mode == "provider-failure" {
					want = "provider failed"
				}
				if messages[1].Content != want {
					t.Fatal("known outcome was changed", messages)
				}
			}
		})
	}
}

type batchTools struct {
	protocol.ToolExecutor
	execute func(context.Context, protocol.ToolRequest) (protocol.ToolResult, error)
}

func (b batchTools) Execute(ctx context.Context, req protocol.ToolRequest) (protocol.ToolResult, error) {
	return b.execute(ctx, req)
}

func batchInvocation(t *testing.T, ctx context.Context, names []string) (context.Context, []protocol.ValidatedToolCall) {
	t.Helper()
	definitions, err := characters.ModelTools("history_read", "finish_job")
	for _, name := range []string{"read", "write", "glob", "grep"} {
		definitions = append(definitions, protocol.Tool{Type: "function", Function: protocol.Function{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)}})
	}
	if err != nil {
		t.Fatal(err)
	}
	request := characters.InvocationRequest{WorkflowID: "task", Role: protocol.RoleWorker, Purpose: "worker", SessionID: "worker:test", CycleKey: "cycle", PromptVersion: "test", Request: protocol.Request{Tools: definitions}}
	var calls []protocol.ToolCall
	for i, name := range names {
		args := fmt.Sprintf(`{"index":%d}`, i)
		if name == "history_read" {
			args = `{"ref":"P1"}`
		}
		calls = append(calls, protocol.ToolCall{ID: strconv.Itoa(i), Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: args}})
	}
	reasoning := "test"
	response := protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: calls}, FinishReason: "tool_calls"}
	ctx = characters.InvocationContext(ctx, request)
	oldStorage := protocol.Storage
	protocol.Storage = batchUsage{oldStorage}
	defer func() { protocol.Storage = oldStorage }()
	_, err = characters.ContextModel(batchProviderFunc(func(context.Context, protocol.Request) (protocol.Response, error) { return response, nil })).Invoke(ctx, request.Request)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := characters.CurrentInvocation(ctx).Batch()
	if err != nil {
		t.Fatal(err)
	}
	return ctx, validated
}

type batchHistory struct {
	protocol.StorageBackend
	read func() []byte
}

func (h batchHistory) ReadFragment(context.Context, string) ([]byte, error) { return h.read(), nil }

func TestBatchReadGroupsAndOrderedReceipts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// History and project reads form one group; both writes wait for all seven.
		names := []string{"glob", "history_read", "grep", "read", "read", "read", "read", "write", "write", "read", "write", "read", "read"}
		started := make(chan int, len(names))
		release := make([]chan struct{}, len(names))
		for i := range release {
			release[i] = make(chan struct{})
		}
		value := "original"
		run := func(i int) string {
			started <- i
			<-release[i]
			if names[i] == "write" {
				value = strconv.Itoa(i)
			}
			return value
		}
		oldTools, oldStorage, oldObserver := protocol.Tools, protocol.Storage, protocol.Observer
		t.Cleanup(func() {
			protocol.Tools, protocol.Storage, protocol.Observer = oldTools, oldStorage, oldObserver
			characters.ResetHistory()
		})
		protocol.Observer = toolObservation{}
		characters.ResetHistory()
		protocol.Storage = batchHistory{read: func() []byte { return []byte(run(1)) }}
		protocol.Tools = batchTools{execute: func(ctx context.Context, req protocol.ToolRequest) (protocol.ToolResult, error) {
			var args struct{ Index int }
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return protocol.ToolResult{}, err
			}
			if args.Index == 0 {
				identity := protocol.DiagnosticIdentityFromContext(ctx)
				if identity.ToolCallID != strconv.Itoa(args.Index) || identity.InvocationID == "" {
					t.Errorf("tool diagnostic lost model call association: %+v", identity)
				}
			}
			return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte(run(args.Index))}, nil
		}}
		ctx, calls := batchInvocation(t, context.Background(), names)
		done := make(chan error, 1)
		go func() {
			err := executeBatch(ctx, calls)
			done <- err
		}()
		for _, group := range [][]int{{0, 1, 2, 3, 4, 5, 6}, {7}, {8}, {9}, {10}, {11, 12}} {
			synctest.Wait()
			var got []int
			for len(started) > 0 {
				got = append(got, <-started)
			}
			slices.Sort(got)
			if !slices.Equal(got, group) {
				t.Fatalf("started %v, want group %v", got, group)
			}
			// Deliberately complete in reverse order, checking the next unit
			// cannot start while even one sibling is unfinished.
			for j := len(group) - 1; j >= 0; j-- {
				if len(started) != 0 {
					t.Fatal("next unit started before read group joined")
				}
				close(release[group[j]])
				synctest.Wait()
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		messages, err := settledMessages(ctx)
		if err != nil || len(messages) != len(names)+1 {
			t.Fatal("incomplete continuation", messages, err)
		}
		for i, message := range messages[1:] {
			if message.ToolCallID != strconv.Itoa(i) {
				t.Fatalf("receipt %d reordered: %+v", i, message)
			}
			want := "original"
			if i >= 10 {
				want = "10"
			} else if i >= 8 {
				want = "8"
			} else if i == 7 {
				want = "7"
			}
			got := message.Content
			if got != want {
				t.Fatalf("call %d saw %q, want %q", i, got, want)
			}
		}
	})
}

func TestBatchRetainsKnownReceiptsOnInterruption(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint("execution-failure=", fatal), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				old := protocol.Tools
				t.Cleanup(func() { protocol.Tools = old })
				started := make(chan struct{}, 3)
				finish := make(chan struct{})
				failure := errors.New("execution infrastructure failure")
				protocol.Tools = batchTools{execute: func(batchCtx context.Context, req protocol.ToolRequest) (protocol.ToolResult, error) {
					if req.Name != "read" {
						t.Error("write ran after interruption")
					}
					if _, ok := batchCtx.Deadline(); !ok {
						t.Error("batch lost deadline")
					}
					started <- struct{}{}
					<-finish
					if fatal && string(req.Arguments) == `{"index":1}` {
						return protocol.ToolResult{}, failure
					}
					return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte(`{"status":"SUCCEEDED"}`)}, nil
				}}
				names := []string{"read", "read", "read", "write", "history_read", "read"}
				ctx, calls := batchInvocation(t, ctx, names)
				var batchErr error
				done := make(chan struct{})
				go func() {
					batchErr = executeBatch(ctx, calls)
					close(done)
				}()
				for range 3 {
					<-started
				}
				if !fatal {
					cancel()
				}
				close(finish)
				<-done
				if fatal {
					if !errors.Is(batchErr, failure) {
						t.Fatal("lost execution failure", batchErr)
					}
					// Fill only the failed and unstarted slots. Both successful
					// siblings must already have receipts, even the later one.
					record := characters.CurrentInvocation(ctx)
					for _, i := range []int{1, 3, 4, 5} {
						if err := record.SaveReceipt(calls[i], []byte("test refusal"), nil); err != nil {
							t.Fatal(err)
						}
					}
					messages, err := settledMessages(ctx)
					if err != nil || len(messages) != len(names)+1 || messageField(messages[1].Content, "status") != string(protocol.ToolSucceeded) || messageField(messages[3].Content, "status") != string(protocol.ToolSucceeded) {
						t.Fatal("lost known sibling receipts", messages, err)
					}
					return
				}
				messages, err := settledMessages(ctx)
				if !errors.Is(batchErr, context.Canceled) || err != nil || len(messages) != len(names)+1 {
					t.Fatal("cannot wrap up", batchErr, err)
				}
				for i := range names {
					want := protocol.ToolSucceeded
					if i >= 3 {
						want = protocol.ToolInterrupted
					}
					if messageField(messages[i+1].Content, "status") != string(want) {
						t.Fatalf("call %d: %+v", i, messages[i+1])
					}
				}
			})
		})
	}
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

type batchUsage struct{ protocol.StorageBackend }

func (batchUsage) ObserveUsage(string, protocol.TokenUsage) error { return nil }
func settledMessages(ctx context.Context) ([]protocol.Message, error) {
	window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 1000000, HardTokenLimit: 2000000}, false)
	if err != nil {
		return nil, err
	}
	if err := characters.CurrentInvocation(ctx).AppendPending(context.WithoutCancel(ctx), window); err != nil {
		return nil, err
	}
	previous := protocol.ModelFlash
	defer func() { protocol.ModelFlash = previous }()
	var messages []protocol.Message
	protocol.ModelFlash = batchProviderFunc(func(_ context.Context, request protocol.Request) (protocol.Response, error) {
		messages = request.Messages
		return protocol.Response{}, nil
	})
	_, err = window.CallModel(context.WithoutCancel(ctx), protocol.Request{})
	return messages, err
}

type batchProviderFunc func(context.Context, protocol.Request) (protocol.Response, error)

func (f batchProviderFunc) Invoke(ctx context.Context, r protocol.Request) (protocol.Response, error) {
	return f(ctx, r)
}
func (batchProviderFunc) InputCapacity() int { return 23552 }

type toolObservation struct{}

func (toolObservation) NotifyReplyRequired() {}

func (toolObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (toolObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (toolObservation) DumpContext(context.Context, string, []byte) {}

func (toolObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
