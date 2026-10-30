package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/characters"
	workerrole "magi/internal/core/characters/worker"
	"magi/internal/core/protocol"
)

func TestWorkerToolDeadlineDoesNotEndHealthyAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		resetWorkerHistory()
		calls := 0
		model := modelFunc(func(ctx context.Context, req characters.InvocationRequest) (protocol.Response, error) {
			calls++
			if ctx.Err() != nil || req.WorkerWrapUp {
				t.Fatal("local tool deadline became role cancellation or wrap-up")
			}
			if calls == 1 {
				return modelResponse(toolCall("local-deadline", "bash", `{"command":"fixture"}`)), nil
			}
			if receipt := req.Messages[len(req.Messages)-1].Content; receipt != "partial\nstatus: timed_out" {
				t.Fatalf("lost settled partial failure: %s", receipt)
			}
			if deadline, ok := ctx.Deadline(); !ok || !time.Now().Before(deadline) {
				t.Fatal("role deadline actually expired")
			}
			return modelResponse(toolCall("finish", "finish_job", "{\"report\":\"Local verification timed out; its partial output was received. Reported scoped progress without claiming that verification passed.\"}")), nil
		})
		provider := toolFunc(func(parent context.Context, _ protocol.ToolRequest) (protocol.ToolResult, error) {
			child, stop := context.WithTimeout(parent, 10*time.Millisecond)
			defer stop()
			<-child.Done()
			if parent.Err() != nil {
				t.Fatal("fixture parent expired with tool child")
			}
			return protocol.ToolResult{Status: protocol.ToolTimedOut, Body: []byte("partial\nstatus: timed_out")}, child.Err()
		})
		configureWorker(model, provider)
		work := order("local-tool-deadline")
		cacheOrders(t, &work)
		result, err := workerrole.Run(context.Background(), work)
		mustWorker(t, err)
		if calls != 2 || result.ReportID == "" {
			t.Fatalf("local tool timeout killed role: calls=%d result=%+v", calls, result)
		}
	})
}

func TestWorkerParentInterruptionReturnsError(t *testing.T) {
	for _, source := range []string{"model", "tool"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", source, deadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					isolateBindings(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if deadline {
						var stop context.CancelFunc
						ctx, stop = context.WithTimeout(ctx, time.Second)
						defer stop()
					}
					interrupt := func(callCtx context.Context) {
						if !deadline {
							cancel()
						}
						<-callCtx.Done()
					}
					calls, toolJoined := 0, false
					model := modelFunc(func(callCtx context.Context, _ characters.InvocationRequest) (protocol.Response, error) {
						calls++
						if source == "model" {
							interrupt(callCtx)
							return protocol.Response{}, callCtx.Err()
						}
						return modelResponse(toolCall("work", "bash", `{"command":"fixture"}`)), nil
					})
					provider := toolFunc(func(callCtx context.Context, _ protocol.ToolRequest) (protocol.ToolResult, error) {
						interrupt(callCtx)
						time.Sleep(time.Second) // Cancellation must still join tool cleanup.
						toolJoined = true
						return protocol.ToolResult{Status: protocol.ToolInterrupted, Body: []byte("known partial output")}, callCtx.Err()
					})
					resetWorkerHistory()
					configureWorker(model, provider)
					work := order("parent-interruption")
					cacheOrders(t, &work)
					result, err := workerrole.Run(ctx, work)
					if ctx.Err() == nil || !errors.Is(err, ctx.Err()) || result != (characters.WorkerReport{}) || calls != 1 || toolJoined != (source == "tool") {
						t.Fatalf("parent interruption lost: result=%+v calls=%d joined=%t err=%v", result, calls, toolJoined, err)
					}
					if _, err := characters.ReadHistory(context.Background(), []byte(`{"ref":"W1"}`)); err == nil {
						t.Fatal("parent interruption synthesized a report")
					}
				})
			})
		}
	}
}

