package engine

import (
	"context"
	"errors"
	"sync"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	reviewerrole "magi/internal/core/characters/reviewer"
	workerrole "magi/internal/core/characters/worker"
	"magi/internal/core/protocol"
)

// runRound keeps intermediate outputs local. Only a completed round replaces
// the State baseline; execution failures require reopening the durable state.
func (e *Engine) runRound(ctx context.Context, workflow Workflow) error {
	if protocol.Tools != nil {
		if err := protocol.Tools.PrepareRound(ctx); err != nil {
			return err
		}
	}
	plan, err := e.planner.Plan(ctx, plannerrole.Input{
		State: workflow.plannerBaseline(), Handoff: e.workflow.state.Handoff,
	})
	if err != nil {
		return err
	}
	reports, userInput, err := e.runParallel(ctx, workflow.ID, plan.Orders)
	if err != nil {
		return err
	}
	input := reviewerrole.Input{
		WorkflowID: workflow.ID, Orders: plan.Orders,
		WorkerReports: reports, Memo: plan.Memo, UserInput: userInput,
	}
	report, err := e.reviewer.Review(ctx, input)
	if err != nil {
		return err
	}
	handoff := plannerrole.Handoff{Workers: reports, Review: &report}
	return e.workflow.commitRound(ctx, handoff, input.Empty(), e.planner.Context, e.reviewer.Context)
}

// Workers and interaction share cancellation and join before review or publication.
func (e *Engine) runParallel(ctx context.Context, workflow protocol.WorkflowID, orders []characters.WorkOrder) ([]characters.WorkerReport, *characters.UserInput, error) {
	roundCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workersErr error
	var reports []characters.WorkerReport
	done := make(chan struct{})
	go func() {
		defer close(done)
		reports, workersErr = runWorkers(roundCtx, cancel, orders)
	}()

	userInput, interactionErr := e.planner.Interact(roundCtx, workflow, done)
	if interactionErr != nil {
		cancel()
	}
	<-done
	parentErr := ctx.Err()
	if parentErr != nil && workersErr == roundCtx.Err() {
		workersErr = nil
	}
	// A pure interaction cancellation adds no cause when the Worker failure or
	// parent cancellation already explains it. Keep wrapped/joined errors intact.
	if (workersErr != nil || parentErr != nil) && interactionErr == roundCtx.Err() {
		interactionErr = nil
	}
	if err := errors.Join(workersErr, interactionErr, parentErr); err != nil {
		return nil, nil, err
	}
	return reports, userInput, nil
}

// runWorkers starts the entire dispatch concurrently and joins every Worker.
// Each slot belongs to one goroutine; known reports survive sibling failures.
// The round owns cancellation so a failure also stops interaction before this join.
// Deduplicate shared cancellation here; runParallel also settles its parent.
func runWorkers(ctx context.Context, cancel context.CancelFunc, orders []characters.WorkOrder) ([]characters.WorkerReport, error) {
	reports := make([]characters.WorkerReport, len(orders))
	failures := make([]error, len(orders))
	var group sync.WaitGroup
	for i, order := range orders {
		group.Go(func() {
			reports[i], failures[i] = workerrole.Run(ctx, order)
			if failures[i] != nil {
				cancel()
			}
		})
	}
	group.Wait()
	// Shared cancellation explains interrupted siblings, but must not obscure
	// independent wrapped or joined failures. Return it once if it is the only cause.
	causes := failures[:0]
	for _, err := range failures {
		if err != nil && err != ctx.Err() {
			causes = append(causes, err)
		}
	}
	if len(causes) == 0 {
		return reports, ctx.Err()
	}
	return reports, errors.Join(causes...)
}
