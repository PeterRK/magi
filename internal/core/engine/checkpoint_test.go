package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
	"magi/internal/storage"
)

type failingRoundPreparation struct {
	protocol.ToolExecutor
	err error
}

func (p failingRoundPreparation) PrepareRound(context.Context) error { return p.err }

func TestRoundPreparationFailureStopsBeforePlanner(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s := &State{state: snapshot{Workflow: testBaseline(t), Handoff: coldStartHandoff()}}
	fault := errors.New("round output preparation failed")
	protocol.Tools = failingRoundPreparation{err: fault}
	if _, err := New(s).Drive(ctx); !errors.Is(err, fault) {
		t.Fatal("round preparation failure did not reach the caller", err)
	}
	if s.state.Workflow.Phase != PhasePlanner || s.state.Planner != nil {
		t.Fatal("round advanced after failed preparation")
	}
}

func newTestState(t *testing.T) (*State, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	project, err := storage.InitProject(ctx, t.TempDir())
	mustEngine(t, err)
	t.Cleanup(func() { project.Close() })
	disk := project.Disk()
	s, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	err = s.Configure(ctx)
	mustEngine(t, err)
	return s, disk
}

func testBaseline(t *testing.T) Workflow {
	t.Helper()
	return Workflow{ID: "task", Round: 1, Phase: PhasePlanner}
}

func coldStartHandoff() plannerrole.Handoff { return plannerrole.Handoff{Entry: "COLD_START"} }
func reviewedHandoff() *characters.ReviewReport {
	return &characters.ReviewReport{ReviewID: "R1", Summary: "Continue from the reviewed findings."}
}

// Fixture for complete live handoffs; numbered history stores only report text.
func workerHandoff(ref, orderID, report string) characters.WorkerReport {
	return characters.WorkerReport{ReportID: ref, OrderID: orderID, Body: report}
}

// Establish the actual initial archive before installing a completed round fixture.
func beginReviewedCheckpoint(t *testing.T, s *State) {
	t.Helper()
	s.state.Workflow = testBaseline(t)
	s.state.Handoff = coldStartHandoff()
	mustEngine(t, s.publish(context.Background(), 0))
	s.state.Workflow.Round = 2
	s.state.Planner = testRoleContext(t, protocol.RolePlanner, "retained Planner history")
	s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "retained Reviewer history")
	s.state.Handoff = plannerrole.Handoff{Review: reviewedHandoff()}
}

func TestResumeEntryKeepsOneCurrentStateAndRetainsOtherHandoff(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		reviewed   bool
		dormant    bool
	}{
		{"unfinished first intake", "COLD_START", false, false},
		{"unfinished demand", "INTERRUPTED_RESUME", true, false},
		{"completed demand", "DORMANT_WAKE", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := &State{state: snapshot{Workflow: testBaseline(t), Handoff: coldStartHandoff()}, configured: true}
			// A recovered baseline can have a later archive number even before its first review.
			s.state.Workflow.Round = 7
			if tc.dormant {
				s.state.Workflow.Phase = PhaseDormant
			}
			retained := workerHandoff("W1", "P1", "reviewed account")
			if tc.reviewed {
				s.state.Planner = testRoleContext(t, protocol.RolePlanner, "retained Planner history")
				s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "retained Reviewer history")
				s.state.Handoff = plannerrole.Handoff{Review: reviewedHandoff()}
				if !tc.dormant {
					s.state.Handoff.Workers = []characters.WorkerReport{retained}
				}
			}
			mustEngine(t, validateCheckpoint(s.state))
			original := s.state.Handoff
			for i := range 2 {
				mustEngine(t, s.Admit(ctx))
				kind := tc.kind
				if tc.dormant && i == 1 {
					kind = "INTERRUPTED_RESUME"
				}
				if !reflect.DeepEqual(s.state.Handoff.Workers, original.Workers) || !reflect.DeepEqual(s.state.Handoff.Review, original.Review) || s.state.Handoff.Entry != kind {
					t.Fatal("resume lost supplied material or gave contradictory entry states", s.state.Handoff)
				}
			}
		})
	}
}

