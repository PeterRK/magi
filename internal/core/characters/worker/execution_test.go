package worker_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/characters"
	workerrole "magi/internal/core/characters/worker"
	"magi/internal/core/protocol"
)

func TestWorkerModelFailureStopsWithoutReport(t *testing.T) {
	for _, mode := range []string{"permanent", "deadline-with-failure", "wrap-up"} {
		t.Run(mode, func(t *testing.T) {
			isolateBindings(t)
			synctest.Test(t, func(t *testing.T) {
				calls, want := 0, 1
				if mode == "wrap-up" {
					want = 2
				}
				model := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
					calls++
					if calls > want {
						t.Fatal("communication failure continued model execution")
					}
					if mode == "deadline-with-failure" {
						<-ctx.Done()
						return protocol.Response{}, errors.Join(ctx.Err(), protocol.ErrModelUnavailable)
					}
					if mode == "wrap-up" {
						if calls == 1 {
							<-ctx.Done()
							return protocol.Response{}, ctx.Err()
						}
						if !request.WorkerWrapUp {
							t.Fatal("soft deadline did not enter wrap-up")
						}
					}
					return protocol.Response{}, protocol.ErrModelUnavailable
				})
				resetWorkerHistory()
				configureWorker(model)
				work := order("model-failure")
				cacheOrders(t, &work)
				checkpoints := 0
				protocol.Observer = checkpointObservation(func(ctx context.Context, event string, snapshot []byte) {
					checkpoints++
					if event != "worker_exit" || len(snapshot) == 0 || protocol.DiagnosticIdentityFromContext(ctx).Role != protocol.RoleWorker {
						t.Fatal("invalid Worker failure checkpoint")
					}
				})
				result, err := workerrole.Run(context.Background(), work)
				if checkpoints != 1 {
					t.Fatal("Worker failure lost its context checkpoint")
				}
				if !errors.Is(err, protocol.ErrModelUnavailable) || result.ReportID != "" || calls != want {
					t.Fatalf("lost fatal model failure: calls=%d result=%+v err=%v", calls, result, err)
				}
			})
		})
	}
}

type checkpointObservation func(context.Context, string, []byte)

func (checkpointObservation) NotifyReplyRequired() {}

func (checkpointObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (checkpointObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (checkpointObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
func (f checkpointObservation) DumpContext(ctx context.Context, event string, snapshot []byte) {
	f(ctx, event, snapshot)
}

func TestWorkerAssemblesCompleteOrderAndAssociatesReport(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	work := order("direct-order")
	// Report publication must not need an order cache entry.
	work.OrderID = "P42"
	model := modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if len(request.Messages) != 2 || request.Messages[1].Role != "user" || request.Messages[1].Content != work.Message().Content {
			t.Fatalf("Worker lost order fields: %+v", request.Messages)
		}
		return controlResponse("finish_job", workerJobFinish{Report: "message received"}), nil
	})
	resetWorkerHistory()
	configureWorker(model)
	checkpoints := 0
	protocol.Observer = checkpointObservation(func(ctx context.Context, event string, snapshot []byte) {
		checkpoints++
		identity := protocol.DiagnosticIdentityFromContext(ctx)
		if event != "worker_exit" || len(snapshot) == 0 || identity.Role != protocol.RoleWorker || identity.SessionID != "worker:"+work.OrderID {
			t.Fatal("invalid Worker completion checkpoint")
		}
	})
	result, err := workerrole.Run(ctx, work)
	if checkpoints != 1 {
		t.Fatal("Worker completion lost its context checkpoint")
	}
	mustWorker(t, err)
	body, err := characters.ReadHistory(ctx, []byte(`{"ref":"`+result.ReportID+`"}`))
	mustWorker(t, err)
	if result.OrderID != work.OrderID || result.Body != "message received" || string(body) != result.Body {
		t.Fatalf("report lost its body or association: %+v, archive=%s", result, body)
	}
}

type cancelAfterReportAllocation struct {
	protocol.StorageBackend
	cancel context.CancelFunc
}

func (s cancelAfterReportAllocation) Allocate(source string) (string, error) {
	ref, err := s.StorageBackend.Allocate(source)
	if err == nil && source == string(protocol.RoleWorker) {
		s.cancel()
	}
	return ref, err
}

func TestWorkerKeepsCommittedReportAfterLateCancellation(t *testing.T) {
	isolateBindings(t)
	resetWorkerHistory()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configureWorker(modelFunc(func(context.Context, characters.InvocationRequest) (protocol.Response, error) {
		return controlResponse("finish_job", workerJobFinish{Report: "Delivered result."}), nil
	}))
	protocol.Storage = cancelAfterReportAllocation{protocol.Storage, cancel}
	work := characters.WorkOrder{OrderID: "P1", WorkflowID: "task", Instruction: "Complete the order."}
	result, err := workerrole.Run(ctx, work)
	mustWorker(t, err)
	if ctx.Err() == nil || result.ReportID == "" || result.Body != "Delivered result." {
		t.Fatal("late cancellation replaced a committed result", result)
	}
	body, err := characters.ReadHistory(context.Background(), []byte(`{"ref":"`+result.ReportID+`"}`))
	mustWorker(t, err)
	if string(body) != result.Body {
		t.Fatal("committed report was discarded")
	}
}

func TestWorkerForwardsBatchResultsInOrder(t *testing.T) {
	isolateBindings(t)
	step := 0
	model := modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		step++
		if step == 1 {
			var names []string
			for _, tool := range request.Tools {
				names = append(names, tool.Function.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{"bash", "edit", "finish_job", "glob", "grep", "history_read", "read", "write"}) {
				t.Fatalf("unexpected Worker capabilities: %v", names)
			}
			return modelResponse(
				toolCall("create", "write", `{"path":"probe.txt","content":"created"}`),
				toolCall("edit", "edit", `{"path":"probe.txt","old_string":"created","new_string":"updated"}`),
				toolCall("check", "bash", `{"command":"test -f probe.txt && cat probe.txt"}`),
				toolCall("read", "read", `{"path":"probe.txt"}`),
			), nil
		}
		var ids []string
		for _, message := range request.Messages {
			if message.Role != "tool" {
				continue
			}
			ids = append(ids, message.ToolCallID)
			if message.Content != "result:"+map[string]string{"create": "write", "edit": "edit", "check": "bash", "read": "read"}[message.ToolCallID] {
				t.Fatalf("tool result was lost or rewritten: %+v", message)
			}
		}
		if strings.Join(ids, ",") != "create,edit,check,read" {
			t.Fatal("reordered receipts", ids)
		}
		return controlResponse("finish_job", workerJobFinish{Report: "file updated, check failed, and subsequent read completed"}), nil
	})
	resetWorkerHistory()
	var executed []string
	bindWorker(model, testTools{func(_ context.Context, request protocol.ToolRequest) (protocol.ToolResult, error) {
		executed = append(executed, request.Name)
		result := protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte("result:" + request.Name)}
		if request.Name == "bash" {
			result.Status = protocol.ToolFailed
			return result, errors.New("exit status 1")
		}
		return result, nil
	}})
	work := order("edit-then-check")
	cacheOrders(t, &work)
	result, err := workerrole.Run(context.Background(), work)
	mustWorker(t, err)
	if result.ReportID == "" || step != 2 || strings.Join(executed, ",") != "write,edit,bash,read" {
		t.Fatal("batch did not complete", result)
	}
}

