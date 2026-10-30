package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/engine"
	"magi/internal/core/protocol"
	"magi/internal/model"
)

func TestCLIDeliversAndSleepsWithoutAcknowledgementThenAsksNextDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		ctx, root := context.Background(), t.TempDir()
		t.Chdir(root)
		var out, errout bytes.Buffer
		r := New(&out, &errout)

		noError(t, r.Run(ctx, []string{"init"}))
		out.Reset()
		r.In = strings.NewReader("Please ship a reviewed delivery.")
		r.modelFactory = func() (protocol.Model, error) {
			return workflowModel(t), nil
		}
		noError(t, r.Run(ctx, nil))
		var result engine.Workflow
		noError(t, json.Unmarshal(out.Bytes(), &result))
		if result.Phase != engine.PhaseDormant {
			t.Fatal("no durable reviewed dormancy", result)
		}
		if strings.Count(errout.String(), "The reviewed work is ready.") != 1 {
			t.Fatal("delivery was not displayed exactly once", errout.String())
		}
		wake := newScriptedModel(map[string][]protocol.Response{
			"PLANNER/interaction": {controlResponse("show_user", showUserArgs{Message: "What next demand?", WaitForReply: true})},
			"PLANNER/planner":     {controlCallResponse("submit_plan", `{"orders":[],"memo":""}`)},
		})
		r.modelFactory = func() (protocol.Model, error) { return wake, nil }
		r.In = strings.NewReader("")
		out.Reset()
		wakeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := r.Run(wakeCtx, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("wake did not wait for the next demand", err)
		}
		if !strings.Contains(errout.String(), "What next demand?") {
			t.Fatal("wake question missing")
		}

	})
}

func TestPlainMagiCreatesAndContinuesOneWorkflowAfterExplicitInit(t *testing.T) {
	isolateBindings(t)
	ctx, root := context.Background(), t.TempDir()
	t.Chdir(root)
	var output bytes.Buffer
	runner := New(&output, &output)

	if err := runner.Run(ctx, nil); err == nil {
		t.Fatal("plain magi created .magi without explicit init")
	}
	output.Reset()
	if err := runner.Run(ctx, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runner.Run(ctx, nil); !errors.Is(err, model.ErrMissingKey) {
		t.Fatal(err)
	}
	store := openTestProject(t, ctx, root)
	first := store.state.CurrentWorkflow()
	_, found, err := store.Disk().Latest(ctx)
	if err != nil || !found {
		t.Fatalf("cold-start workflow=%q found=%t err=%v", first.ID, found, err)
	}
	if first.ID == "" || first.Round != 1 || first.Phase != engine.PhasePlanner {
		t.Fatalf("cold-start task=%+v", first)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err = runner.Run(ctx, nil); !errors.Is(err, model.ErrMissingKey) {
		t.Fatal(err)
	}
	store = openTestProject(t, ctx, root)
	resumed := store.state.CurrentWorkflow()
	if resumed != first {
		t.Fatalf("resumed task=%+v", resumed)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureDebugOptIn(t *testing.T) {
	isolateBindings(t)
	for _, dir := range []string{"", "relative", filepath.Join(t.TempDir(), "probe")} {
		t.Run(dir, func(t *testing.T) {
			t.Setenv("MAGI_DEBUG_API_DIR", dir)
			t.Setenv("MAGI_DEBUG_API_PAUSE", "")
			configureDebug()
			if dir == "" {
				t.Setenv("MAGI_DEBUG_API_PAUSE", "invalid-leftover")
				configureDebug()
				if protocol.Observer != nil {
					t.Fatal("debug enabled without a directory")
				}
				return
			}
			if protocol.Observer == nil {
				t.Fatal("explicit debug setting was not wired")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("wiring created a probe", err)
			}

		})
	}
}

func TestStartupBindsModelsAfterAdmission(t *testing.T) {
	isolateBindings(t)
	for _, mode := range []string{"ready", "missing", "failed"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, root := context.Background(), t.TempDir()
				t.Chdir(root)
				r := New(io.Discard, io.Discard)

				noError(t, r.Run(ctx, []string{"init"}))
				previousPro, previousFlash := protocol.ModelPro, protocol.ModelFlash
				t.Cleanup(func() { protocol.ModelPro, protocol.ModelFlash = previousPro, previousFlash })
				var staleCalls atomic.Int32
				stale := func(context.Context, protocol.Request) (protocol.Response, error) {
					staleCalls.Add(1)
					return protocol.Response{}, errors.New("previous run's model called during startup")
				}
				protocol.ModelPro, protocol.ModelFlash = directModel(stale), directModel(stale)
				factoryCalls := 0
				failure := errors.New("model configuration failed")
				r.modelFactory = func() (protocol.Model, error) {
					factoryCalls++
					point, found, err := protocol.Storage.Latest(ctx)
					if err != nil || !found || point.Number != 0 {
						t.Fatal("model configured before cold-start admission", point, err)
					}
					switch mode {
					case "missing":
						return nil, nil
					case "failed":
						return nil, failure
					default:
						return workflowModel(t), nil
					}
				}
				r.In = strings.NewReader("Ship the reviewed task.")
				err := r.Run(ctx, nil)
				if mode == "ready" && err != nil || mode == "missing" && !protocol.IsCode(err, protocol.CodeInvalidCommand) || mode == "failed" && !errors.Is(err, failure) {
					t.Fatal("startup did not preserve the model binding boundary", mode, err)
				}
				if factoryCalls != 1 || staleCalls.Load() != 0 {
					t.Fatal("startup reused stale APIs or configured repeatedly", factoryCalls, staleCalls.Load())
				}
			})
		})
	}
}