func TestSafetyPointRestoresCompleteMemoriesWithoutCallRecords(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Planner = testRoleContext(t, protocol.RolePlanner, "completed Planner prefix")
	s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "independent Reviewer history")
	s.state.Handoff = plannerrole.Handoff{Workers: []characters.WorkerReport{workerHandoff("W1", "P1", "next Planner handoff")}}
	s.state.Handoff.Review = reviewedHandoff()
	ref, err := disk.Allocate(string(protocol.RoleReviewer))
	mustEngine(t, err)
	mustEngine(t, disk.WriteFragment(ctx, ref, []byte("committed original")))
	s.state.Handoff.Workers[0] = workerHandoff("W1", "P1", "next Planner handoff [["+ref+"]]")
	expected := s.state
	mustEngine(t, s.publish(ctx, 1))
	s.state.Handoff = plannerrole.Handoff{}
	s.state.Planner = nil
	orphanRef, err := disk.Allocate(string(protocol.RoleReviewer))
	mustEngine(t, err)
	mustEngine(t, disk.WriteFragment(ctx, orphanRef, []byte("unfinished original")))
	pending, err := characters.CacheHistoryOutput(ctx, protocol.RoleReviewer, "unfinished report")
	mustEngine(t, err)
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	err = restored.Configure(ctx)
	mustEngine(t, err)
	if !jsonEqual(expected, restored.state) {
		t.Fatal("restart depends on unfinished calls or lost complete role context")
	}
	if _, err := readHistory(ctx, historyReadArgs{Ref: pending}); err == nil {
		t.Fatal("restart retained disposable history metadata")
	}
	body, err := disk.ReadFragment(ctx, ref)
	mustEngine(t, err)
	if string(body) != "committed original" {
		t.Fatal("committed original unavailable")
	}
	next, err := disk.Allocate(string(protocol.RoleReviewer))
	mustEngine(t, err)
	if next != "R4" {
		t.Fatal("orphan allocation was reused")
	}
}

func TestCheckpointRejectsIncompleteBaselineAndHandoff(t *testing.T) {
	for _, tc := range []struct {
		name          string
		breakSnapshot func(*testing.T, *snapshot)
	}{
		{"missing Planner", func(_ *testing.T, s *snapshot) { s.Planner = nil }},
		{"missing Reviewer", func(_ *testing.T, s *snapshot) { s.Reviewer = nil }},
		{"missing handoff", func(_ *testing.T, s *snapshot) { s.Handoff = plannerrole.Handoff{} }},
		{"missing review", func(_ *testing.T, s *snapshot) { s.Handoff.Review = nil }},
		{"unexpected entry", func(_ *testing.T, s *snapshot) { s.Handoff.Entry = "COLD_START" }},
		{"noncanonical source", func(_ *testing.T, s *snapshot) { s.Handoff.Workers[0].ReportID = "W01" }},
		{"wrong source role", func(_ *testing.T, s *snapshot) { s.Handoff.Workers[0].ReportID = "P1" }},
		{"duplicate source", func(_ *testing.T, s *snapshot) { s.Handoff.Workers = append(s.Handoff.Workers, s.Handoff.Workers[0]) }},
		{"empty material", func(_ *testing.T, s *snapshot) { s.Handoff.Workers[0].Body = "" }},
		{"wrong review source", func(_ *testing.T, s *snapshot) { s.Handoff.Review.ReviewID = "W2" }},
		{"missing order reference", func(_ *testing.T, s *snapshot) { s.Handoff.Workers[0] = workerHandoff("W1", "", "retained report") }},
		{"wrong order reference", func(_ *testing.T, s *snapshot) { s.Handoff.Workers[0] = workerHandoff("W1", "W1", "retained report") }},
		{"duplicate order reference", func(_ *testing.T, s *snapshot) {
			s.Handoff.Workers = append(s.Handoff.Workers, workerHandoff("W2", "P1", "another report"))
		}},
		{"cold start missing entry", func(t *testing.T, s *snapshot) {
			s.Workflow = testBaseline(t)
			s.Workflow.Round = 2
			s.Planner, s.Reviewer, s.Handoff = nil, nil, plannerrole.Handoff{}
		}},
		{"cold start with memory", func(t *testing.T, s *snapshot) {
			s.Workflow = testBaseline(t)
			s.Workflow.Round = 2
			s.Handoff = coldStartHandoff()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := snapshot{
				Workflow: Workflow{ID: "task", Round: 2, Phase: PhasePlanner},
				Planner:  testRoleContext(t, protocol.RolePlanner, "retained Planner history"),
				Reviewer: testRoleContext(t, protocol.RoleReviewer, "retained Reviewer history"),
				Handoff:  plannerrole.Handoff{Workers: []characters.WorkerReport{workerHandoff("W1", "P1", "retained report")}, Review: reviewedHandoff()},
			}
			mustEngine(t, validateCheckpoint(state))
			tc.breakSnapshot(t, &state)
			if err := validateCheckpoint(state); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
				t.Fatal("incomplete checkpoint accepted", err)
			}
		})
	}
}