func TestWorkerToolDeadlineWaitsForOutcomeBeforeWrapUp(t *testing.T) {
	for _, mode := range []string{"known", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			isolateBindings(t)
			synctest.Test(t, func(t *testing.T) {
				modelCalls, toolCalls := 0, 0
				toolFinished := false
				model := modelFunc(func(ctx context.Context, r characters.InvocationRequest) (protocol.Response, error) {
					modelCalls++
					if modelCalls == 1 {
						response := modelResponse(toolCall("work", "bash", `{"command":"fixture"}`), toolCall("unstarted", "bash", `{"command":"fixture"}`))
						if mode == "capacity" {
							response.Usage.InputTokens = 19000
						}
						return response, nil
					}
					if modelCalls != 2 || !toolFinished || !r.WorkerWrapUp || ctx.Err() != nil {
						t.Fatal("wrap-up started without a known completed outcome")
					}
					receipts := map[string]string{}
					for _, message := range r.Messages {
						if message.Role == "tool" {
							receipts[message.ToolCallID] = message.Content
						}
					}
					var unstarted struct{ Status protocol.ToolCallStatus }
					if receipts["work"] != "known partial result" || json.Unmarshal([]byte(receipts["unstarted"]), &unstarted) != nil || unstarted.Status != protocol.ToolTimedOut {
						t.Fatal("wrap-up lost known or unstarted outcomes", receipts)
					}
					return modelResponse(toolCall("finish", "finish_job", `{"report":"Known partial work; remaining work was not started."}`)), nil
				})
				provider := toolFunc(func(ctx context.Context, _ protocol.ToolRequest) (protocol.ToolResult, error) {
					toolCalls++
					<-ctx.Done()
					// A deadline cancels execution; it does not mean cleanup has finished.
					time.Sleep(time.Second)
					toolFinished = true
					return protocol.ToolResult{Status: protocol.ToolTimedOut, Body: []byte("known partial result")}, ctx.Err()
				})
				resetWorkerHistory()
				configureWorker(model, provider)
				work := order("tool-timeout")
				cacheOrders(t, &work)
				result, err := workerrole.Run(context.Background(), work)
				if !toolFinished || toolCalls != 1 {
					t.Fatal("Worker failed to join the tool or started another", toolCalls)
				}
				if mode == "capacity" {
					if err != nil || modelCalls != 1 || !strings.Contains(result.Body, "worker cannot continue:") || result.ReportID == "" {
						t.Fatal("wrap-up capacity failure did not report its cause", result, err)
					}
				} else if err != nil || modelCalls != 2 {
					t.Fatal("known timed-out tool did not reach wrap-up", result, err)
				}
			})
		})
	}
}

type delayedUsageStorage struct{ protocol.StorageBackend }

func (s delayedUsageStorage) ObserveUsage(role string, usage protocol.TokenUsage) error {
	// Cross the execution deadline after transport success, before dispatch.
	time.Sleep(2 * time.Millisecond)
	return s.StorageBackend.ObserveUsage(role, usage)
}

type deadlinePublicationStorage struct {
	protocol.StorageBackend
	deadline time.Time
	rejected bool
}

func (s *deadlinePublicationStorage) Allocate(role string) (string, error) {
	if role == string(protocol.RoleWorker) && !s.rejected {
		s.rejected = true
		// Inject a cancellation refusal before history publication, after the
		// control-call guard. No number or report is published by this attempt.
		time.Sleep(time.Until(s.deadline) + time.Millisecond)
		return "", context.DeadlineExceeded
	}
	return s.StorageBackend.Allocate(role)
}

func TestWorkerCancelledPublicationRetainsReceipt(t *testing.T) {
	isolateBindings(t)
	synctest.Test(t, func(t *testing.T) {
		var disk *deadlinePublicationStorage
		calls := 0
		model := modelFunc(func(ctx context.Context, r characters.InvocationRequest) (protocol.Response, error) {
			calls++
			if calls == 1 {
				disk.deadline, _ = ctx.Deadline()
				return modelResponse(toolCall("unsubmitted", "finish_job", `{"report":"Original report."}`)), nil
			}
			if calls != 2 || !r.WorkerWrapUp || ctx.Err() != nil {
				t.Fatal("cancelled publication failed to enter live wrap-up")
			}
			found := false
			for _, message := range r.Messages {
				if message.Role == "tool" && message.ToolCallID == "unsubmitted" {
					var receipt struct{ Status protocol.ToolCallStatus }
					found = json.Unmarshal([]byte(message.Content), &receipt) == nil && receipt.Status == protocol.ToolTimedOut
				}
			}
			if !found {
				t.Fatal("cancelled publication has no failure receipt")
			}
			return modelResponse(toolCall("finish", "finish_job", `{"report":"Recovered report."}`)), nil
		})
		resetWorkerHistory()
		configureWorker(model)
		disk = &deadlinePublicationStorage{StorageBackend: protocol.Storage}
		protocol.Storage = disk
		work := order("publication-timeout")
		cacheOrders(t, &work)
		result, err := workerrole.Run(context.Background(), work)
		if err != nil || calls != 2 || result.Body != "Recovered report." || result.ReportID != "W1" {
			t.Fatal("cancelled publication lost or duplicated the report", result, calls, err)
		}
	})
}

