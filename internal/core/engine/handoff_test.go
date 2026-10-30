package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	"magi/internal/core/protocol"
)

func readHistory(ctx context.Context, args historyReadArgs) (string, error) {
	body, _ := json.Marshal(args)
	outcome, err := characters.ReadHistory(ctx, body)
	return string(outcome), err
}

func TestSafetyPointPublishesHistoryAndRestoresHandoff(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	initializeWorkflow(t, s)
	orders := []characters.WorkOrder{{Instruction: "Preserve the supplied signature."}}
	mustEngine(t, cacheOrders(ctx, orders))
	orderRef := orders[0].OrderID
	account := "Partial findings; see [[" + orderRef + "]]."
	w, err := characters.CacheHistoryOutput(ctx, protocol.RoleWorker, account)
	mustEngine(t, err)
	review := "Reconsider the premise based on [[" + w + "]]."
	r, err := characters.CacheHistoryOutput(ctx, protocol.RoleReviewer, review)
	mustEngine(t, err)
	if _, err := disk.ReadFragment(ctx, orderRef); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
		t.Fatal("handoff published history before the safety point", err)
	}
	originals := map[string]string{orderRef: orders[0].Instruction, w: account, r: review}
	handoff := plannerrole.Handoff{Workers: []characters.WorkerReport{workerHandoff(w, orderRef, account)}, Review: &characters.ReviewReport{ReviewID: r, Summary: review}}
	mustEngine(t, s.commitRound(ctx, handoff, false, testRoleContext(t, protocol.RolePlanner, "completed planning"), testRoleContext(t, protocol.RoleReviewer, "completed review")))
	for ref, body := range originals {
		got, err := disk.ReadFragment(ctx, ref)
		mustEngine(t, err)
		if string(got) != body {
			t.Fatal("durable output changed")
		}
	}
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	if !reflect.DeepEqual(restored.state.Handoff, s.state.Handoff) {
		t.Fatal("full safety point lost numbered handoff")
	}

}

func TestOutputWriteFailureStopsBeforePublishingPoint(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	s.state.Workflow = testBaseline(t)
	s.state.Handoff = coldStartHandoff()
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RoleWorker, "original report")
	mustEngine(t, err)
	fault := errors.New("history write failed")
	archive := &publicationFault{StorageBackend: disk, stage: "history", fault: fault}
	s.disk = archive
	if err = s.publish(ctx, 0); !errors.Is(err, fault) || archive.publications != 0 {
		t.Fatal("failed output write reached snapshot publication or lost its error", err)
	}
	_, found, err := disk.Latest(ctx)
	mustEngine(t, err)
	if found {
		t.Fatal("incomplete output batch became a safety point")
	}
	body, err := readHistory(ctx, historyReadArgs{Ref: ref})
	if err != nil || body != "original report" {
		t.Fatal("failed publication lost the readable report", body, err)
	}

}

type historyReadArgs struct {
	Ref string `json:"ref"`
}