// History rules are tested in characters; Worker owns feeding both success
// and repairable refusal receipts into its next model request.
func TestWorkerHistoryReceiptsReachNextRequest(t *testing.T) {
	isolateBindings(t)
	resetWorkerHistory()
	work := order("history")
	cacheOrders(t, &work)
	step := 0
	model := modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		step++
		if step == 1 {
			return modelResponse(toolCall("read", "history_read", `{"ref":"P1"}`), toolCall("missing", "history_read", `{"ref":"P999"}`)), nil
		}
		var receipts []protocol.Message
		for _, message := range request.Messages {
			if message.Role == "tool" {
				receipts = append(receipts, message)
			}
		}
		if step != 2 || len(receipts) != 2 || receipts[0].ToolCallID != "read" || receipts[0].Content != work.Instruction || receipts[1].ToolCallID != "missing" || messageField(receipts[1].Content, "error") != "source_unavailable" {
			t.Fatal("history outcomes did not reach the model in order", receipts)
		}
		return modelResponse(toolCall("finish", "finish_job", `{"report":"Consulted available history."}`)), nil
	})
	configureWorker(model)
	result, err := workerrole.Run(context.Background(), work)
	mustWorker(t, err)
	if step != 2 {
		t.Fatal("history refusal stopped the Worker", result)
	}
}

func TestWorkerRejectsInvalidBatchBeforeToolExecution(t *testing.T) {
	isolateBindings(t)
	step := 0
	model := modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		step++
		if step == 1 {
			return modelResponse(
				toolCall("valid", "read", `{"path":"sample"}`),
				toolCall("invalid", "bash", `{"command":"touch marker","background":true}`),
			), nil
		}
		if step != 2 {
			t.Fatal("unexpected model continuation")
		}
		receipts := request.Messages[len(request.Messages)-2:]
		if receipts[0].Role != "tool" || receipts[0].ToolCallID != "valid" || receipts[1].Role != "tool" || receipts[1].ToolCallID != "invalid" || receipts[0].Content != receipts[1].Content || messageField(receipts[0].Content, "error") != "argument_invalid" {
			t.Fatal("rejected batch lost paired receipts", receipts)
		}
		return controlResponse("finish_job", workerJobFinish{Report: "No tools executed."}), nil
	})
	resetWorkerHistory()
	configureWorker(model, func(context.Context, protocol.ToolRequest) (protocol.ToolResult, error) {
		t.Fatal("invalid batch reached external tools")
		return protocol.ToolResult{}, nil
	})
	work := order("rejected-batch")
	cacheOrders(t, &work)
	_, err := workerrole.Run(context.Background(), work)
	mustWorker(t, err)
}

