package cli

import (
	"context"
	"errors"
	"os"

	"magi/internal/core/engine"
	"magi/internal/core/protocol"
	"magi/internal/model"
	"magi/internal/storage"
)

// Keep the project open and locked from self-check through the final result.
// Contexts retain their model tier; shared APIs are bound only after admission.
func (r *Runner) launch(ctx context.Context) (returnErr error) {
	ctx, interrupt := context.WithCancel(ctx)
	defer interrupt()
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	unlock, err := storage.Lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	disk, err := storage.OpenProject(ctx, root)
	if err != nil {
		return err
	}
	store := &workspaceState{Project: disk}
	defer func() { returnErr = errors.Join(returnErr, store.Close()) }()
	input := newStdin(r.In)
	defer input.close()
	if err = store.checkState(ctx); err != nil {
		if err = r.recoverBeforeEngine(ctx, store, input, err); err != nil {
			return err
		}
	}
	var body []byte
	if !input.terminal {
		body, err = input.readAll(ctx)
		if err != nil {
			return err
		}
		if len(body) > 0 {
			if err = validateInputText(body); err != nil {
				return err
			}
		}
	}
	if err = store.configure(ctx); err != nil {
		return err
	}
	if err = store.state.Admit(ctx); err != nil {
		return err
	}
	var adapter protocol.Model
	if r.modelFactory != nil {
		adapter, err = r.modelFactory()
	} else {
		adapter, err = model.FromEnv()
		if err == nil {
			configureDebug()
		}
	}
	if err != nil {
		return err
	}
	if adapter == nil {
		return protocol.InvalidCommand("model adapter is required")
	}
	bindModel(adapter)
	userInput, err := newWorkflowInput(ctx, input, body, r.Err, interrupt)
	if err != nil {
		return err
	}
	protocol.UserMessages, protocol.ShowUser = userInput.messages, userInput.ShowUser
	// Stop input before writing the final result.
	driven, err := func() (workflow engine.Workflow, runErr error) {
		defer func() { runErr = errors.Join(runErr, userInput.close()) }()
		for {
			workflow, runErr = store.engine.Drive(ctx)
			if runErr != nil || userInput.finish() {
				return workflow, runErr
			}
			// Input arrived after the last interaction. Keep capture alive and
			// let Planner consume it in the next admitted round.
			if runErr = store.state.Admit(ctx); runErr != nil {
				return workflow, runErr
			}
		}
	}()
	if err != nil {
		return err
	}
	return writeJSON(r.Out, driven)
}
