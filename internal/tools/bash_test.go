package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"magi/internal/core/protocol"
)

func TestBashUsesConfiguredExecutableAndNoStartupFiles(t *testing.T) {
	root := t.TempDir()
	homeDir := filepath.Join(root, "home with spaces")
	t.Setenv("HOME", homeDir)
	bin := filepath.Join(root, "bin")
	noError(t, os.Mkdir(bin, 0700))
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	// The selected launcher records its exact flags then invokes real Bash.
	launcher := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$3\" > launcher-flags\nexec " + quote(defaultBashPath) + " \"$@\"\n"
	noError(t, os.WriteFile(filepath.Join(bin, "bash"), []byte(launcher), 0700))
	startup := filepath.Join(root, "startup")
	noError(t, os.WriteFile(startup, []byte("printf leaked > startup-ran\n"), 0600))
	for _, key := range []string{"DEEPSEEK_API_KEY", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "MAGI_API_DEBUG", "MAGI_SECRET_TEST"} {
		t.Setenv(key, "host-only-value")
	}
	t.Setenv("BASH_ENV", startup)
	t.Setenv("ENV", startup)
	t.Setenv("PATH", bin)
	script := `test -n "$BASH_VERSION" || exit 71
shopt -q login_shell && exit 72
set -o pipefail
false | true
pipeline=("${PIPESTATUS[@]}")
[[ ${pipeline[0]} == 1 && ${pipeline[1]} == 0 ]] || exit 73
test -z "${DEEPSEEK_API_KEY+x}${HTTPS_PROXY+x}${HTTP_PROXY+x}${ALL_PROXY+x}${MAGI_API_DEBUG+x}${MAGI_SECRET_TEST+x}${BASH_ENV+x}${ENV+x}" || exit 74
printf '%s\n' "$HOME"
HOME="$PWD/build home"
GOPATH="$PWD/build go"
PYTHONPATH="$PWD/python"
BUILD_OPTION='value with spaces'
export HOME GOPATH PYTHONPATH BUILD_OPTION
printf '%s\n' "$HOME" "$GOPATH" "$PYTHONPATH" "$BUILD_OPTION"`
	runtime, err := NewRuntime(root, ExecutablePaths{Bash: filepath.Join(bin, "bash")})
	noError(t, err)
	t.Setenv("PATH", root) // Changing PATH after startup must not change Bash.
	result, err := runtime.Execute(context.Background(), protocol.ToolRequest{ID: "bash", Name: "bash", Arguments: execution(root, map[string]any{"command": script}).Arguments})
	noError(t, err)
	want := fmt.Sprintf("%s\n%s/build home\n%s/build go\n%s/python\nvalue with spaces\n", homeDir, root, root, root)
	if receiptFields(t, result.Body)["stdout_tail"] != want {
		t.Fatal("Bash script did not retain explicit build configuration")
	}
	flags, err := os.ReadFile(filepath.Join(root, "launcher-flags"))
	noError(t, err)
	if string(flags) != "--noprofile\n--norc\n-c\n" {
		t.Fatalf("unexpected Bash startup flags: %q", flags)
	}
	if _, err := os.Stat(filepath.Join(root, "startup-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("host startup hook ran")
	}
}

func TestBashRejectsUnsafeArgumentsBeforeExecution(t *testing.T) {
	for _, args := range []map[string]any{
		{"command": "printf started > marker\x00"},
		{"command": "true", "workdir": "bad\x00path"},
		{"command": "printf started > marker", "workdir": "/"},
		{"command": "printf started > marker", "timeout": 0},
		{"command": "printf started > marker", "timeout": -1},
		{"command": "printf started > marker", "timeout": 1.5},
		{"command": "printf started > marker", "timeout": "1"},
		{"command": "printf started > marker", "timeout": int64(9223372037)},
	} {
		root := t.TempDir()
		request := execution(root, args)
		var feedback protocol.CandidateFeedback
		result, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), request)
		if result.Status != "" || !errors.As(err, &feedback) {
			t.Fatalf("provider did not reject execution constraint: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "marker")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rejected script executed")
		}
	}
}