// One malformed checkpoint verifies all recovery entrances use validation.
// Detailed context rules and handoff combinations are tested at their owners.
func TestIncompleteSafetyPointFailsRecoveryEntriesWithoutMutation(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	protocol.ModelPro = directModel(func(context.Context, protocol.Request) (protocol.Response, error) {
		t.Fatal("recovery called the model")
		return protocol.Response{}, nil
	})
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Reviewer = nil
	mustEngine(t, s.publish(ctx, 1))
	broken, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	original, err := disk.Read(ctx, broken.Hash)
	mustEngine(t, err)
	if _, err = Open(ctx, disk, "local-project"); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("resume accepted an incomplete checkpoint", err)
	}
	candidates, err := RecoveryCandidates(ctx, disk, "local-project")
	mustEngine(t, err)
	if len(candidates) != 2 {
		t.Fatal("lost retained candidates")
	}
	for _, candidate := range candidates {
		if candidate.Point.Number == 0 {
			mustEngine(t, candidate.Err)
		} else if !protocol.IsCode(candidate.Err, protocol.CodeIntegrityFailure) {
			t.Fatal("candidate inspection accepted an incomplete checkpoint", candidate.Err)
		}
	}
	before, err := disk.Counters(ctx)
	mustEngine(t, err)
	if err = Recover(ctx, disk, "local-project", 1); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("explicit recovery accepted an incomplete checkpoint", err)
	}
	after, err := disk.Counters(ctx)
	mustEngine(t, err)
	latest, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	unchanged, err := disk.Read(ctx, broken.Hash)
	mustEngine(t, err)
	if !reflect.DeepEqual(before, after) || latest.Number != 1 || !bytes.Equal(original, unchanged) {
		t.Fatal("rejected recovery changed state")
	}
	mustEngine(t, Recover(ctx, disk, "local-project", 0))
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	if restored.state.Workflow.Round != 3 || restored.state.Planner != nil || restored.state.Reviewer != nil || (len(restored.state.Handoff.Workers) != 0 || restored.state.Handoff.Review != nil || restored.state.Handoff.Entry != coldStartHandoff().Entry) {
		t.Fatal("recovery lost the relocated cold-start baseline")
	}
}

func TestDormantCheckpointPreservesHandoff(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Workflow.Phase = PhaseDormant
	s.state.Handoff.Review.Summary = "Original completed review."
	mustEngine(t, s.publish(ctx, 1))
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	if !jsonEqual(s.state, restored.state) {
		t.Fatal("validation rewrote the retained dormant baseline")
	}
}

