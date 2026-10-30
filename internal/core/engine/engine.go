// Package engine drives complete rounds and publishes their safety points.
package engine

import (
	"context"

	plannerrole "magi/internal/core/characters/planner"
	reviewerrole "magi/internal/core/characters/reviewer"
	"magi/internal/core/protocol"
)

// Engine owns the active workflow and role memories. Process-wide capabilities
// are bound before Drive; all branches must join before replacing those bindings.
type Engine struct {
	workflow *State
	planner  plannerrole.Planner
	reviewer reviewerrole.Reviewer
}

// New restores this driver's private role memories without binding shared resources.
func New(state *State) *Engine {
	e := &Engine{workflow: state}
	if state.state.Planner != nil {
		e.planner.Context = state.state.Planner.Clone()
	}
	if state.state.Reviewer != nil {
		e.reviewer.Context = state.state.Reviewer.Clone()
	}
	return e
}

// Drive returns on any execution or publication error. Restart by loading a new State.
func (e *Engine) Drive(ctx context.Context) (Workflow, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Workflow{}, err
		}
		state := e.workflow.CurrentWorkflow()
		switch state.Phase {
		case PhaseDormant:
			return state, nil
		case PhaseAbsent:
			return Workflow{}, protocol.LifecycleConflict("workflow has not been admitted")
		case PhasePlanner:
			if err := e.runRound(ctx, state); err != nil {
				return Workflow{}, err
			}

		default:
			return Workflow{}, protocol.IntegrityFailure("unsupported active task phase")
		}
	}
}