func TestWorkerRepairsToolRefusalAndPreservesBatch(t *testing.T) {
	isolateBindings(t)
	resetWorkerHistory()
	step := 0
	var executed atomic.Int32
	refusal := protocol.WithCandidateFeedback(protocol.InvalidCommand("rejected"), "choose another target")
	wantReceipt := string(protocol.SemanticToolCallError(refusal).(protocol.ToolCallError).Receipt())
	model := modelFunc(func(_ context.Context, r characters.InvocationRequest) (protocol.Response, error) {
		step++
		switch step {
		case 1:
			return modelResponse(toolCall("bad", "read", `{"path":"reject"}`), toolCall("good", "read", `{"path":"accepted"}`)), nil
		case 2:
			receipts := r.Messages[len(r.Messages)-2:]
			if receipts[0].Role != "tool" || receipts[0].ToolCallID != "bad" || receipts[0].Content != wantReceipt {
				t.Fatalf("refusal was not paired: %+v", receipts)
			}
			if receipts[1].ToolCallID != "good" || receipts[1].Content != "opaque result" || executed.Load() != 1 {
				t.Fatal("healthy batch member was lost or repeated", receipts)
			}
			return modelResponse(toolCall("corrected", "read", `{"path":"accepted"}`)), nil
		case 3:
			last := r.Messages[len(r.Messages)-1]
			if last.ToolCallID != "corrected" || last.Content != "opaque result" {
				t.Fatal("corrected result lost", last)
			}
			return controlResponse("finish_job", workerJobFinish{Report: "corrected query complete"}), nil
		default:
			t.Fatal("unnecessary model continuation")
			return protocol.Response{}, nil
		}
	})
	configureWorker(model, func(_ context.Context, request protocol.ToolRequest) (protocol.ToolResult, error) {
		if string(request.Arguments) == `{"path":"reject"}` {
			return protocol.ToolResult{}, refusal
		}
		if request.ID == "" || request.Name != "read" || string(request.Arguments) != `{"path":"accepted"}` {
			return protocol.ToolResult{}, errors.New("changed query identity or arguments")
		}
		executed.Add(1)
		return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte("opaque result")}, nil
	})
	work := order("repair")
	cacheOrders(t, &work)
	_, err := workerrole.Run(context.Background(), work)
	mustWorker(t, err)
	if step != 3 || executed.Load() != 2 {
		t.Fatal("repair did not complete", step, executed.Load())
	}
}

func TestWorkerInstancesKeepPrivateContextAndFixedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		resetWorkerHistory()
		var previousSession string
		for _, id := range []string{"P1", "P2"} {
			work := characters.WorkOrder{OrderID: id, WorkflowID: "task", Instruction: "Work for " + id}
			calls := 0
			var session string
			var deadline time.Time
			configureWorker(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
				calls++
				d, ok := ctx.Deadline()
				if !ok {
					t.Fatal("Worker request lost its execution deadline")
				}
				if calls == 1 {
					session, deadline = request.SessionID, d
					if session == "" || session == previousSession || len(request.Messages) != 2 || request.Messages[1].Content != work.Message().Content {
						t.Fatal("Worker inherited another instance's session or context", request)
					}
					time.Sleep(time.Second)
					return modelResponse(toolCall("check", "bash", `{"command":"fixture"}`)), nil
				}
				if calls != 2 || request.SessionID != session || !d.Equal(deadline) {
					t.Fatal("Worker changed its session, renewed its deadline or continued after delivery")
				}
				return controlResponse("finish_job", workerJobFinish{Report: "done"}), nil
			}))
			result, err := workerrole.Run(context.Background(), work)
			mustWorker(t, err)
			if calls != 2 || result.ReportID == "" || result.OrderID != id || result.Body != "done" {
				t.Fatal("Worker did not complete the supplied order", calls, result)
			}
			previousSession = session
		}
	})
}

func TestWorkerRetainsConsecutiveErrorStopLoss(t *testing.T) {
	isolateBindings(t)
	calls := 0
	model := modelFunc(func(context.Context, characters.InvocationRequest) (protocol.Response, error) {
		calls++
		return modelResponse(toolCall("bad-finish", "finish_job", "{\"report\":\"\"}")), nil
	})
	resetWorkerHistory()
	configureWorker(model)
	work := order("timeout-only-errors")
	cacheOrders(t, &work)
	result, err := workerrole.Run(context.Background(), work)
	mustWorker(t, err)
	if calls != 5 || result.ReportID == "" || !strings.Contains(result.Body, "worker cannot continue:") {
		t.Fatalf("error stop-loss lost: calls=%d %+v", calls, result)
	}

}
