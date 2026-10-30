package engine

import (
	"context"
	"errors"
	"testing"

	"magi/internal/core/characters"
	"magi/internal/core/protocol"
)

func TestReviewerFailureDoesNotPublishLiveMemory(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	call := controlCallResponse
	model := &scriptedModel{lanes: map[string][]protocol.Response{
		"PLANNER/planner":     {call("submit_plan", `{"orders":[{"instruction":"Implement the change."}],"memo":"Dispatch intent: fulfill the assigned outcomes."}`)},
		"PLANNER/interaction": {showStep("Implementing.", false)},
		"WORKER/worker":       {call("finish_job", `{"report":"Implementation complete."}`)},
	}}
	adapter := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if request.Role == protocol.RoleReviewer {
			return protocol.Response{}, protocol.ErrModelUnavailable
		}
		return model.Invoke(ctx, request.Request)
	})
	app, err := openTestRuntime(ctx, t.TempDir(), adapter)
	mustEngine(t, err)
	t.Cleanup(func() { app.Close() })
	initializeWorkflow(t, app.Round)
	app.Round.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "accepted review history")
	before, err := app.Round.state.Reviewer.Save()
	mustEngine(t, err)
	baseline, point := app.Round.state.Workflow, *app.Round.latest
	setDisplay(func(string) error { return nil })
	setInput(nil)
	driver := New(app.Round)
	if _, err := driver.Drive(ctx); !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("review failure did not stop the round", err)
	}
	after, err := app.Round.state.Reviewer.Save()
	mustEngine(t, err)
	if string(before) != string(after) || app.Round.state.Workflow != baseline || *app.Round.latest != point {
		t.Fatal("failed review changed the committed baseline")
	}
}
