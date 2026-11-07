package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"magi/internal/core/characters"
	"magi/internal/core/protocol"
)

type dialogueSource chan string

func newInteractionApp(t *testing.T, lanes map[string][]protocol.Response) (*testProject, *observedModel) {
	t.Helper()
	model := &scriptedModel{lanes: lanes}
	probe := &observedModel{Model: model}
	app, err := openTestRuntime(context.Background(), t.TempDir(), probe)
	mustEngine(t, err)
	t.Cleanup(func() { app.Close() })
	setDisplay(func(string) error { return nil })
	setInput(dialogueSource(nil))
	mustEngine(t, app.Round.Admit(context.Background()))
	return app, probe
}
func simpleDialogue() map[string][]protocol.Response {
	return map[string][]protocol.Response{
		"PLANNER/planner":     {planResponse(plannerPlanDraft{}), planResponse(plannerPlanDraft{Orders: []string{"Implement only the reviewed requirement; see [[P1]]."}}), planResponse(plannerPlanDraft{})},
		"PLANNER/interaction": {showStep("What should we do?", true), showStep("Request received; next round will implement it.", false), showStep("Implementing the reviewed request.", false), showStep("Delivered the result; preparing to sleep.", false)},

		"PLANNER/interaction_summary": {textResponse("Implement the requirement and keep the public interface unchanged. [[U1]]")},
		"WORKER/worker":               {controlResponse("finish_job", workerJobFinish{Report: "Implemented the assigned requirement and ran focused checks."})},
		"REVIEWER/review":             {reviewStep(), reviewStep()},
	}
}

func TestRoundPublishesUserInputAndContinuesToDelivery(t *testing.T) {
	isolateBindings(t)
	app, probe := newInteractionApp(t, simpleDialogue())
	const original = "Implement it and preserve the public interface."
	input := make(dialogueSource, 1)
	input <- original
	setInput(input)
	result, err := New(app.Round).Drive(context.Background())
	mustEngine(t, err)
	if result.Phase != PhaseDormant || result.Round != 4 {
		t.Fatalf("boundary=%+v", result)
	}
	var reviewedInput, reviewedWork, plannedWork bool
	for _, request := range probe.snapshot() {
		for _, message := range request.Messages {
			switch request.Purpose {
			case "review":
				reviewedInput = reviewedInput || messageField(message.Content, "user_input") != ""
				reviewedWork = reviewedWork || messageField(message.Content, "report") != ""
			case "planner":
				plannedWork = plannedWork || messageField(message.Content, "report") != ""
			}
		}
	}
	if !reviewedInput || !reviewedWork || !plannedWork {
		t.Fatal("round lost a handoff", reviewedInput, reviewedWork, plannedWork)
	}
	root := app.Root()
	mustEngine(t, app.Close())
	reopened, err := openTestProject(context.Background(), root)
	mustEngine(t, err)
	defer reopened.Close()
	if reopened.Round.CurrentWorkflow() != result {
		t.Fatal("completed workflow did not survive reopening")
	}
	body, err := reopened.Round.disk.ReadFragment(context.Background(), "U1")
	mustEngine(t, err)
	if string(body) != original {
		t.Fatal("safety point lost the user input", string(body))
	}
}

func TestPlannerInteractionFailureNeverBecomesNilInput(t *testing.T) {
	isolateBindings(t)
	lanes := simpleDialogue()
	lanes["PLANNER/interaction"] = []protocol.Response{showStep("What task?", true)}
	app, _ := newInteractionApp(t, lanes)
	input := make(dialogueSource, 1)
	input <- "An actual requirement"
	setInput(input)
	_, err := New(app.Round).Drive(context.Background())
	if err == nil {
		t.Fatal("missing message for real input became no-input sleep")
	}
	root := app.Root()
	mustEngine(t, app.Close())
	reopened, err := openTestProject(context.Background(), root)
	mustEngine(t, err)
	defer reopened.Close()
	state := reopened.Round.CurrentWorkflow()
	if state.Round != 1 || reopened.Round.state.Planner != nil || reopened.Round.state.Reviewer != nil || reopened.Round.state.Handoff.Review != nil {
		t.Fatal("unfinished interaction escaped initial safety point", state)
	}
}