func TestBashTimeoutKillsChildrenAndRetainsOutput(t *testing.T) {
	for _, tc := range []struct {
		name           string
		timeout        int
		parentDeadline time.Duration
	}{
		{"command deadline", 1, 5 * time.Second},
		{"earlier parent deadline", 60, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runtime, err := NewRuntime(root, ExecutablePaths{})
			noError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), tc.parentDeadline)
			defer cancel()
			// The background writer ignores TERM, so a returned timeout alone is
			// insufficient: its heartbeat must stop and the child must be dead.
			script := `trap '' TERM
(trap '' TERM; while :; do printf x >> heartbeat; sleep 0.01; done) &
printf '%s\n' "$!" > child.pid
printf partial
printf warning >&2
wait`
			request := protocol.ToolRequest{ID: "timed", Name: "bash", Arguments: execution(root, map[string]any{"command": script, "timeout": tc.timeout}).Arguments}
			result, err := runtime.Execute(ctx, request)
			body := decodeReceipt[bashResult](t, result.Body)
			if !errors.Is(err, context.DeadlineExceeded) || result.Status != protocol.ToolTimedOut || body.Status != protocol.ToolTimedOut || body.StdoutTail != "partial" || body.StderrTail != "warning" {
				t.Fatalf("timeout lost its output or status: %+v, %v", body, err)
			}
			pidBody, err := os.ReadFile(filepath.Join(root, "child.pid"))
			noError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidBody)))
			noError(t, err)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			before, err := os.ReadFile(filepath.Join(root, "heartbeat"))
			noError(t, err)
			if len(before) == 0 {
				t.Fatal("child never started")
			}
			time.Sleep(100 * time.Millisecond)
			after, err := os.ReadFile(filepath.Join(root, "heartbeat"))
			noError(t, err)
			if string(before) != string(after) {
				t.Fatal("child kept running after the timeout returned")
			}
			// Orphaned children may remain as zombies until the host reaps them.
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err == nil {
				_, tail, ok := strings.Cut(string(stat), ") ")
				if !ok || !strings.HasPrefix(tail, "Z ") {
					t.Fatalf("child is still alive after timeout: %s", stat)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if tc.name == "command deadline" {
				if ctx.Err() != nil || !strings.Contains(body.Diagnostic, "1s") {
					t.Fatal("local timeout expired the parent or lost its duration", body.Diagnostic)
				}
				request.ID = "continue"
				request.Arguments = json.RawMessage(`{"command":"printf continued"}`)
				result, err = runtime.Execute(ctx, request)
				noError(t, err)
				if decodeReceipt[bashResult](t, result.Body).StdoutTail != "continued" {
					t.Fatal("could not continue after a local timeout")
				}
			}
		})
	}
}

func TestBashMissingExecutableDoesNotFallBackToSh(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	result, err := (bashProvider{program: filepath.Join(root, "missing-bash")}).Execute(context.Background(), execution(root, map[string]any{"command": "printf started > marker"}))
	if err == nil || receiptFields(t, result.Body)["status"] != string(protocol.ToolFailed) || receiptFields(t, result.Body)["exit_code"] != float64(127) {
		t.Fatalf("missing Bash did not fail explicitly: %+v %v", result, err)
	}
	if got := decodeReceipt[bashResult](t, result.Body).Diagnostic; got != err.Error() {
		t.Fatalf("execution error was lost or repeated: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing Bash silently used another shell")
	}
}

func TestBashWorkdirFailureReturnsKnownOutcome(t *testing.T) {
	root := t.TempDir()
	noError(t, os.WriteFile(filepath.Join(root, "file"), []byte("not a directory"), 0600))
	for _, workdir := range []string{"missing", "file/child"} {
		t.Run(workdir, func(t *testing.T) {
			result, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), execution(root, map[string]any{
				"command": "printf started > marker", "workdir": workdir,
			}))
			var body bashResult
			noError(t, json.Unmarshal(result.Body, &body))
			if err == nil || result.Status != protocol.ToolFailed || body.Status != protocol.ToolFailed || body.ExitCode != -1 || body.Diagnostic == "" {
				t.Fatalf("workdir failure cannot be forwarded as a tool receipt: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
				t.Fatal("command ran despite workdir failure", err)
			}
		})
	}
}