func TestOpenRestoresProContextsBeforeModelBinding(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Planner = testRoleContext(t, protocol.RolePlanner, "planner memory")
	s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "reviewer memory")
	mustEngine(t, s.publish(ctx, 1))
	protocol.ModelPro, protocol.ModelFlash = nil, nil
	var called []string
	modelAPI := func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		called = append(called, input.Messages[0].Content)
		return protocol.Response{}, nil
	}
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	protocol.ModelPro = directModel(modelAPI)
	for _, window := range []*corecontext.Context{restored.state.Planner, restored.state.Reviewer} {
		_, err := window.CallModel(ctx, protocol.Request{})
		mustEngine(t, err)
	}
	if !reflect.DeepEqual(called, []string{string(protocol.RolePlanner), string(protocol.RoleReviewer)}) {
		t.Fatalf("restored contexts lost Pro selection: %v", called)
	}
}

func testRoleContext(t *testing.T, role protocol.Role, content string) *corecontext.Context {
	t.Helper()
	window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 6_000, HardTokenLimit: 7_500, HeadReservedMessages: 1, TailReservedTokens: 65_536}, true)
	mustEngine(t, err)
	for _, message := range []protocol.Message{{Role: "system", Content: string(role)}, {Role: "user", Content: content}} {
		mustEngine(t, window.Append(context.Background(), message))
	}
	return window
}

func TestExplicitRecoveryPublishesNewPointWithoutRollingBackCounters(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Planner = testRoleContext(t, protocol.RolePlanner, "retained prefix")
	s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "reviewed old round")
	s.state.Handoff = plannerrole.Handoff{Workers: []characters.WorkerReport{workerHandoff("W1", "P1", "older handoff")}}
	s.state.Handoff.Review = reviewedHandoff()
	saved := s.state
	saved.Handoff.Workers = slices.Clone(saved.Handoff.Workers)
	point, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	for n := uint64(1); n <= 3; n++ {
		if n > 1 {
			s.state.Workflow, err = rebaseRecoveredWorkflow(s.state.Workflow, n+1)
			mustEngine(t, err)
		}
		s.state.Handoff.Workers[0] = workerHandoff("W1", "P1", "later handoff")
		mustEngine(t, s.publish(ctx, n))
		if n == 1 {
			point, _, err = disk.Latest(ctx)
			mustEngine(t, err)
			saved = s.state
			saved.Handoff.Workers = slices.Clone(saved.Handoff.Workers)
		}
	}
	latest, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	for role, count := range map[string]int{"PLANNER": 12, "WORKER": 23, "REVIEWER": 34, "USER": 45} {
		for range count {
			_, err := disk.Allocate(role)
			mustEngine(t, err)
		}
	}
	mustEngine(t, disk.ReserveNumbers(ctx))
	mustEngine(t, disk.ObserveUsage(string(protocol.RolePlanner), protocol.TokenUsage{Hit: 1, Miss: 2, Output: 3}))
	mustEngine(t, disk.FlushUsage(ctx))
	before, err := disk.Counters(ctx)
	mustEngine(t, err)
	fault := protocol.IntegrityFailure("snapshot unavailable")
	archive := unreadableSnapshot{StorageBackend: disk, hash: latest.Hash, fault: fault}
	if _, err = Open(ctx, archive, "local-project"); !errors.Is(err, fault) {
		t.Fatal("latest failure was lost or silently fell back", err)
	}
	candidates, err := RecoveryCandidates(ctx, archive, "local-project")
	mustEngine(t, err)
	if len(candidates) != 3 || candidates[0].Point != latest || candidates[2].Point != point || !errors.Is(candidates[0].Err, fault) || candidates[2].Err != nil {
		t.Fatal("wrong retained candidates", candidates)
	}
	for _, number := range []uint64{0, latest.Number, latest.Number + 1} {
		if err := Recover(ctx, archive, "local-project", number); err == nil {
			t.Fatalf("accepted unavailable recovery selection %d", number)
		}
	}
	mustEngine(t, Recover(ctx, archive, "local-project", point.Number))
	recovered, err := Open(ctx, archive, "local-project")
	mustEngine(t, err)

	if !sameRoleContext(t, saved.Planner, recovered.state.Planner) || !sameRoleContext(t, saved.Reviewer, recovered.state.Reviewer) {
		t.Fatal("role memory changed")
	}
	if recovered.state.Workflow.Round != latest.Number+2 || !reflect.DeepEqual(saved.Handoff, recovered.state.Handoff) {
		t.Fatal("recovery controls changed or extra notice inserted", recovered.state)
	}
	after, err := disk.Counters(ctx)
	mustEngine(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("counters rolled back: before=%+v after=%+v", before, after)
	}
	points, err := disk.Retained(ctx)
	mustEngine(t, err)
	if points[0].Number != 4 || points[1] != latest {
		t.Fatal("replaced old metadata", points)
	}
	if err = Recover(ctx, archive, "local-project", points[0].Number); err == nil {
		t.Fatal("accepted current safety point as a recovery source")
	}
}