func TestPlannerMemoWithoutOrdersContinuesDiscussion(t *testing.T) {
	isolateBindings(t)
	const message = "Discuss the unresolved requirement before assigning implementation."
	app, probe := newInteractionApp(t, map[string][]protocol.Response{
		"PLANNER/planner":     {planResponse(plannerPlanDraft{Memo: message})},
		"PLANNER/interaction": {showStep("Considering the next step.", false)},

		"REVIEWER/review": {reviewStep()},
	})
	_, err := New(app.Round).Drive(context.Background())
	if !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("discussion did not reach the next planning round", err)
	}
	if state := app.Round.CurrentWorkflow(); state.Round != 2 || state.Phase != PhasePlanner {
		t.Fatal("memo-only round was treated as idle", state)
	}
	reviewed := false
	for _, request := range probe.snapshot() {
		if request.Purpose != "review" {
			continue
		}
		for _, input := range request.Messages {
			if messageField(input.Content, "ref") == "P1" && messageField(input.Content, "memo") == message {
				reviewed = true
			}
		}
	}
	if !reviewed {
		t.Fatal("Reviewer did not receive the discussion message")
	}
}

func TestPlannerInteractionOutputFailureStopsBeforeReview(t *testing.T) {
	isolateBindings(t)
	app, probe := newInteractionApp(t, map[string][]protocol.Response{"PLANNER/planner": {planResponse(plannerPlanDraft{})}, "PLANNER/interaction": {showStep("Preparing to sleep.", false)}})
	failure := errors.New("display unavailable")
	setDisplay(func(string) error { return failure })
	_, err := New(app.Round).Drive(context.Background())
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, r := range probe.snapshot() {
		if r.Purpose == "review" || r.Purpose == "interaction_summary" {
			t.Fatal("failed display proceeded to review or summary")
		}
	}
}

func TestPlannerEmptyQuietRoundArchivesAndSleepsWithoutModelReview(t *testing.T) {
	isolateBindings(t)
	// No orders and no message is idle, after the mandatory display and finish.
	app, probe := newInteractionApp(t, map[string][]protocol.Response{
		"PLANNER/planner":     {planResponse(plannerPlanDraft{})},
		"PLANNER/interaction": {showStep("Pausing work.", false)},
	})
	result, err := New(app.Round).Drive(context.Background())
	mustEngine(t, err)
	if result.Phase != PhaseDormant {
		t.Fatal(result.Phase)
	}
	for _, r := range probe.snapshot() {
		if r.Purpose == "interaction_summary" || r.Role == protocol.RoleReviewer {
			t.Fatal("empty output invoked unnecessary API")
		}
	}
	if _, err := app.Round.disk.ReadFragment(context.Background(), "P1"); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
		t.Fatal("empty input published a P fragment", err)
	}
	if app.Round.state.Reviewer != nil {
		t.Fatal("idle round created Reviewer context")
	}
	report := app.Round.state.Handoff.Review
	if report == nil || report.ReviewID == "" {
		t.Fatal("idle round did not retain its report", report)
	}
	body, err := app.Round.disk.ReadFragment(context.Background(), report.ReviewID)
	mustEngine(t, err)
	if string(body) != report.Summary {
		t.Fatal("safety point did not publish the idle report")
	}
	restored, err := Open(context.Background(), app.Round.disk, app.Round.state.ProjectID)
	mustEngine(t, err)
	if restored.state.Workflow != result || restored.state.Reviewer != nil || *restored.state.Handoff.Review != *report {
		t.Fatal("idle safety point did not restore without Reviewer context")
	}
	mustEngine(t, restored.Configure(context.Background()))
	mustEngine(t, restored.Admit(context.Background()))
	workflow := restored.CurrentWorkflow()
	if workflow.ID != result.ID || restored.state.Handoff.Entry != "DORMANT_WAKE" || workflow.Round != result.Round {
		t.Fatal("idle safety point was misclassified as a cold start")
	}
}

