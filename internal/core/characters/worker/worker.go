package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

const executionTimeout = time.Hour

type jobFinish struct {
	Report string `json:"report"`
}

// Run synchronously executes the supplied order and returns its report or an error.
// The order carries its Planner-assigned P reference.
func Run(ctx context.Context, order characters.WorkOrder) (characters.WorkerReport, error) {
	tools, err := modelTools()
	if err != nil {
		return characters.WorkerReport{}, err
	}
	key := "worker:" + order.OrderID
	request := characters.InvocationRequest{
		WorkflowID: order.WorkflowID, Role: protocol.RoleWorker, Purpose: "worker",
		SessionID: key, CycleKey: key, PromptVersion: prompt.Version,
		Request: protocol.Request{Tools: tools},
	}
	instructions, err := prompt.ForPurpose("worker")
	if err != nil {
		return characters.WorkerReport{}, err
	}
	ctx = characters.InvocationContext(ctx, request)
	window, err := corecontext.New(characters.RoleContextConfig(protocol.ModelFlash, 2), false)
	if err != nil {
		return characters.WorkerReport{}, err
	}
	defer window.DumpContext(ctx, "worker_exit")
	for _, message := range []protocol.Message{{Role: "system", Content: instructions}, order.Message()} {
		if err := window.Append(ctx, message); err != nil {
			return characters.WorkerReport{}, err
		}
	}
	executionCtx, cancel := context.WithTimeout(ctx, executionTimeout)
	result, err := runPhase(executionCtx, order, window)
	executionErr := executionCtx.Err()
	cancel()
	// Only the execution timeout starts wrap-up. Parent cancellation and
	// uncertain effects retain their original failure boundary.
	if err == nil || executionErr != context.DeadlineExceeded || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || protocol.IsCode(err, protocol.CodeIntegrityFailure) || protocol.IsCode(err, protocol.CodeOutcomeUnknown) || errors.Is(err, protocol.ErrModelUnavailable) {
		return executionResult(ctx, order, result, err)
	}
	if err = wrapUp(ctx, window); err == nil {
		result, err = runPhase(ctx, order, window)
	}
	return executionResult(ctx, order, result, err)
}

func wrapUp(ctx context.Context, window *corecontext.Context) error {
	record := characters.CurrentInvocation(ctx)
	tools, err := characters.ModelTools("finish_job")
	if err != nil {
		return err
	}
	record.EnterWrapUp(tools)
	if err := record.AppendPending(ctx, window); err != nil {
		return err
	}
	return window.Append(ctx, protocol.Message{Role: "system", Content: prompt.WorkerWrapUp})
}

// runPhase owns both normal execution and wrap-up; the invocation's current
// tools and parent context determine which phase is active.
func runPhase(ctx context.Context, order characters.WorkOrder, window *corecontext.Context) (characters.WorkerReport, error) {
	record := characters.CurrentInvocation(ctx)
	request := record.Owner().Request
	for {
		if err := ctx.Err(); err != nil {
			return characters.WorkerReport{}, err
		}
		_, err := window.CallModel(ctx, request)
		if err != nil {
			return characters.WorkerReport{}, err
		}
		calls, batchErr := record.Batch()
		if batchErr != nil {
			if err := record.RejectBatch(ctx, window, batchErr); err != nil {
				return characters.WorkerReport{}, err
			}
			continue
		}
		if len(calls) == 1 && calls[0].Call.Function.Name == "finish_job" {
			call := calls[0]
			var candidate characters.WorkerReport
			callErr := ctx.Err()
			if callErr == nil {
				candidate, callErr = submitJob(ctx, order, call)
			}
			if callErr == nil {
				// This job's Context is discarded; no success receipt is needed.
				return candidate, nil
			}
			var result []byte
			receiptErr := callErr
			interrupted := callErr == context.Canceled || callErr == context.DeadlineExceeded
			if interrupted {
				// Cancellation before publication needs a paired refusal for wrap-up.
				result, receiptErr = interruptedToolResult(callErr), nil
			}
			if err := record.SaveReceipt(call, result, receiptErr); err != nil {
				return characters.WorkerReport{}, err
			}
			if interrupted {
				return characters.WorkerReport{}, callErr
			}
		} else if err := executeBatch(ctx, calls); err != nil {
			// Known results and interruption receipts are retained for wrap-up.
			return characters.WorkerReport{}, err
		}
		if err := record.AppendPending(ctx, window); err != nil {
			return characters.WorkerReport{}, err
		}
	}
}

func submitJob(ctx context.Context, order characters.WorkOrder, call protocol.ValidatedToolCall) (characters.WorkerReport, error) {
	var draft jobFinish
	if err := json.Unmarshal(call.CanonicalArguments, &draft); err != nil {
		return characters.WorkerReport{}, err
	}
	return finish(ctx, order, draft.Report)
}

func executionResult(ctx context.Context, order characters.WorkOrder, result characters.WorkerReport, err error) (characters.WorkerReport, error) {
	if err == nil {
		return result, nil
	}
	if protocol.IsCode(err, protocol.CodeIntegrityFailure) || protocol.IsCode(err, protocol.CodeOutcomeUnknown) || errors.Is(err, protocol.ErrModelUnavailable) {
		return characters.WorkerReport{}, err
	}
	// Parent interruption ends the round; it is not a reportable job result.
	if ctx.Err() != nil {
		return characters.WorkerReport{}, err
	}
	if protocol.IsCode(err, protocol.CodeCapacityLimited) {
		return finish(ctx, order, "worker cannot continue: "+err.Error())
	}
	return characters.WorkerReport{}, err
}

func finish(ctx context.Context, order characters.WorkOrder, report string) (characters.WorkerReport, error) {
	result := characters.WorkerReport{OrderID: order.OrderID, Body: report}
	// Archive only the body; the order reference stays in the report envelope.
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RoleWorker, result.Body)
	if err != nil {
		return characters.WorkerReport{}, err
	}
	result.ReportID = ref
	return result, nil
}