type historyReadTrap struct {
	protocol.StorageBackend
	reads int
}

func (s *historyReadTrap) ReadFragment(context.Context, string) ([]byte, error) {
	s.reads++
	return nil, errors.New("recovery must not read history")
}

func TestRecoveryDoesNotScanHistory(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.Handoff.Workers = []characters.WorkerReport{workerHandoff("W1", "P1", "See [[W9]].")}
	mustEngine(t, s.publish(ctx, 1))
	archive := &historyReadTrap{StorageBackend: disk}
	_, err := Open(ctx, archive, "local-project")
	mustEngine(t, err)
	if archive.reads != 0 {
		t.Fatal("recovery read history", archive.reads)
	}
}

func TestSafetyPointRestoresAcrossRuntimePromptVersionChanges(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	beginReviewedCheckpoint(t, s)
	s.state.RuntimePromptVersion = "previous-prompt-pack"
	s.state.Planner = testRoleContext(t, protocol.RolePlanner, "retained demand and [[P1]]")
	s.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "earlier review doubts [[R1]]")
	s.state.Handoff = plannerrole.Handoff{Workers: []characters.WorkerReport{workerHandoff("W1", "P1", "pending worker report [[W1]]")}}
	s.state.Handoff.Review = reviewedHandoff()
	expected := s.state
	mustEngine(t, s.publish(ctx, 1))
	point, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	raw, err := disk.Read(ctx, point.Hash)
	mustEngine(t, err)
	restored, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	if !reflect.DeepEqual(restored.state.Workflow, expected.Workflow) || !sameRoleContext(t, restored.state.Planner, expected.Planner) || !sameRoleContext(t, restored.state.Reviewer, expected.Reviewer) || !reflect.DeepEqual(restored.state.Handoff, expected.Handoff) {
		t.Fatal("loading changed the saved workflow, role memories or handoff")
	}
	if restored.state.RuntimePromptVersion != prompt.Version {
		t.Fatal("new run kept old prompt version")
	}
	after, err := disk.Read(ctx, point.Hash)
	mustEngine(t, err)
	if string(after) != string(raw) {
		t.Fatal("restore modified the original safety point")
	}
}

func TestRecoveryCandidateInspectionPreservesWorkingHistory(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	store, disk := newTestState(t)
	store.state.Workflow = testBaseline(t)
	store.state.Handoff = coldStartHandoff()
	mustEngine(t, store.publish(ctx, 0))
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RoleWorker, "working result")
	mustEngine(t, err)
	candidates, err := RecoveryCandidates(ctx, disk, "local-project")
	mustEngine(t, err)
	if len(candidates) != 1 || candidates[0].Err != nil {
		t.Fatal(candidates)
	}
	body, err := readHistory(ctx, historyReadArgs{Ref: ref})
	if err != nil || body != "working result" {
		t.Fatal("read-only recovery inspection discarded working history", body, err)
	}
}

