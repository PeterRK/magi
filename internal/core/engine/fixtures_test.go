package engine

import (
	"context"
	"fmt"
	"testing"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
	"magi/internal/storage"
)

type testProject struct {
	*storage.Project
	Round *State
}

func openTestProject(ctx context.Context, root string) (*testProject, error) {
	disk, err := storage.InitProject(ctx, root)
	if err != nil {
		return nil, err
	}
	round, err := Open(ctx, disk.Disk(), disk.Metadata().ProjectID)
	if err != nil {
		_ = disk.Close()
		return nil, err
	}

	return &testProject{Project: disk, Round: round}, nil
}

// Role fixtures start from a fixed initial archive; admission itself is tested
// through State.Admit, including publication failures and wake-up.
func initializeWorkflow(t *testing.T, state *State) Workflow {
	t.Helper()
	state.state.Workflow = testBaseline(t)
	state.state.Handoff = coldStartHandoff()
	mustEngine(t, state.publish(context.Background(), 0))
	return state.state.Workflow
}

func mustEngine(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Global protocol bindings model the single active workspace. These tests must
// stay serial, and any goroutines must finish before test cleanup restores them.
func isolateBindings(t *testing.T) {
	t.Helper()
	pro, flash := protocol.ModelPro, protocol.ModelFlash
	storage, tools := protocol.Storage, protocol.Tools
	userMessages, showUser, observer := protocol.UserMessages, protocol.ShowUser, protocol.Observer
	characters.ResetHistory()
	t.Cleanup(func() {
		protocol.ModelPro, protocol.ModelFlash = pro, flash
		protocol.Storage, protocol.Tools = storage, tools
		protocol.UserMessages, protocol.ShowUser, protocol.Observer = userMessages, showUser, observer
		characters.ResetHistory()
	})
}

func sameRoleContext(t *testing.T, a, b *corecontext.Context) bool {
	t.Helper()
	if a == nil || b == nil {
		return a == b
	}
	left, err := a.Save()
	mustEngine(t, err)
	right, err := b.Save()
	mustEngine(t, err)
	return string(left) == string(right)
}

func openTestRuntime(ctx context.Context, root string, adapter protocol.Model) (*testProject, error) {
	app, err := openTestProject(ctx, root)
	if err != nil {
		return nil, err
	}
	err = app.Round.Configure(ctx)
	if err != nil {
		app.Close()
		return nil, err
	}
	protocol.Tools = emptyTools{}
	bindModel(adapter)
	return app, nil
}

func bindModel(model protocol.Model) {
	protocol.ModelPro = characters.ContextModel(model)
	protocol.ModelFlash = protocol.ModelPro
}

type emptyTools struct{}

func (emptyTools) PrepareRound(context.Context) error { return nil }

func (emptyTools) Execute(context.Context, protocol.ToolRequest) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, fmt.Errorf("unexpected external tool call")
}

type plannerPlanDraft struct {
	Orders []string `json:"orders"`
	Memo   string   `json:"memo"`
}

type reviewDraft struct {
	Summary string `json:"summary"`
}

type workerJobFinish struct {
	Report string `json:"report"`
}

func cacheOrders(ctx context.Context, orders []characters.WorkOrder) error {
	for i := range orders {
		ref, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, orders[i].Instruction)
		if err != nil {
			return err
		}
		orders[i].OrderID = ref
	}
	return nil
}

func setInput(input <-chan string)       { protocol.UserMessages = input }
func setDisplay(show func(string) error) { protocol.ShowUser = show }

type directModel func(context.Context, protocol.Request) (protocol.Response, error)

func (f directModel) Invoke(ctx context.Context, r protocol.Request) (protocol.Response, error) {
	return f(ctx, r)
}
func (directModel) InputCapacity() int { return 23552 }
