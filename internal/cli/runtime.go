package cli

import (
	"context"
	"os"
	"os/exec"

	"magi/internal/core/characters"
	"magi/internal/core/engine"
	"magi/internal/core/protocol"
	"magi/internal/debug"
	"magi/internal/storage"
	"magi/internal/tools"
)

type workspaceState struct {
	*storage.Project
	state  *engine.State
	engine *engine.Engine
}

func (s *workspaceState) checkState(ctx context.Context) error {
	if err := s.ValidateStateDirectory(); err != nil {
		return err
	}
	state, err := engine.Open(ctx, s.Disk(), s.Metadata().ProjectID)
	if err != nil {
		return err
	}
	s.state = state
	return nil
}

func (s *workspaceState) configure(ctx context.Context) error {
	state := s.state
	// Discover once at startup; tools retains these absolute paths for this run.
	bash, err := exec.LookPath("bash")
	if err != nil {
		return err
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	toolRuntime, err := tools.NewRuntime(s.Root(), tools.ExecutablePaths{Bash: bash, Git: git})
	if err != nil {
		return err
	}
	err = state.Configure(ctx)
	if err != nil {
		return err
	}
	protocol.Tools = toolRuntime
	s.engine = engine.New(state)
	return nil
}

func bindModel(provider protocol.Model) {
	protocol.ModelPro = characters.ContextModel(provider)
	protocol.ModelFlash = protocol.ModelPro
}

func configureDebug() {
	protocol.Observer = nil
	if os.Getenv("MAGI_DEBUG_API_DIR") != "" {
		protocol.Observer = debug.Observer{}
	}
}
