package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Keep the group leader alive until its owner has captured the command status
// and killed the whole group. Waiting/reaping earlier would release its PID.
const processWatchdog = `(
	while IFS= read -r _ <&3; do :; done
	kill -KILL 0
) 4>&- &
watchdog_pid=$!
"$@" 3<&- 4>&-
status=$?
printf '%s\n' "$status" >&4
wait "$watchdog_pid"`

type processExitError int

func (e processExitError) Error() string { return fmt.Sprintf("process exited with status %d", int(e)) }

func (e processExitError) ExitCode() int { return int(e) }

type processResult struct {
	ExitCode          int
	Stdout            []byte
	Stderr            []byte
	StdoutPath        string
	StderrPath        string
	StdoutOmitted     int64
	StderrOmitted     int64
	StdoutTotal       int64
	StderrTotal       int64
	CaptureIncomplete bool   // Stream drain failed; uncaptured byte count is unknown.
	Diagnostic        string // Output storage diagnostics; execution errors are returned separately.
}

func runBash(ctx context.Context, program, script, cwd string, request toolExecution) (processResult, error) {
	controlRead, controlWrite, pipeErr := os.Pipe()
	if pipeErr != nil {
		return processResult{ExitCode: -1}, pipeErr
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	statusRead, statusWrite, pipeErr := os.Pipe()
	if pipeErr != nil {
		return processResult{ExitCode: -1}, pipeErr
	}
	defer statusRead.Close()
	defer statusWrite.Close()
	environment := controlledEnv()
	command := exec.Command("/bin/sh", "-c", processWatchdog, "magi-exec-watchdog", program, "--noprofile", "--norc", "-c", script)
	command.Dir = cwd
	command.Env = environment
	command.ExtraFiles = []*os.File{controlRead, statusWrite}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		return processResult{ExitCode: -1}, err
	}
	defer stdoutPipe.Close()
	stderrPipe, err := command.StderrPipe()
	if err != nil {
		return processResult{ExitCode: -1}, err
	}
	defer stderrPipe.Close()
	if err := command.Start(); err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return processResult{ExitCode: -1}, err
	}
	_ = controlRead.Close()
	_ = statusWrite.Close()
	stdout := newStreamOutput(request, "stdout")
	stderr := newStreamOutput(request, "stderr")
	outputDone := make(chan error, 2)
	go func() { _, err := io.Copy(stdout, stdoutPipe); outputDone <- err }()
	go func() { _, err := io.Copy(stderr, stderrPipe); outputDone <- err }()
	statusDone := make(chan error, 1)
	exitStatus := -1
	go func() {
		_, readErr := fmt.Fscan(statusRead, &exitStatus)
		if readErr == nil && (exitStatus < 0 || exitStatus > 255) {
			readErr = errors.New("invalid command exit status")
		}
		statusDone <- readErr
	}()
	err = nil
	select {
	case err = <-statusDone:
	case <-ctx.Done():
		err = ctx.Err()
		_ = statusRead.Close()
		<-statusDone
	}
	// The leader has not been reaped, so this cannot target a reused group ID.
	killErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	_ = controlWrite.Close()
	// Drain before Wait closes its pipes. Wait's cleanup ExitError would mask
	// copy errors, so own and join these readers explicitly with a bounded drain.
	drainTimer := time.NewTimer(time.Second)
	defer drainTimer.Stop()
	incomplete := false
	for remaining := 2; remaining > 0; remaining-- {
		select {
		case copyErr := <-outputDone:
			if copyErr != nil {
				incomplete = true
				err = errors.Join(err, copyErr)
			}
		case <-drainTimer.C:
			incomplete = true
			err = errors.Join(err, errors.New("process output drain exceeded cleanup deadline"))
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
			<-outputDone
		}
	}
	waitErr := command.Wait()
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		err = errors.Join(err, killErr)
	}
	// SIGKILL is our cleanup, not the actual command's exit status. Other wait
	// errors must not become success; output errors were collected separately.
	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		err = errors.Join(err, waitErr)
	}
	if err == nil && exitStatus != 0 {
		err = processExitError(exitStatus)
	}

	// Both capture goroutines have joined; the result owns their buffers.
	stdoutPath, stdoutOmitted, stdoutDiagnostic := stdout.finish()
	stderrPath, stderrOmitted, stderrDiagnostic := stderr.finish()
	result := processResult{Stdout: stdout.tail, Stderr: stderr.tail, StdoutPath: stdoutPath, StderrPath: stderrPath, StdoutOmitted: stdoutOmitted, StderrOmitted: stderrOmitted, StdoutTotal: stdout.total, StderrTotal: stderr.total, CaptureIncomplete: incomplete, Diagnostic: strings.TrimSpace(stdoutDiagnostic + "\n" + stderrDiagnostic)}
	result.ExitCode = exitStatus
	return result, err
}

func controlledEnv() []string {
	var result []string
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		switch name {
		case "HOME", "PATH", "LANG", "LC_ALL", "TERM", "TMPDIR":
			result = append(result, item)
		}
	}
	return result
}
