package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"magi/internal/core/characters"
	"magi/internal/core/protocol"
	"magi/internal/storage/mock"
)

func TestDriveRejectsUnadmittedWorkflow(t *testing.T) {
	s := &State{state: snapshot{Workflow: emptyWorkflow()}}
	result, err := New(s).Drive(context.Background())
	if !protocol.IsCode(err, protocol.CodeLifecycleConflict) || result.Phase == PhaseDormant {
		t.Fatal("unadmitted workflow reported completion", result, err)
	}
	if s.state.Workflow != emptyWorkflow() || s.latest != nil {
		t.Fatal("rejected execution changed the workflow")
	}
}

func TestWorkersStartEntireDispatchAndJoin(t *testing.T) {
	const workerCount = 3
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRun), func(t *testing.T) {
			isolateBindings(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			release := make(chan struct{})
			observed := make(chan characters.InvocationRequest, workerCount)
			complete := controlResponse("finish_job", workerJobFinish{Report: "done"})
			orders := make([]characters.WorkOrder, workerCount)
			for i := range orders {
				orders[i] = characters.WorkOrder{WorkflowID: "task", OrderID: fmt.Sprintf("P%d", i+1), Instruction: fmt.Sprintf("parallel-%d", i)}
			}
			protocol.Storage = &mock.Storage{}
			bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
				observed <- request
				select {
				case <-release:
					return complete, nil
				case <-ctx.Done():
					return protocol.Response{}, ctx.Err()
				}
			}))
			var reports []characters.WorkerReport
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() {
				defer close(exited)
				var err error
				reports, err = runWorkers(ctx, cancel, orders)
				done <- err
			}()
			defer func() { cancel(); <-exited }()
			for range orders {
				select {
				case <-observed:
				case <-ctx.Done():
					t.Fatal("Workers did not start the entire dispatch concurrently")
				}
			}
			select {
			case err := <-done:
				t.Fatal("returned before Workers finished", err)
			default:
			}
			if cancelRun {
				cancel()
			} else {
				close(release)
			}
			err := <-done
			if cancelRun {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("interrupted dispatch lost its cancellation", err)
				}
			} else {
				mustEngine(t, err)
			}
			for i, report := range reports {
				if cancelRun {
					if report != (characters.WorkerReport{}) {
						t.Fatal("cancelled Worker synthesized a report", report)
					}
				} else if report.OrderID != orders[i].OrderID || report.ReportID == "" {
					t.Fatalf("Worker lost its ordered result: %+v", report)
				}
			}
		})
	}
}

func TestWorkerModelFailureCancelsSiblingsWithoutReports(t *testing.T) {
	isolateBindings(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	roundCtx, cancelRound := context.WithCancel(ctx)
	defer cancelRound()
	started := make(chan characters.InvocationRequest, 2)
	release := make(chan struct{})
	protocol.Storage = &mock.Storage{}
	bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		started <- request
		if request.SessionID == "worker:P1" {
			select {
			case <-release:
				return protocol.Response{}, protocol.ErrModelUnavailable
			case <-ctx.Done():
				return protocol.Response{}, ctx.Err()
			}
		}
		<-ctx.Done()
		return protocol.Response{}, ctx.Err()
	}))
	orders := []characters.WorkOrder{
		{WorkflowID: "task", OrderID: "P1", Instruction: "first"},
		{WorkflowID: "task", OrderID: "P2", Instruction: "second"},
	}
	var reports []characters.WorkerReport
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		var err error
		reports, err = runWorkers(roundCtx, cancelRound, orders)
		done <- err
	}()
	defer func() { cancel(); <-exited }()
	for range orders {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("siblings did not reach the model")
		}
	}
	close(release)
	if err := <-done; !errors.Is(err, protocol.ErrModelUnavailable) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		t.Fatal("model failure did not stop the dispatch promptly", err)
	}
	for _, report := range reports {
		if report != (characters.WorkerReport{}) {
			t.Fatal("failed or interrupted Worker synthesized a report", report)
		}
	}
}

func TestParallelParentCancellationReturnedOnce(t *testing.T) {
	isolateBindings(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan struct{})
	protocol.Storage = &mock.Storage{}
	bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if request.Purpose == "worker" {
			close(started)
		} else {
			select {
			case <-started:
				cancel()
			case <-ctx.Done():
			}
		}
		<-ctx.Done()
		return protocol.Response{}, ctx.Err()
	}))
	e := &Engine{}
	e.planner.Context = testRoleContext(t, protocol.RolePlanner, "assigned work")
	_, _, err := e.runParallel(ctx, "task", []characters.WorkOrder{{
		WorkflowID: "task", OrderID: "P1", Instruction: "Perform assigned work.",
	}})
	if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Fatal("parent cancellation was lost or repeated", err)
	}
}
