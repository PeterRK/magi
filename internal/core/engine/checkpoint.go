package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"unicode/utf8"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

type snapshot struct {
	FormatVersion        int                  `json:"format_version"`
	RuntimePromptVersion string               `json:"runtime_prompt_version"` // Restored role heads retain their original instructions.
	ProjectID            string               `json:"project_id"`
	Workflow             Workflow             `json:"workflow"`
	Planner              *corecontext.Context `json:"-"` // Encoded through Context.Save below.
	Reviewer             *corecontext.Context `json:"-"`
	Handoff              plannerrole.Handoff  `json:"handoff"`
}

// Serialize role contexts through Save; model APIs and runtime tiers never enter JSON.
func (s snapshot) MarshalJSON() ([]byte, error) {
	var memories [2]json.RawMessage
	for i, window := range []*corecontext.Context{s.Planner, s.Reviewer} {
		if window != nil {
			body, err := window.Save()
			if err != nil {
				return nil, err
			}
			memories[i] = body
		}
	}
	type fields snapshot
	return json.Marshal(struct {
		*fields
		Planner  json.RawMessage `json:"planner"`
		Reviewer json.RawMessage `json:"reviewer"`
	}{(*fields)(&s), memories[0], memories[1]})
}

// Decode each saved Planner/Reviewer Context once, selecting the shared Pro API.
func restoreCheckpoint(body []byte) (snapshot, error) {
	type fields snapshot
	var state snapshot
	saved := struct {
		*fields
		Planner  json.RawMessage `json:"planner"`
		Reviewer json.RawMessage `json:"reviewer"`
	}{fields: (*fields)(&state)}
	if err := protocol.UnmarshalJSON(body, &saved); err != nil {
		return snapshot{}, err
	}
	for _, entry := range []struct {
		saved  json.RawMessage
		target **corecontext.Context
	}{{saved.Planner, &state.Planner}, {saved.Reviewer, &state.Reviewer}} {
		if len(entry.saved) == 0 || string(entry.saved) == "null" {
			continue
		}
		window, err := corecontext.Restore(entry.saved, true)
		if err != nil {
			return snapshot{}, err
		}
		*entry.target = window
	}
	return state, nil
}

func validateCheckpoint(state snapshot) error {
	if state.Workflow.ID == "" || checkInvariants(state.Workflow) != nil {
		return protocol.IntegrityFailure("safety point has no completed control baseline")
	}
	workflow := state.Workflow
	// Explicit recovery can move the initial baseline to a later archive number.
	// A cold baseline has an entry and no role memories or reviewed handoff.
	if state.Planner == nil {
		if workflow.Phase != PhasePlanner || state.Reviewer != nil || state.Handoff.Review != nil || len(state.Handoff.Workers) != 0 {
			return protocol.IntegrityFailure("invalid cold-start safety point")
		}
		if state.Handoff.Entry != "COLD_START" {
			return protocol.IntegrityFailure("cold-start safety point lacks its entry message")
		}
		return nil
	}
	if workflow.Round < 2 {
		return protocol.IntegrityFailure("reviewed safety point precedes its first completed round")
	}
	if err := state.Planner.ValidateMemory(1); err != nil {
		return err
	}
	if state.Reviewer != nil || workflow.Phase != PhaseDormant {
		if err := state.Reviewer.ValidateMemory(1); err != nil {
			return err
		}
	}
	return validateCheckpointHandoff(state)
}

// Validate the material already in the snapshot; history files are read on demand.
func validateCheckpointHandoff(state snapshot) error {
	h := state.Handoff
	if h.Entry != "" {
		return protocol.IntegrityFailure("reviewed safety point retains an entry message")
	}
	seen := map[string]bool{}
	valid := func(ref, body string, role byte) bool {
		if !protocol.ValidHistoryRef(ref) || ref[0] != role || seen[ref] || body == "" || !utf8.ValidString(body) {
			return false
		}
		seen[ref] = true
		return true
	}
	for _, report := range h.Workers {
		if !valid(report.ReportID, report.Body, 'W') || state.Workflow.Phase == PhaseDormant {
			return protocol.IntegrityFailure("invalid Worker handoff material")
		}
		if !protocol.ValidHistoryRef(report.OrderID) || report.OrderID[0] != 'P' || seen[report.OrderID] {
			return protocol.IntegrityFailure("invalid Worker handoff order")
		}
		seen[report.OrderID] = true
	}
	if h.Review == nil || !valid(h.Review.ReviewID, h.Review.Summary, 'R') {
		return protocol.IntegrityFailure("safety point handoff is incomplete")
	}
	return nil
}

// Validate the stored metadata and restore each Context once.
func loadCheckpoint(ctx context.Context, disk protocol.StorageBackend, projectID string, point *protocol.StoredRound) (snapshot, error) {
	state := snapshot{FormatVersion: protocol.StorageFormatVersion, RuntimePromptVersion: prompt.Version, ProjectID: projectID, Workflow: emptyWorkflow()}
	if point != nil {
		body, err := disk.Read(ctx, point.Hash)
		if err != nil {
			return snapshot{}, err
		}
		state, err = restoreCheckpoint(body)
		if err != nil {
			return snapshot{}, protocol.IntegrityFailure("invalid safety point body")
		}
		if state.FormatVersion != protocol.StorageFormatVersion || state.ProjectID != projectID || (!state.Workflow.active()) != point.Dormant || state.Workflow.Round != point.Number+1 {
			return snapshot{}, protocol.IntegrityFailure("safety point metadata differs from its complete body")
		}
		if err = validateCheckpoint(state); err != nil {
			return snapshot{}, err
		}
	}
	// Identify this runtime, not the version of every inherited role head.
	// Saved instructions are preserved and the old runtime version is not a recovery gate.
	state.RuntimePromptVersion = prompt.Version
	return state, nil
}