// Loading, validation and explicit recovery operate on their supplied disk even
// while another workspace's role bindings remain installed. Execution is still
// limited to one active workspace; these operations must not activate another.
func TestLoadingAndRecoveryPreserveActiveRoleResources(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	archive, disk := newTestState(t)
	archive.state.Workflow = testBaseline(t)
	archive.state.Handoff = coldStartHandoff()
	mustEngine(t, archive.publish(ctx, 0))
	var err error
	archive.state.Workflow, err = rebaseRecoveredWorkflow(archive.state.Workflow, 2)
	mustEngine(t, err)
	mustEngine(t, archive.publish(ctx, 1))
	reserved, err := disk.Allocate(string(protocol.RolePlanner))
	mustEngine(t, err)

	active, activeDisk := newTestState(t)
	active.state.Workflow = testBaseline(t)
	active.state.Handoff = coldStartHandoff()
	mustEngine(t, active.publish(ctx, 0))
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, "active unpublished original")
	mustEngine(t, err)
	nextID, err := strconv.ParseUint(characters.NextID(), 10, 64)
	mustEngine(t, err)
	unchanged := func() {
		t.Helper()
		if protocol.Storage != activeDisk {
			t.Fatal("inspection or recovery rebound the active storage")
		}
		body, err := readHistory(ctx, historyReadArgs{Ref: ref})
		if err != nil || body != "active unpublished original" {
			t.Fatal("inspection or recovery changed active history")
		}
		nextID++
		if got := characters.NextID(); got != strconv.FormatUint(nextID, 10) {
			t.Fatal("inspection or recovery reset active IDs", got)
		}
		if _, err := activeDisk.ReadFragment(ctx, ref); err == nil {
			t.Fatal("inspection or recovery flushed active history")
		}
		if _, err := disk.ReadFragment(ctx, ref); err == nil {
			t.Fatal("active history escaped into the inspected workspace")
		}
	}
	unchanged()
	loaded, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	_ = New(loaded)
	unchanged()
	// A valid archive with the wrong project identity fails after reading it.
	if _, err := Open(ctx, disk, "different-project"); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("mismatched project accepted", err)
	}
	unchanged()
	candidates, err := RecoveryCandidates(ctx, disk, "local-project")
	mustEngine(t, err)
	if len(candidates) != 2 || candidates[0].Err != nil || candidates[1].Err != nil {
		t.Fatal("retained candidates changed", candidates)
	}
	unchanged()
	if err := Recover(ctx, disk, "local-project", 99); err == nil {
		t.Fatal("unretained recovery accepted")
	}
	unchanged()
	mustEngine(t, Recover(ctx, disk, "local-project", 0))
	unchanged()
	latest, found, err := disk.Latest(ctx)
	mustEngine(t, err)
	if !found || latest.Number != 2 {
		t.Fatal("recovery did not publish on its own disk", latest)
	}
	counters, err := disk.Counters(ctx)
	mustEngine(t, err)
	if reserved != "P1" || counters.PlannerNumber != 1 {
		t.Fatal("recovery lost its disk's allocated number highwater")
	}
	// Explicit startup, unlike loading, switches the active role resources.
	recovered, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	err = recovered.Configure(ctx)
	mustEngine(t, err)
	if protocol.Storage != disk || characters.NextID() != strconv.FormatUint(nextID+1, 10) {
		t.Fatal("startup changed IDs or failed to activate the recovered storage")
	}
	if _, err := readHistory(ctx, historyReadArgs{Ref: ref}); err == nil {
		t.Fatal("startup retained the previous working history")
	}
}

