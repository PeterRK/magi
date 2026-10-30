package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

// Input names the material supplied by other roles. Review owns its ordering
// and assembly into its continuing context; engine metadata is separate.
type Input struct {
	WorkflowID    protocol.WorkflowID
	Orders        []characters.WorkOrder
	WorkerReports []characters.WorkerReport
	Memo          *characters.PlannerMemo
	UserInput     *characters.UserInput
}

// Empty identifies an idle round from its orders, memo and user input.
func (in Input) Empty() bool {
	return len(in.Orders) == 0 && in.Memo == nil && in.UserInput == nil
}

func (in Input) messages() []protocol.Message {
	messages := make([]protocol.Message, 0, len(in.Orders)+len(in.WorkerReports)+2)
	for _, order := range in.Orders {
		messages = append(messages, order.Message())
	}
	if in.Memo != nil {
		messages = append(messages, in.Memo.Message())
	}
	if in.UserInput != nil {
		messages = append(messages, in.UserInput.Message())
	}
	for _, report := range in.WorkerReports {
		messages = append(messages, report.Message())
	}
	return messages
}

// Reviewer retains its full review history. All process resources are shared.
type Reviewer struct {
	Context *corecontext.Context
}

// Review advances its continuing Context directly. Engine snapshots isolate
// accepted memory; an execution failure ends the run without publishing it.
func (r *Reviewer) Review(ctx context.Context, input Input) (characters.ReviewReport, error) {
	if err := ctx.Err(); err != nil {
		return characters.ReviewReport{}, err
	}
	if input.Empty() {
		return archiveReview(ctx, "本回合没有可供审阅的素材，任务结束")
	}
	tools, err := characters.ModelTools("history_read", "submit_review", "glob", "grep", "read", "git_log", "git_diff", "git_status")
	if err != nil {
		return characters.ReviewReport{}, err
	}
	owner := characters.InvocationRequest{
		WorkflowID: input.WorkflowID, Role: protocol.RoleReviewer, Purpose: "review",
		SessionID: "reviewer:" + string(input.WorkflowID), CycleKey: "review:" + characters.NextID(), PromptVersion: prompt.Version,
		Request: protocol.Request{Tools: tools, Effort: protocol.EffortLow},
	}
	ctx = characters.InvocationContext(ctx, owner)
	request := owner.Request
	window := r.Context
	if window == nil {
		instructions, err := prompt.ForPurpose("review")
		if err != nil {
			return characters.ReviewReport{}, err
		}
		window, err = corecontext.New(characters.RoleContextConfig(protocol.ModelPro, 1), true)
		if err != nil {
			return characters.ReviewReport{}, err
		}
		if err := window.Append(ctx, protocol.Message{Role: "system", Content: instructions}); err != nil {
			return characters.ReviewReport{}, err
		}
	}
	if err := window.TryCompact(ctx); err != nil {
		return characters.ReviewReport{}, err
	}
	for _, message := range input.messages() {
		if err := window.Append(ctx, message); err != nil {
			return characters.ReviewReport{}, err
		}
	}
	record := characters.CurrentInvocation(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return characters.ReviewReport{}, err
		}
		_, err := window.CallModel(ctx, request)
		if err != nil {
			return characters.ReviewReport{}, err
		}
		if err := ctx.Err(); err != nil {
			return characters.ReviewReport{}, err
		}
		calls, batchErr := record.Batch()
		if batchErr != nil {
			if err := record.RejectBatch(ctx, window, batchErr); err != nil {
				return characters.ReviewReport{}, err
			}
			continue
		}
		if len(calls) == 1 && calls[0].Call.Function.Name == "submit_review" {
			report, callErr := submitReview(ctx, calls[0])
			var body []byte
			if callErr == nil {
				body, _ = json.Marshal(struct {
					Status string `json:"status"`
					Ref    string `json:"ref"`
				}{Status: "committed", Ref: report.ReviewID})
			}
			if err := record.SaveReceipt(calls[0], body, callErr); err != nil {
				return characters.ReviewReport{}, err
			}
			if err := record.AppendPending(ctx, window); err != nil {
				return characters.ReviewReport{}, err
			}
			if callErr != nil {
				continue
			}
			r.Context = window
			return report, nil
		}
		if err := readBatch(ctx, calls); err != nil {
			return characters.ReviewReport{}, err
		}
		if err := ctx.Err(); err != nil {
			return characters.ReviewReport{}, err
		}
		if err := record.AppendPending(ctx, window); err != nil {
			return characters.ReviewReport{}, err
		}
	}
}

// readBatch joins every query before saving receipts in model order, including
// known sibling results after cancellation or a failed read.
func readBatch(ctx context.Context, calls []protocol.ValidatedToolCall) error {
	results := make([]struct {
		body []byte
		err  error
	}, len(calls))
	var group sync.WaitGroup
	for i, call := range calls {
		group.Go(func() {
			results[i].body, results[i].err = readTool(ctx, call)
		})
	}
	group.Wait()
	record := characters.CurrentInvocation(ctx)
	var failures []error
	for i, result := range results {
		if err := record.SaveReceipt(calls[i], result.body, result.err); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func readTool(ctx context.Context, call protocol.ValidatedToolCall) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call.Call.Function.Name == "history_read" {
		return characters.ReadHistory(ctx, call.CanonicalArguments)
	}
	result, err := protocol.Tools.Execute(characters.ToolContext(ctx, call.Call.ID), protocol.ToolRequest{ID: characters.NextID(), Name: call.Call.Function.Name, Arguments: call.CanonicalArguments})
	return characters.ToolOutput(result, err)
}

func submitReview(ctx context.Context, call protocol.ValidatedToolCall) (characters.ReviewReport, error) {
	var report characters.ReviewReport
	if err := json.Unmarshal(call.CanonicalArguments, &report); err != nil {
		return characters.ReviewReport{}, err
	}
	return archiveReview(ctx, report.Summary)
}

func archiveReview(ctx context.Context, summary string) (characters.ReviewReport, error) {
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RoleReviewer, summary)
	return characters.ReviewReport{ReviewID: ref, Summary: summary}, err
}
