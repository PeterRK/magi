package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	"magi/internal/core/protocol"
)

type Phase string

const (
	PhaseAbsent  Phase = "ABSENT"
	PhasePlanner Phase = "PLANNER"
	PhaseDormant Phase = "DORMANT"
)

// Workflow contains round-boundary controls. Round-local outputs stay in runRound.
type Workflow struct {
	ID    protocol.WorkflowID `json:"id"`
	Phase Phase               `json:"phase"`
	Round uint64              `json:"round"`
}

func emptyWorkflow() Workflow {
	return Workflow{Phase: PhaseAbsent}
}

func (t Workflow) active() bool {
	return t.Phase == PhasePlanner
}

// plannerBaseline projects the workflow identity used by Planner execution.
func (t Workflow) plannerBaseline() plannerrole.Baseline {
	return plannerrole.Baseline{ID: t.ID, Round: t.Round}
}

func checkInvariants(workflow Workflow) error {
	if workflow.Phase == PhaseAbsent {
		if workflow != emptyWorkflow() {
			return protocol.IntegrityFailure("absent task contains workflow state")
		}
		return nil
	}
	if workflow.ID == "" {
		return protocol.IntegrityFailure("workflow_id is required")
	}
	switch workflow.Phase {
	case PhasePlanner, PhaseDormant:
	default:
		return protocol.IntegrityFailure("unknown workflow phase")
	}
	if workflow.Round == 0 {
		return protocol.IntegrityFailure("workflow lacks round")
	}
	return nil
}

// State owns the completed baseline and checkpoint publication target. Its caller
// must serialize admission, execution and recovery; parallel role branches do not
// access State. External execution capabilities remain shared through protocol.
type State struct {
	disk       protocol.StorageBackend
	state      snapshot
	latest     *protocol.StoredRound
	configured bool
}

// Configure prepares this workspace and binds the remaining shared role
// resources once. The caller holds the project lock and has joined any prior run.
func (s *State) Configure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.configured {
		return nil
	}
	// Finish interrupted pruning before a new publication moves the retention boundary.
	if err := s.disk.Prune(ctx); err != nil {
		return err
	}
	// Binding belongs to explicit startup, never to loading or inspecting workflow state.
	protocol.Storage = s.disk
	characters.ResetHistory()
	s.configured = true
	return nil
}

// Admit selects creation, resumption or wake-up for this workspace. The caller
// holds the project lock and has configured resources, but has not started Drive.
func (s *State) Admit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.configured {
		return protocol.LifecycleConflict("workflow is not configured")
	}
	workflow := s.state.Workflow
	creating := workflow.Phase == PhaseAbsent
	kind := "COLD_START"
	if creating {
		var seed [12]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return err
		}
		workflow.ID = protocol.WorkflowID("task-" + hex.EncodeToString(seed[:]))
		workflow.Round, workflow.Phase = 1, PhasePlanner
	} else {
		if workflow.Phase == PhaseDormant {
			workflow.Phase = PhasePlanner
			kind = "DORMANT_WAKE"
		} else if !workflow.active() {
			return protocol.LifecycleConflict("workflow cannot resume")
		} else if s.state.Planner != nil {
			kind = "INTERRUPTED_RESUME"
		}
	}
	if err := checkInvariants(workflow); err != nil {
		return err
	}
	next := s.state
	next.Workflow = workflow
	next.Handoff.Entry = kind
	s.state = next
	if creating {
		return s.publish(ctx, 0)
	}
	return nil
}

func (s *State) CurrentWorkflow() Workflow { return s.state.Workflow }
