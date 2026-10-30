package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessCancelsProcessGroup(t *testing.T) {
	root := t.TempDir()
	script := "(while :; do printf x >> heartbeat; sleep 0.01; done) & wait"
	ctx, cancel := context.WithCancel(processContext(t, 3*time.Second))
	defer cancel()
	type outcome struct {
		result processResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runBash(ctx, defaultBashPath, script, root, toolExecution{MaxOutputBytes: 100})
		done <- outcome{result, err}
	}()
	for {
		if body, err := os.ReadFile(filepath.Join(root, "heartbeat")); err == nil && len(body) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("child did not start", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	started := time.Now()
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.result.ExitCode != -1 || time.Since(started) > time.Second {
		t.Fatalf("result=%+v err=%v elapsed=%v", got.result, got.err, time.Since(started))
	}
	before, err := os.ReadFile(filepath.Join(root, "heartbeat"))
	noError(t, err)
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(root, "heartbeat"))
	noError(t, err)
	if string(before) != string(after) {
		t.Fatal("child kept running after cancellation")
	}
}

func TestProcessParentDeathHelper(t *testing.T) {
	if os.Getenv("MAGI_PARENT_DEATH_HELPER") != "1" {
		return
	}
	heartbeat := os.Getenv("MAGI_PARENT_DEATH_HEARTBEAT")
	groupFile := os.Getenv("MAGI_PARENT_DEATH_GROUP")
	script := fmt.Sprintf("echo $PPID > %q; (while :; do printf x >> %q; sleep 0.02; done) & wait", groupFile, heartbeat)
	if _, err := runBash(processContext(t, time.Minute), defaultBashPath, script, filepath.Dir(heartbeat), toolExecution{MaxOutputBytes: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestProcessKillsTreeWhenHostDies(t *testing.T) {
	root := t.TempDir()
	heartbeat := filepath.Join(root, "heartbeat")
	groupFile := filepath.Join(root, "group")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessParentDeathHelper$")
	cmd.Env = append(os.Environ(),
		"MAGI_PARENT_DEATH_HELPER=1",
		"MAGI_PARENT_DEATH_HEARTBEAT="+heartbeat,
		"MAGI_PARENT_DEATH_GROUP="+groupFile,
	)
	noError(t, cmd.Start())
	var processGroup int
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if processGroup > 0 {
			_ = syscall.Kill(-processGroup, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		body, groupErr := os.ReadFile(groupFile)
		info, heartbeatErr := os.Stat(heartbeat)
		if groupErr == nil && heartbeatErr == nil && info.Size() > 2 {
			processGroup, _ = strconv.Atoi(strings.TrimSpace(string(body)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if processGroup <= 0 {
		t.Fatal("helper process tree did not start")
	}
	noError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	first, err := os.Stat(heartbeat)
	noError(t, err)
	time.Sleep(250 * time.Millisecond)
	second, err := os.Stat(heartbeat)
	noError(t, err)
	if second.Size() != first.Size() {
		t.Fatalf("orphan process kept running after host death: size %d -> %d", first.Size(), second.Size())
	}
	processGroup = 0
}

func TestProcessReapsWritersBeforeReturning(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		redirect string
	}{
		{"success", 0, "</dev/null >/dev/null 2>&1"},
		{"nonzero", 7, "</dev/null >/dev/null 2>&1"},
		{"high_exit_status", 200, "</dev/null >/dev/null 2>&1"},
		{"inherited_pipes", 0, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			shell := fmt.Sprintf("echo $PPID > group; (while :; do printf x >> heartbeat; sleep 0.01; done) %s & while [ ! -s heartbeat ]; do sleep 0.01; done; printf out; printf err >&2; exit %d", test.redirect, test.status)
			started := time.Now()
			result, err := runBash(processContext(t, 600*time.Millisecond), defaultBashPath, shell, root, toolExecution{ProjectRoot: root, MaxOutputBytes: 100})
			elapsed := time.Since(started)
			groupBody, _ := os.ReadFile(filepath.Join(root, "group"))
			group, _ := strconv.Atoi(strings.TrimSpace(string(groupBody)))
			if group > 0 {
				defer syscall.Kill(-group, syscall.SIGKILL)
			}
			if result.ExitCode != test.status || (err == nil) != (test.status == 0) || errors.Is(err, context.DeadlineExceeded) || elapsed >= 500*time.Millisecond {
				t.Fatalf("command status/output-pipe lifetime: result=%+v err=%v", result, err)
			}
			if string(result.Stdout) != "out" || string(result.Stderr) != "err" {
				t.Fatalf("lost output: %+v", result)
			}
			before, readErr := os.ReadFile(filepath.Join(root, "heartbeat"))
			if readErr != nil || len(before) == 0 {
				t.Fatalf("writer never started: %v", readErr)
			}
			time.Sleep(100 * time.Millisecond)
			after, readErr := os.ReadFile(filepath.Join(root, "heartbeat"))
			if readErr != nil || string(before) != string(after) {
				t.Fatalf("writer survived synchronous return: %d -> %d, err=%v", len(before), len(after), readErr)
			}
		})
	}
}

func TestProcessReportsUnfinishedOutputDrain(t *testing.T) {
	root := t.TempDir()
	// A command can leave the owned group, but that must not make an
	// incomplete output capture look successful. The test owns its cleanup.
	shell := "setsid /bin/sh -c 'echo $$ > detached; sleep 10' & while [ ! -s detached ]; do sleep 0.01; done; printf captured"
	started := time.Now()
	result, err := runBash(processContext(t, 2*time.Second), defaultBashPath, shell, root, toolExecution{ProjectRoot: root, MaxOutputBytes: 100})
	elapsed := time.Since(started)
	body, _ := os.ReadFile(filepath.Join(root, "detached"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	if pid > 0 {
		defer syscall.Kill(-pid, syscall.SIGKILL)
	}
	if err == nil || !result.CaptureIncomplete || elapsed > 3*time.Second {
		t.Fatalf("unfinished pipes reported success or hung: %+v %v", result, err)
	}
}

func TestExplicitExitStatusPreserved(t *testing.T) {
	for _, status := range []int{0, 7, 128, 137, 255} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{"command": fmt.Sprintf("exit %d", status)})
			root := t.TempDir()
			request := toolExecution{Arguments: args, ProjectRoot: root, MaxOutputBytes: 100}
			result, err := (bashProvider{program: defaultBashPath}).Execute(processContext(t, time.Second), request)
			if (err == nil) != (status == 0) || receiptFields(t, result.Body)["exit_code"] != float64(status) || receiptFields(t, result.Body)["stdout_tail"] != nil || receiptFields(t, result.Body)["stderr_tail"] != nil {
				t.Fatalf("explicit exit %d: result=%+v err=%v", status, result, err)
			}
			if status != 0 {
				var exitErr interface{ ExitCode() int }
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != status {
					t.Fatalf("error lost exit status %d: %v", status, err)
				}
			}
		})
	}
}

func TestProcessMissingStatusCannotSucceed(t *testing.T) {
	script := "kill -KILL $PPID"
	result, err := runBash(processContext(t, time.Second), defaultBashPath, script, t.TempDir(), toolExecution{MaxOutputBytes: 100})
	if err == nil || result.ExitCode != -1 {
		t.Fatalf("missing wrapper status became a command outcome: %+v %v", result, err)
	}
}

func TestProcessSetupFailureKeepsUnknownExit(t *testing.T) {
	if os.Getenv("MAGI_PIPE_FAILURE_HELPER") == "1" {
		var limit syscall.Rlimit
		noError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit))
		reduced := limit
		reduced.Cur = 3
		noError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &reduced))
		defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)
		result, err := runBash(context.Background(), defaultBashPath, "exit 0", ".", toolExecution{})
		if !errors.Is(err, syscall.EMFILE) || result.ExitCode != -1 {
			t.Fatalf("setup failure became an exit status: %+v, err=%v", result, err)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestProcessSetupFailureKeepsUnknownExit$")
	command.Env = append(os.Environ(), "MAGI_PIPE_FAILURE_HELPER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pipe failure helper: %v\n%s", err, output)
	}
}

func processContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}