func TestWorkerFailureCancelsInteractionBeforeSiblingCleanup(t *testing.T) {
	for _, extraFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "independent interaction failure"}[extraFailure], func(t *testing.T) {
			isolateBindings(t)
			app, probe := newInteractionApp(t, map[string][]protocol.Response{
				"PLANNER/planner": {planResponse(plannerPlanDraft{Orders: []string{
					"Fail the first model request.",
					"Hold a different operation open until cancellation.",
				}})},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			interactionStarted := make(chan struct{})
			interactionCancelled := make(chan struct{})
			siblingStarted := make(chan struct{})
			cleanup := make(chan struct{})
			independent := errors.New("interaction cleanup failed")
			script := probe.Model
			probe.Model = modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
				switch request.Purpose {
				case "interaction":
					close(interactionStarted)
					<-ctx.Done()
					close(interactionCancelled)
					if extraFailure {
						return protocol.Response{}, errors.Join(ctx.Err(), independent)
					}
					return protocol.Response{}, ctx.Err()
				case "worker":
					if strings.HasSuffix(request.SessionID, ":P1") {
						select {
						case <-interactionStarted:
						case <-ctx.Done():
							return protocol.Response{}, ctx.Err()
						}
						select {
						case <-siblingStarted:
							return protocol.Response{}, protocol.ErrModelUnavailable
						case <-ctx.Done():
							return protocol.Response{}, ctx.Err()
						}
					}
					close(siblingStarted)
					<-ctx.Done()
					<-cleanup
					return protocol.Response{}, ctx.Err()
				default:
					return script.Invoke(ctx, request.Request)
				}
			})
			done := make(chan error, 1)
			go func() {
				_, err := New(app.Round).Drive(ctx)
				done <- err
			}()
			defer func() {
				close(cleanup)
				err := <-done
				if !errors.Is(err, protocol.ErrModelUnavailable) {
					t.Error("round lost the original Worker failure", err)
				}
				if errors.Is(err, independent) != extraFailure || errors.Is(err, context.Canceled) != extraFailure {
					t.Error("round repeated cancellation or lost an independent interaction failure", err)
				}
			}()
			select {
			case <-interactionCancelled:
				if ctx.Err() != nil {
					t.Fatal("interaction only stopped when the parent expired")
				}
			case <-ctx.Done():
				t.Fatal("Worker failure did not cancel interaction before cleanup")
			}
			select {
			case err := <-done:
				// Return the result for the deferred join before failing the assertion.
				done <- err
				t.Fatal("round returned before the sibling exited", err)
			default:
			}
		})
	}
}

func TestPlannerInteractionJoinsWorkers(t *testing.T) {
	isolateBindings(t)
	lanes := map[string][]protocol.Response{
		"PLANNER/planner": {planResponse(plannerPlanDraft{
			Orders: []string{"Investigate the bounded scope."},
		})},
		"PLANNER/interaction": {showStep("Investigating.", false)},

		"WORKER/worker": {controlResponse("finish_job", workerJobFinish{Report: "Investigation done."})},
		// Report prose cannot turn a nonempty round into a dormant workflow.
		"REVIEWER/review": {controlResponse("submit_review", reviewDraft{Summary: "本回合没有可供审阅的素材，任务结束"})},
	}
	app, probe := newInteractionApp(t, lanes)
	workerEntered, workerDone := make(chan struct{}), make(chan struct{})
	release, shown := make(chan struct{}), make(chan struct{})
	script := probe.Model
	probe.Model = modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		switch request.Purpose {
		case "worker":
			close(workerEntered)
			select {
			case <-release:
			case <-ctx.Done():
				return protocol.Response{}, ctx.Err()
			}
			defer close(workerDone)
		case "review":
			select {
			case <-workerDone:
			default:
				t.Error("review began before Worker finished")
			}
		}
		return script.Invoke(ctx, request.Request)
	})
	setDisplay(func(string) error { close(shown); return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := New(app.Round).Drive(ctx)
		done <- err
	}()
	defer func() { cancel(); <-exited }()
	select {
	case <-workerEntered:
	case <-ctx.Done():
		t.Fatal("Worker did not reach the model")
	}
	select {
	case <-shown:
	case <-ctx.Done():
		t.Fatal("interaction did not display progress while Worker was running")
	}
	close(release)
	err := <-done
	if !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("expected finite script to stop at next plan", err)
	}
	calls := 0
	for _, r := range probe.snapshot() {
		if r.Purpose == "interaction" {
			calls++
		}
		if r.Purpose == "review" {
			found := false
			for _, message := range r.Messages {
				found = found || message.Role == "user" && messageField(message.Content, "memo") == "Dispatch intent: fulfill the assigned outcomes."
			}
			if !found {
				t.Fatal("Reviewer did not receive Planner's future plan")
			}
		}
	}
	if calls != 1 {
		t.Fatal("ordinary waiting must only display before runtime closure", calls)
	}
}
