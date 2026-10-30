package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"magi/internal/core/protocol"
)

func TestRuntimeUsesBoundWorkspace(t *testing.T) {
	root := t.TempDir()
	noError(t, os.Mkdir(filepath.Join(root, "child"), 0700))
	alias := filepath.Join(t.TempDir(), "project")
	noError(t, os.Symlink(root, alias))
	t.Chdir(filepath.Dir(alias))
	runtime, err := NewRuntime("project", ExecutablePaths{})
	noError(t, err)
	// All providers must retain the canonical root even after cwd changes.
	t.Chdir(t.TempDir())
	noError(t, runtime.PrepareRound(context.Background()))
	for i, workdir := range []string{".", "child"} {
		request := protocol.ToolRequest{ID: fmt.Sprintf("cwd-%d", i), Name: "bash"}
		request.Arguments = json.RawMessage(`{"command":"pwd -P","workdir":"` + workdir + `"}`)
		result, err := runtime.Execute(context.Background(), request)
		noError(t, err)
		if result.Status != protocol.ToolSucceeded || receiptFields(t, result.Body)["stdout_tail"] != filepath.Join(root, workdir)+"\n" {
			t.Fatalf("tool used the wrong workspace: %+v", result)
		}
	}
	request := protocol.ToolRequest{ID: "write", Name: "write"}
	request.Arguments = json.RawMessage(`{"path":"result.txt","content":"shared root\n"}`)
	_, err = runtime.Execute(context.Background(), request)
	noError(t, err)
	request.Name, request.ID = "read", "read"
	request.Arguments = json.RawMessage(`{"path":"result.txt"}`)
	result, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	if !strings.Contains(string(result.Body), "shared root") {
		t.Fatal("read and write used different roots", result)
	}
}

func TestRuntimeRejectsRelativeExecutablePaths(t *testing.T) {
	for _, paths := range []ExecutablePaths{{Bash: "bash"}, {Git: "bin/git"}} {
		if _, err := NewRuntime(t.TempDir(), paths); err == nil {
			t.Fatal("accepted an executable path that depends on cwd or PATH", paths)
		}
	}
}

func TestRuntimeDoesNotInvokeProviderAfterCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			root := t.TempDir()
			runtime, err := NewRuntime(root, ExecutablePaths{})
			noError(t, err)
			got, err := runtime.Execute(ctx, protocol.ToolRequest{ID: "cancelled", Name: "bash", Arguments: json.RawMessage(`{"command":"touch marker"}`)})
			if _, statErr := os.Stat(filepath.Join(root, "marker")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("command started after cancellation", statErr)
			}
			if !errors.Is(err, want) || (got.Status == protocol.ToolTimedOut) != deadline || (got.Status == protocol.ToolInterrupted) == deadline {
				t.Fatalf("interruption lost: %+v %v", got, err)
			}
		})
	}
}

func TestBuiltinJSONReceiptsPreserveSourceText(t *testing.T) {
	root := t.TempDir()
	noError(t, os.WriteFile(filepath.Join(root, "sample.txt"), []byte("{\"text\":\"中文\"}\nsecond\n"), 0600))
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	for _, tc := range []struct{ name, args, field, want string }{
		{"read", `{"path":"sample.txt"}`, "content", "1: {\"text\":\"中文\"}\n2: second\n"},
		{"bash", `{"command":"cat sample.txt; printf warning >&2"}`, "stdout_tail", "{\"text\":\"中文\"}\nsecond\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runtime.Execute(context.Background(), protocol.ToolRequest{ID: tc.name, Name: tc.name, Arguments: json.RawMessage(tc.args)})
			noError(t, err)
			if result.Status != protocol.ToolSucceeded || receiptFields(t, result.Body)[tc.field] != tc.want {
				t.Fatalf("receipt differs:\ngot: %s\nwant: %s", result.Body, tc.want)
			}
		})
	}
}

func receiptFields(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("invalid JSON receipt: %s: %v", body, err)
	}
	return fields
}

// Real commands must overlap even though each writes to the workspace.
func TestRuntimeDoesNotSerializeIndependentWrites(t *testing.T) {
	records := make(chan protocol.ToolDiagnostic, 8)
	observeTools(t, records)
	root := t.TempDir()
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var group sync.WaitGroup
	for i := range 8 {
		group.Go(func() {
			command := fmt.Sprintf("touch %d.ready; while true; do files=(*.ready); if [ ${#files[@]} -eq 8 ]; then break; fi; sleep 0.01; done", i)
			args, _ := json.Marshal(bashArgs{Command: command})
			result, err := runtime.Execute(ctx, protocol.ToolRequest{ID: fmt.Sprint(i), Name: "bash", Arguments: args})
			if err != nil || result.Status != protocol.ToolSucceeded {
				t.Errorf("independent write did not complete: %+v %v", result, err)
			}
		})
	}
	group.Wait()
	if len(records) != 8 {
		t.Fatalf("missing concurrent tool diagnostics: %d", len(records))
	}
}

type toolObservation chan protocol.ToolDiagnostic

func (toolObservation) NotifyReplyRequired() {}

func (toolObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (toolObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (toolObservation) DumpContext(context.Context, string, []byte) {}

func (o toolObservation) RecordToolExecution(_ context.Context, record *protocol.ToolDiagnostic) {
	o <- *record
}

func observeTools(t *testing.T, records chan protocol.ToolDiagnostic) {
	t.Helper()
	previous := protocol.Observer
	protocol.Observer = toolObservation(records)
	t.Cleanup(func() { protocol.Observer = previous })
}

func TestRuntimeToolDiagnostics(t *testing.T) {
	records := make(chan protocol.ToolDiagnostic, 3)
	observeTools(t, records)
	root := t.TempDir()
	noError(t, os.WriteFile(filepath.Join(root, "sample"), []byte("source text"), 0600))
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	for _, tc := range []struct {
		name, args string
		status     protocol.ToolCallStatus
	}{
		{"read", `{"path":"sample"}`, protocol.ToolSucceeded},
		{"read", `{"path":"missing"}`, protocol.ToolFailed},
		{"bash", `{"command":"pwd","workdir":"/"}`, protocol.ToolFailed},
	} {
		request := protocol.ToolRequest{ID: "call", Name: tc.name, Arguments: json.RawMessage(tc.args)}
		before := time.Now()
		result, err := runtime.Execute(context.Background(), request)
		if tc.status == protocol.ToolSucceeded {
			noError(t, err)
			if !strings.Contains(string(result.Body), "source text") || strings.Contains(string(result.Body), "duration_ms") {
				t.Fatalf("diagnostics changed the receipt: %s", result.Body)
			}
		} else if err == nil {
			t.Fatal("diagnostics swallowed the tool error")
		}
		select {
		case record := <-records:
			if record.CallID != request.ID || record.Tool != tc.name || record.Status != tc.status || record.Started.Before(before) || record.Ended.Before(record.Started) || record.DurationMS < 0 {
				t.Fatalf("incorrect execution diagnostic: %+v", record)
			}
		default:
			t.Fatal("missing execution diagnostic")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runtime.Execute(ctx, protocol.ToolRequest{ID: "unstarted", Name: "read"})
	if !errors.Is(err, context.Canceled) || len(records) != 0 {
		t.Fatal("unstarted tool was observed as executed", err)
	}
}
