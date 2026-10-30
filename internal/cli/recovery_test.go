package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/protocol"
)

func TestStartupFaultRecoveryPrecedesModelAndWorkflowInput(t *testing.T) {
	for _, choice := range []string{"", "999", "0"} {
		t.Run("selection-"+choice, func(t *testing.T) {
			isolateBindings(t)
			ctx, root := context.Background(), t.TempDir()
			t.Chdir(root)
			var out, diagnostic bytes.Buffer
			r := New(&out, &diagnostic)
			noError(t, r.Run(ctx, []string{"init"}))
			// Admission publishes the cold baseline before requesting a model.
			stopped := errors.New("stop before model construction")
			r.modelFactory = func() (protocol.Model, error) { return nil, stopped }
			if err := r.Run(ctx, nil); !errors.Is(err, stopped) {
				t.Fatal(err)
			}
			s := openTestProject(t, ctx, root)
			disk := s.Disk()
			prior, _, err := disk.Latest(ctx)
			noError(t, err)
			// Storage accepts opaque bytes; engine rejects this incomplete baseline.
			_, err = disk.Publish(ctx, protocol.StoredRound{Number: prior.Number + 1}, []byte(`{}`), &prior)
			noError(t, err)
			noError(t, s.Close())
			pending := strings.NewReader("New user requirement must not become recovery control.")
			r.In, r.recoveryInput = pending, strings.NewReader(choice+"\n")
			diagnostic.Reset()
			calls := 0
			r.modelFactory = func() (protocol.Model, error) { calls++; return nil, stopped }
			err = r.Run(ctx, nil)
			if choice == "0" {
				if !errors.Is(err, stopped) || calls != 1 || !strings.Contains(diagnostic.String(), "startup state recheck passed") {
					t.Fatal("recovery did not recheck before model construction", err, calls, diagnostic.String())
				}
			} else if err == nil || calls != 0 || pending.Len() != len("New user requirement must not become recovery control.") {
				t.Fatal("fault consumed task input or constructed a model", err, calls, pending.Len())
			}
		})
	}
}

func TestRecoveredWorkflowContinuesWithoutRollingBackWorkspace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		ctx, root := context.Background(), t.TempDir()
		t.Chdir(root)
		var out, diagnostic bytes.Buffer
		r := New(&out, &diagnostic)
		noError(t, r.Run(ctx, []string{"init"}))
		r.In = strings.NewReader("Ship the task.")
		r.modelFactory = func() (protocol.Model, error) { return workflowModel(t), nil }
		noError(t, r.Run(ctx, nil))
		s := openTestProject(t, ctx, root)
		prior, _, err := s.Disk().Latest(ctx)
		noError(t, err)
		_, err = s.Disk().Publish(ctx, protocol.StoredRound{Number: prior.Number + 1, Dormant: prior.Dormant}, []byte(`{}`), &prior)
		noError(t, err)
		noError(t, s.Close())
		r.In, r.recoveryInput = strings.NewReader(""), strings.NewReader(fmt.Sprint(prior.Number)+"\n")
		r.modelFactory = func() (protocol.Model, error) {
			model := newScriptedModel(map[string][]protocol.Response{
				"PLANNER/interaction": {controlResponse("show_user", showUserArgs{Message: "Recovered work is complete; preparing to sleep."})},
				"PLANNER/planner":     {controlCallResponse("submit_plan", `{"orders":[],"memo":""}`)},
			})
			return model, nil
		}
		out.Reset()
		diagnostic.Reset()
		noError(t, r.Run(ctx, nil))
		if !strings.Contains(out.String(), `"phase": "DORMANT"`) {
			t.Fatal("engine could not continue from explicit recovery", out.String())
		}
		body, err := os.ReadFile(filepath.Join(root, "work.txt"))
		noError(t, err)
		if strings.TrimSpace(string(body)) != "done" {
			t.Fatal("workspace rolled back")
		}
	})
}

func TestRecoveryReadsTerminalSelection(t *testing.T) {
	isolateBindings(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	r := New(&out, &out)
	noError(t, r.Run(ctx, []string{"init"}))
	stopped := errors.New("stop before model construction")
	r.modelFactory = func() (protocol.Model, error) { return nil, stopped }
	if err := r.Run(ctx, nil); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	store := openTestProject(t, ctx, root)
	defer store.Close()
	master, slave := openTestTerminal(t)
	diagnostic := &terminalOutput{ready: make(chan struct{}, 1)}
	r.Err, r.recoveryInput = diagnostic, slave
	input := newStdin(strings.NewReader("untouched task"))
	defer input.close()
	fault := errors.New("startup fault")
	done := make(chan error, 1)
	go func() { done <- r.recoverBeforeEngine(ctx, store, input, fault) }()
	select {
	case <-diagnostic.ready:
		_, err := master.WriteString("invalid\n")
		noError(t, err)
	case err := <-done:
		t.Fatalf("recovery ended before terminal readiness: %v", err)
	case <-ctx.Done():
		t.Fatal("recovery did not prompt for a selection")
	}
	if err := <-done; !errors.Is(err, fault) || !strings.Contains(err.Error(), "invalid recovery selection") {
		t.Fatal(err)
	}
	if text := diagnostic.text(); !strings.Contains(text, "Restore round:") {
		t.Fatalf("missing recovery prompt: %q", text)
	}
}

func TestStartupFailureDoesNotConsumeInputOrCallModel(t *testing.T) {
	isolateBindings(t)
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	r := New(&out, &out)
	pending := strings.NewReader("must remain unread")
	r.In = pending
	calls := 0
	r.modelFactory = func() (protocol.Model, error) {
		calls++
		return nil, nil
	}
	if err := r.Run(context.Background(), nil); err == nil || calls != 0 || pending.Len() != len("must remain unread") {
		t.Fatal("startup failure consumed input or called the model", err)
	}
	if strings.Contains(out.String(), "Restore round:") {
		t.Fatal("offered snapshot recovery without a project")
	}
}