func TestStatePublicationUsesItsOwnBackend(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	store, disk := newTestState(t)
	store.state.Workflow = testBaseline(t)
	store.state.Handoff = coldStartHandoff()
	ref, err := characters.CacheHistoryOutput(ctx, protocol.RoleWorker, "complete report")
	mustEngine(t, err)
	// A shared role binding is not the dependency of this State. The nil
	// embedded backend deliberately fails if publication consults that binding.
	protocol.Storage = usageStorageTrap{}
	mustEngine(t, store.publish(ctx, 0))
	body, err := disk.ReadFragment(ctx, ref)
	mustEngine(t, err)
	latest, found, err := disk.Latest(ctx)
	mustEngine(t, err)
	if string(body) != "complete report" || !found || latest.Number != 0 {
		t.Fatal("publication lost the owned history or snapshot")
	}
}

type usageStorageTrap struct{ protocol.StorageBackend }

func TestEngineRestoresIndependentRoleMemories(t *testing.T) {
	ctx := context.Background()
	store := &State{}
	store.state.Planner = testRoleContext(t, protocol.RolePlanner, "planner baseline")
	store.state.Reviewer = testRoleContext(t, protocol.RoleReviewer, "reviewer baseline")
	driver := New(store)
	for _, pair := range [][2]*corecontext.Context{
		{store.state.Planner, driver.planner.Context},
		{store.state.Reviewer, driver.reviewer.Context},
	} {
		before, err := pair[0].Save()
		mustEngine(t, err)
		if !sameRoleContext(t, pair[0], pair[1]) {
			t.Fatal("driver lost restored role memory")
		}
		mustEngine(t, pair[1].Append(ctx, protocol.Message{Role: "user", Content: "only this driver"}))
		after, err := pair[0].Save()
		mustEngine(t, err)
		if !bytes.Equal(before, after) {
			t.Fatal("driver mutated the saved role memory")
		}
	}
}

func TestAdmitCreatesDurableBaselineAndResumesWithoutPublishing(t *testing.T) {
	isolateBindings(t)
	ctx := context.Background()
	s, disk := newTestState(t)
	mustEngine(t, s.Admit(ctx))
	workflow := s.CurrentWorkflow()
	if workflow.ID == "" || workflow.Round != 1 || workflow.Phase != PhasePlanner {
		t.Fatalf("invalid admitted workflow: %+v", s.state.Workflow)
	}
	point, found, err := disk.Latest(ctx)
	mustEngine(t, err)
	if !found || point.Number != 0 {
		t.Fatalf("missing initial baseline: %+v", point)
	}
	if len(s.state.Handoff.Workers) != 0 || s.state.Handoff.Review != nil || s.state.Handoff.Entry != "COLD_START" {
		t.Fatal("entry receipt differs from admission", s.state.Handoff)
	}
	// Resume updates only memory until the next completed round.
	mustEngine(t, s.Admit(ctx))
	if s.CurrentWorkflow() != workflow {
		t.Fatal("resume replaced workflow or changed the round")
	}
	latest, _, err := disk.Latest(ctx)
	mustEngine(t, err)
	if latest != point {
		t.Fatal("resume prematurely published role state")
	}
	reopened, err := Open(ctx, disk, "local-project")
	mustEngine(t, err)
	if reopened.CurrentWorkflow() != workflow {
		t.Fatal("reopen depended on uncommitted entry")
	}
}