// commitRound forms the next baseline and captures role memories for publication.
// No intermediate role submission installs a Context snapshot. Errors end this run;
// subsequent execution must reload the durable baseline into a fresh State.
func (s *State) commitRound(ctx context.Context, handoff plannerrole.Handoff, idle bool, planner, reviewer *corecontext.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.state.Workflow.Phase != PhasePlanner {
		return protocol.LifecycleConflict("round publication requires an active workflow")
	}
	if planner == nil || reviewer == nil && !idle {
		return protocol.IntegrityFailure("safety point lacks an executed role context")
	}
	workflow := s.state.Workflow
	workflow.Round++
	if idle {
		workflow.Phase = PhaseDormant
	}
	if err := checkInvariants(workflow); err != nil {
		return err
	}
	s.state.Workflow = workflow
	s.state.Handoff = handoff
	s.state.Planner, s.state.Reviewer = planner.Clone(), nil
	if reviewer != nil {
		s.state.Reviewer = reviewer.Clone()
	}
	number := s.state.Workflow.Round - 1
	if err := s.publish(ctx, number); err != nil {
		return err
	}
	characters.ResetHistory()
	return nil
}

// publish is the active-round boundary: history precedes the snapshot.
func (s *State) publish(ctx context.Context, number uint64) error {
	if err := characters.FlushHistory(ctx, s.disk); err != nil {
		return err
	}
	if err := s.publishSnapshot(ctx, number); err != nil {
		return err
	}
	return s.finishPublication(ctx)
}

// publishSnapshot writes a complete baseline using only this store's backend.
// Recovery uses it without touching active role memories or history.
func (s *State) publishSnapshot(ctx context.Context, number uint64) error {
	body, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	r := protocol.StoredRound{Number: number, Dormant: !s.state.Workflow.active()}
	r, err = s.disk.Publish(ctx, r, body, s.latest)
	if err != nil {
		return err
	}
	s.latest = &r
	return nil
}

func (s *State) finishPublication(ctx context.Context) error {
	if err := s.disk.FlushUsage(ctx); err != nil {
		return fmt.Errorf("safety point committed; usage flush: %w", err)
	}
	if err := s.disk.Prune(ctx); err != nil {
		return fmt.Errorf("safety point committed; pruning: %w", err)
	}
	return nil
}

// RecoveryCandidate pairs a retained safety point with validation of its
// complete control baseline and role memories.
type RecoveryCandidate struct {
	Point protocol.StoredRound
	Err   error
}

func RecoveryCandidates(ctx context.Context, disk protocol.StorageBackend, projectID string) ([]RecoveryCandidate, error) {
	points, err := disk.Retained(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []RecoveryCandidate
	for _, point := range points {
		_, err := loadCheckpoint(ctx, disk, projectID, &point)
		candidates = append(candidates, RecoveryCandidate{Point: point, Err: err})
	}
	return candidates, nil
}

// Recover validates an explicitly selected retained safety point and publishes
// it as a new baseline. The caller holds the project lock; global allocation
// state and workspace effects remain independent of recovery.
func Recover(ctx context.Context, disk protocol.StorageBackend, projectID string, number uint64) error {
	latest, found, err := disk.Latest(ctx)
	if err != nil {
		return err
	}
	if !found {
		return protocol.InvalidCommand("no safety point to recover")
	}
	points, err := disk.Retained(ctx)
	if err != nil {
		return err
	}
	for _, point := range points {
		if point.Number != number {
			continue
		}
		state, err := openPoint(ctx, disk, projectID, &point)
		if err != nil {
			return err
		}
		return state.restoreAsNext(ctx, latest)
	}
	return protocol.InvalidCommand("recovery selection is outside the latest three safety points")
}

func Open(ctx context.Context, disk protocol.StorageBackend, projectID string) (*State, error) {
	r, found, err := disk.Latest(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		return openPoint(ctx, disk, projectID, nil)
	}
	return openPoint(ctx, disk, projectID, &r)
}

func openPoint(ctx context.Context, disk protocol.StorageBackend, projectID string, point *protocol.StoredRound) (*State, error) {
	state, err := loadCheckpoint(ctx, disk, projectID, point)
	if err != nil {
		return nil, err
	}
	return &State{disk: disk, state: state, latest: point}, nil
}

func (s *State) restoreAsNext(ctx context.Context, latest protocol.StoredRound) error {
	if s.latest == nil || s.latest.Number >= latest.Number || latest.Number >= math.MaxInt64-1 {
		return protocol.InvalidCommand("recovery requires an older retained complete safety point")
	}
	workflow, err := rebaseRecoveredWorkflow(s.state.Workflow, latest.Number+2)
	if err != nil {
		return err
	}
	s.state.Workflow = workflow
	if err := s.disk.Prune(ctx); err != nil {
		return err
	}
	// Preserve this disk's allocation highwaters without flushing another
	// runtime's pending history cache into the recovered workspace.
	if err = s.disk.ReserveNumbers(ctx); err != nil {
		return err
	}
	s.latest = &latest
	if err := s.publishSnapshot(ctx, latest.Number+1); err != nil {
		return err
	}
	return s.finishPublication(ctx)
}

// rebaseRecoveredWorkflow places a validated complete baseline after the latest
// archive without changing role memories or pending handoff.
func rebaseRecoveredWorkflow(workflow Workflow, round uint64) (Workflow, error) {
	if round <= workflow.Round {
		return workflow, protocol.IntegrityFailure("recovery must advance archive controls")
	}
	workflow.Round = round
	return workflow, checkInvariants(workflow)
}