func TestWorkerDeadlineBeforeDispatchSettlesUnstartedCalls(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprint("control-", control), func(t *testing.T) {
			isolateBindings(t)
			synctest.Test(t, func(t *testing.T) {
				unstarted := []protocol.ToolCall{
					toolCall("first", "bash", `{"command":"fixture"}`),
					toolCall("second", "bash", `{"command":"fixture"}`),
				}
				if control {
					unstarted = []protocol.ToolCall{toolCall("finish", "finish_job", `{"report":"Unsubmitted report."}`)}
				}
				modelCalls, toolCalls := 0, 0
				model := modelFunc(func(ctx context.Context, r characters.InvocationRequest) (protocol.Response, error) {
					modelCalls++
					if modelCalls == 1 {
						deadline, _ := ctx.Deadline()
						time.Sleep(time.Until(deadline) - time.Millisecond)
						return modelResponse(unstarted...), nil
					}
					if _, limited := ctx.Deadline(); modelCalls != 2 || !r.WorkerWrapUp || ctx.Err() != nil || limited {
						t.Fatal("unstarted calls failed to reach live wrap-up")
					}
					var receipts []protocol.Message
					for _, message := range r.Messages {
						if message.Role == "tool" {
							receipts = append(receipts, message)
						}
					}
					if len(receipts) != len(unstarted) {
						t.Fatal("missing interruption receipts", receipts)
					}
					for i, receipt := range receipts {
						var result struct{ Status protocol.ToolCallStatus }
						if receipt.ToolCallID != unstarted[i].ID || json.Unmarshal([]byte(receipt.Content), &result) != nil || result.Status != protocol.ToolTimedOut {
							t.Fatal("unstarted call was not settled in order", receipts)
						}
					}
					return modelResponse(toolCall("wrap-up", "finish_job", `{"report":"No requested operations were started."}`)), nil
				})
				provider := toolFunc(func(context.Context, protocol.ToolRequest) (protocol.ToolResult, error) {
					toolCalls++
					return protocol.ToolResult{Status: protocol.ToolSucceeded, Body: []byte("unexpected execution")}, nil
				})
				resetWorkerHistory()
				configureWorker(model, provider)
				protocol.Storage = delayedUsageStorage{protocol.Storage}
				work := order("unstarted-timeout")
				cacheOrders(t, &work)
				result, err := workerrole.Run(context.Background(), work)
				if err != nil || modelCalls != 2 || toolCalls != 0 || result.Body != "No requested operations were started." {
					t.Fatalf("unstarted calls did not settle: result=%+v models=%d tools=%d err=%v", result, modelCalls, toolCalls, err)
				}
			})
		})
	}
}

func TestWorkerSoftDeadlineStartsFinishOnlyWrapUp(t *testing.T) {
	isolateBindings(t)
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		model := modelFunc(func(ctx context.Context, r characters.InvocationRequest) (protocol.Response, error) {
			calls++
			if calls == 1 {
				return modelResponse(toolCall("work", "bash", `{"command":"fixture"}`)), nil
			}
			if calls == 2 {
				<-ctx.Done()
				return protocol.Response{}, ctx.Err()
			}
			if calls > 4 || !r.WorkerWrapUp || len(r.Tools) != 1 || r.Tools[0].Function.Name != "finish_job" {
				t.Fatal("wrap-up must only offer finish_job")
			}
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("wrap-up retained the execution deadline")
			}
			if calls == 3 {
				// An obsolete history call must be rejected, not executed.
				return modelResponse(toolCall("read-in-wrap-up", "history_read", `{"ref":"P999"}`)), nil
			}
			receipts := map[string]string{}
			for _, message := range r.Messages {
				if message.Role == "tool" {
					receipts[message.ToolCallID] = message.Content
				}
			}
			if receipts["work"] == "" || !strings.Contains(receipts["read-in-wrap-up"], "not allowed") {
				t.Fatal("wrap-up lost known work or allowed history lookup", receipts)
			}
			time.Sleep(20 * time.Minute)
			if ctx.Err() != nil {
				t.Fatal("wrap-up acquired a new deadline")
			}
			response := modelResponse(toolCall("finish", "finish_job", `{"report":"Settled work retained; implementation incomplete."}`))
			response.Usage.InputTokens = 19000 // A terminal report needs no append or Full.
			return response, nil
		})
		resetWorkerHistory()
		configureWorker(model)
		work := order("soft-wrap")
		cacheOrders(t, &work)
		result, err := workerrole.Run(context.Background(), work)
		if err != nil || calls != 4 || result.ReportID == "" {
			t.Fatalf("wrap-up failed: %+v calls=%d err=%v", result, calls, err)
		}
	})
}