func TestAdmitRejectsCancelledAndUnconfiguredEntry(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			ctx := context.Background()
			s := &State{state: snapshot{Workflow: emptyWorkflow()}}
			if configured {
				s.configured = true
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if err := s.Admit(ctx); (configured && !errors.Is(err, context.Canceled)) || (!configured && !protocol.IsCode(err, protocol.CodeLifecycleConflict)) {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if s.state.Workflow != emptyWorkflow() || s.latest != nil {
				t.Fatal("rejected entry changed workflow or published a baseline")
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(left) == string(right)
}

// Faults belong to the storage boundary; its own tests cover filesystem mechanics.
type publicationFault struct {
	protocol.StorageBackend
	stage        string
	fault        error
	publications int
	prunes       int
}

func (s *publicationFault) Publish(ctx context.Context, point protocol.StoredRound, body []byte, prior *protocol.StoredRound) (protocol.StoredRound, error) {
	s.publications++
	if s.stage == "snapshot" {
		return protocol.StoredRound{}, s.fault
	}
	return s.StorageBackend.Publish(ctx, point, body, prior)
}
func (s *publicationFault) WriteFragment(ctx context.Context, ref string, body []byte) error {
	if s.stage == "history" {
		return s.fault
	}
	return s.StorageBackend.WriteFragment(ctx, ref, body)
}
func (s *publicationFault) FlushUsage(ctx context.Context) error {
	if s.stage == "usage" {
		return s.fault
	}
	return s.StorageBackend.FlushUsage(ctx)
}
func (s *publicationFault) Prune(ctx context.Context) error {
	s.prunes++
	if s.stage == "prune" {
		return s.fault
	}
	return s.StorageBackend.Prune(ctx)
}

func TestPublicationFailureEndsRunAndReopensCommittedBaseline(t *testing.T) {
	isolateBindings(t)
	for _, creating := range []bool{true, false} {
		for _, stage := range []string{"snapshot", "usage", "prune"} {
			t.Run(fmt.Sprintf("creating=%t/%s", creating, stage), func(t *testing.T) {
				ctx := context.Background()
				s, disk := newTestState(t)
				if !creating {
					initializeWorkflow(t, s)
					bindModel(&scriptedModel{lanes: map[string][]protocol.Response{
						"PLANNER/planner":     {planResponse(plannerPlanDraft{})},
						"PLANNER/interaction": {showStep("Done.", false)},
					}})
					setDisplay(func(string) error { return nil })
					setInput(nil)
				}
				fault := errors.New("injected " + stage + " failure")
				archive := &publicationFault{StorageBackend: disk, stage: stage, fault: fault}
				s.disk = archive
				if creating {
					err := s.Admit(ctx)
					if !errors.Is(err, fault) {
						t.Fatal("creation lost publication error", err)
					}
				} else {
					result, err := New(s).Drive(ctx)
					if !errors.Is(err, fault) || result.Phase == PhaseDormant {
						t.Fatal("failed publication reported completion", result, err)
					}
				}
				if archive.publications != 1 {
					t.Fatal("publication was retried", archive.publications)
				}
				restored, err := Open(ctx, archive, "local-project")
				mustEngine(t, err)
				committed := stage != "snapshot"
				if creating {
					if (restored.state.Workflow.Phase != PhaseAbsent) != committed {
						t.Fatal("wrong creation baseline", restored.state.Workflow)
					}
				} else if (restored.state.Workflow.Phase == PhaseDormant) != committed {
					t.Fatal("wrong completed-round baseline", restored.state.Workflow)
				}
				if stage == "prune" {
					oldBinding := protocol.Storage
					if err := restored.Configure(ctx); !errors.Is(err, fault) {
						t.Fatal("startup bypassed failed pruning", err)
					}
					if restored.configured || protocol.Storage != oldBinding {
						t.Fatal("failed startup activated resources")
					}
				}
				archive.stage = ""
				beforePrunes := archive.prunes
				mustEngine(t, restored.Configure(ctx))
				if archive.prunes != beforePrunes+1 || archive.publications != 1 {
					t.Fatal("startup did not prune without republishing")
				}
				// Admission reuses a committed workflow; a failed initial publication creates anew.
				mustEngine(t, restored.Admit(ctx))
				if committed && restored.CurrentWorkflow().ID != s.CurrentWorkflow().ID {
					t.Fatal("restart replaced the committed workflow")
				}
			})
		}
	}
}

// Fail at the storage interface by its opaque snapshot key, without filesystem edits.
type unreadableSnapshot struct {
	protocol.StorageBackend
	hash  string
	fault error
}

func (s unreadableSnapshot) Read(ctx context.Context, hash string) ([]byte, error) {
	if hash == s.hash {
		return nil, s.fault
	}
	return s.StorageBackend.Read(ctx, hash)
}
