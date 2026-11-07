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
	"unicode/utf8"

	"magi/internal/core/protocol"
)

func TestOutputCompletenessDoesNotReplaceExecutionStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		result processResult
		err    error
	}{
		{"provider_limit", processResult{Stdout: []byte("prefix"), StdoutTotal: 106, StdoutOmitted: 100}, nil},
		{"provider_limit_empty", processResult{StderrTotal: 11, StderrOmitted: 11}, nil},
		{"drain_failure", processResult{Stdout: []byte("prefix"), StdoutTotal: 6, CaptureIncomplete: true}, errors.New("drain failed")},
		{"drain_failure_with_retention_limit", processResult{Stdout: []byte("prefix"), StdoutTotal: 106, StdoutOmitted: 100, CaptureIncomplete: true}, errors.New("drain failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := bashOutcome(test.result, 4, test.err)
			text := string(outcome.Body)
			result := decodeReceipt[bashResult](t, outcome.Body)
			wantStatus := protocol.ToolSucceeded
			if strings.HasPrefix(test.name, "drain_failure") {
				wantStatus = protocol.ToolFailed
			}
			if (err != nil) != (wantStatus == protocol.ToolFailed) || outcome.Status != wantStatus {
				t.Fatalf("result=%s err=%v", text, err)
			}
			switch test.name {
			case "provider_limit":
				if result.StdoutTail != "efix" || !strings.Contains(result.Diagnostic, "retained 6 of 106") || result.StdoutTotal != 106 {
					t.Fatal(text)
				}
			case "provider_limit_empty":
				if result.StderrTail != "" || result.StderrTotal != 11 || !strings.Contains(result.Diagnostic, "retained 0 of 11") {
					t.Fatal(text)
				}
			case "drain_failure", "drain_failure_with_retention_limit":
				if !strings.Contains(text, "missing byte count is unknown") || !strings.Contains(result.Diagnostic, "drain failed") {
					t.Fatal(text)
				}
				if test.name == "drain_failure_with_retention_limit" && (!strings.Contains(result.Diagnostic, "retained 6 of 106") || result.StdoutTotal != 106) {
					t.Fatal("capture failure hid retention loss", text)
				}
			}
			if result.Status != wantStatus || result.ExitCode != 0 || result.Diagnostic == "" {
				t.Fatal(text)
			}
		})
	}
}

func TestBashOutcomePreservesExecutionStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		exit   int
		err    error
		status protocol.ToolCallStatus
	}{
		{"success", 0, nil, protocol.ToolSucceeded},
		{"exit_failure", 7, processExitError(7), protocol.ToolFailed},
		{"cancelled", -1, context.Canceled, protocol.ToolInterrupted},
		{"timed_out", -1, context.DeadlineExceeded, protocol.ToolTimedOut},
	} {
		t.Run(test.name, func(t *testing.T) {
			const storageDiagnostic = "output file unavailable: storage failure"
			outcome, err := bashOutcome(processResult{ExitCode: test.exit, Diagnostic: storageDiagnostic}, 100, test.err)
			result := decodeReceipt[bashResult](t, outcome.Body)
			if !errors.Is(err, test.err) || outcome.Status != test.status || result.Status != test.status || result.ExitCode != test.exit {
				t.Fatalf("execution outcome changed: %s, err=%v", outcome.Body, err)
			}
			wantDiagnostic := storageDiagnostic
			if test.err != nil {
				wantDiagnostic += "\n" + test.err.Error()
			}
			if result.Diagnostic != wantDiagnostic {
				t.Fatalf("lost or repeated diagnostic: %q", result.Diagnostic)
			}
		})
	}
}

func TestToolOutputPreviewKeepsTextBoundariesAndLabelsBinary(t *testing.T) {
	for _, test := range []struct {
		name, out, errout string
		stdout, stderr    []byte
		limit             int
	}{
		{name: "marker text", stdout: []byte("status: FAILED\n\n[stderr]\nSource"), limit: 64, out: "status: FAILED\n\n[stderr]\nSource"},
		{name: "complete text", stdout: []byte("ready"), stderr: []byte("warning"), limit: 16, out: "ready", errout: "warning"},
		{name: "stderr only", stderr: []byte("warning"), limit: 16, errout: "warning"},
		{name: "unicode boundary", stdout: []byte("AéB"), stderr: []byte("Z界Q"), limit: 2, out: "B", errout: "Q"},
		{name: "one byte limit", stdout: []byte("é"), stderr: []byte("xé"), limit: 1},
		{name: "binary one byte limit", stdout: []byte{0xff}, stderr: []byte{0xfe}, limit: 1, out: "[binary output; base64 tail: ; 1 bytes omitted]", errout: "[binary output; base64 tail: ; 1 bytes omitted]"},
		{name: "binary", stdout: []byte{0xff, 0x00, 'A', 'B'}, stderr: []byte{0xfe, 'z'}, limit: 4, out: "[binary output; base64 tail: AEFC; 1 bytes omitted]", errout: "[binary output; base64 tail: /no; 0 bytes omitted]"},
		{name: "line limit", stdout: []byte("old\n" + strings.Repeat("line\n", 2000)), stderr: []byte("old\n" + strings.Repeat("line\n", 1999) + "end"), limit: 50 << 10, out: strings.Repeat("line\n", 2000), errout: strings.Repeat("line\n", 1999) + "end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := processResult{Stdout: test.stdout, Stderr: test.stderr, StdoutTotal: int64(len(test.stdout)), StderrTotal: int64(len(test.stderr))}
			outcome, err := bashOutcome(raw, test.limit, nil)
			if err != nil || outcome.Status != protocol.ToolSucceeded {
				t.Fatalf("result=%+v err=%v", outcome, err)
			}
			result := decodeReceipt[bashResult](t, outcome.Body)
			if result.StdoutTail != test.out || result.StderrTail != test.errout || !utf8.Valid(outcome.Body) || strings.ContainsRune(string(outcome.Body), utf8.RuneError) {
				t.Fatalf("preview changed: %s", outcome.Body)
			}
		})
	}
}

func TestBashOutputCanBeReadAndSearchedWithoutRepeatingCommand(t *testing.T) {
	root := t.TempDir()
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	if _, err := os.Stat(filepath.Join(root, ".magi-temp")); !os.IsNotExist(err) {
		t.Fatal("registry created temporary output directory", err)
	}
	noError(t, runtime.PrepareRound(context.Background()))
	request := outputRequest()
	request.Name = "bash"
	request.Arguments = json.RawMessage(`{"command":"printf once >> counter; for i in {1..2001}; do printf 'line %s\n' \"$i\"; done; printf warning >&2"}`)
	outcome, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	dump := ".magi-temp/" + request.ID + ".stdout"
	text := string(outcome.Body)
	result := decodeReceipt[bashResult](t, outcome.Body)
	data, err := os.ReadFile(filepath.Join(root, dump))
	noError(t, err)
	if outcome.Status != protocol.ToolSucceeded || result.StdoutTail != strings.TrimPrefix(string(data), "line 1\n") || result.StderrTail != "warning" || result.StdoutTotal != int64(len(data)) || result.StdoutDump != dump || result.StderrTotal != 0 || result.StderrDump != "" {
		t.Fatalf("result=%s", text)
	}
	if !strings.HasPrefix(string(data), "line 1\n") || !strings.HasSuffix(string(data), "line 2001\n") {
		t.Fatalf("saved output=%q", data)
	}
	// Short stderr remains fully inline while both streams are still captured.
	stderr, err := os.ReadFile(filepath.Join(root, ".magi-temp", request.ID+".stderr"))
	noError(t, err)
	if string(stderr) != "warning" {
		t.Fatalf("stderr=%q", stderr)
	}
	info, err := os.Stat(filepath.Join(root, dump))
	noError(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("output file is not private", info.Mode())
	}
	request.Name, request.ID = "read", "read-output"
	request.Arguments, _ = json.Marshal(map[string]any{"path": dump, "offset": 20, "limit": 2})
	page, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	gotPage := decodeReceipt[readResult](t, page.Body)
	if gotPage.Path != dump || gotPage.Content != "20: line 20\n21: line 21\n" || gotPage.Next != 22 {
		t.Fatalf("page=%s", page.Body)
	}
	request.Name, request.ID = "grep", "search-output"
	request.Arguments, _ = json.Marshal(map[string]any{"path": dump, "pattern": "^line 20$"})
	found, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	if decodeReceipt[grepResult](t, found.Body).Matches[dump] != "20: line 20\n" {
		t.Fatal(found)
	}
	request.ID = "search-project"
	request.Arguments = json.RawMessage(`{"path":".","pattern":"^line 20$"}`)
	found, err = runtime.Execute(context.Background(), request)
	noError(t, err)
	if strings.Contains(string(found.Body), ".magi-temp") {
		t.Fatal("temporary output leaked into project search", found)
	}
	counter, err := os.ReadFile(filepath.Join(root, "counter"))
	noError(t, err)
	if string(counter) != "once" {
		t.Fatal("command repeated", string(counter))
	}
}

func TestBashDefaultRetainsOutputBeyondInlineTail(t *testing.T) {
	root := t.TempDir()
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	request := outputRequest()
	request.Name = "bash"
	request.Arguments = json.RawMessage(`{"command":"printf '%65536s' x"}`)
	outcome, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	result := decodeReceipt[bashResult](t, outcome.Body)
	if result.StdoutTotal != 65536 || result.StdoutTail != strings.Repeat(" ", 51200-1)+"x" || result.StdoutDump == "" || result.Diagnostic != "" {
		t.Fatalf("default output retention lost bytes beyond the inline tail: %+v", result)
	}
	dump, err := os.ReadFile(filepath.Join(root, result.StdoutDump))
	noError(t, err)
	if len(dump) != 65536 || string(dump[len(dump)-len(result.StdoutTail):]) != result.StdoutTail {
		t.Fatal("default dump does not preserve complete output and its tail")
	}
}

func TestBashDoesNotAdvertiseRemovedOutput(t *testing.T) {
	root := t.TempDir()
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	request := outputRequest()
	request.Name = "bash"
	// Fill the pipe before removal, ensuring capture files have been created.
	request.Arguments = json.RawMessage(`{"command":"printf '%131072s' x; rm -rf .magi-temp"}`)
	outcome, err := runtime.Execute(context.Background(), request)
	noError(t, err)
	result := decodeReceipt[bashResult](t, outcome.Body)
	if result.Status != protocol.ToolSucceeded || result.ExitCode != 0 || result.StdoutDump != "" || result.StderrDump != "" || result.StdoutTotal != 131072 || !strings.HasSuffix(result.StdoutTail, "x") || !strings.Contains(result.Diagnostic, "output file unavailable") {
		t.Fatalf("missing dump changed command outcome or hid output loss: %+v", result)
	}
}

func TestOutputDirectoryRecoveryPreservesConcurrentCaptures(t *testing.T) {
	root := t.TempDir()
	var workers sync.WaitGroup
	failures := make(chan error, 16)
	for i := range 16 {
		workers.Go(func() {
			file, err := createOutput(root, fmt.Sprintf("%srestore-%d.stdout", outputDirectory, i))
			if err == nil {
				_, writeErr := file.WriteString("retained")
				err = errors.Join(writeErr, file.Close())
			}
			failures <- err
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		noError(t, err)
	}
	for i := range 16 {
		data, err := os.ReadFile(filepath.Join(root, tempDirectory, fmt.Sprintf("restore-%d.stdout", i)))
		noError(t, err)
		if string(data) != "retained" {
			t.Fatalf("recovery cleared a sibling's output: %q", data)
		}
	}
}

func TestOutputDirectoryRecoveryDoesNotFollowReplacementLink(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	noError(t, os.Symlink(target, filepath.Join(root, tempDirectory)))
	file, err := createOutput(root, outputDirectory+"probe.stdout")
	if err == nil {
		file.Close()
		t.Fatal("capture followed a replacement link")
	}
	entries, err := os.ReadDir(target)
	noError(t, err)
	if len(entries) != 0 {
		t.Fatal("capture changed the link target", entries)
	}
}

func TestRoundOutputPreparationRemovesContentsWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	noError(t, os.Mkdir(filepath.Join(root, ".magi"), 0700))
	metadata := filepath.Join(root, ".magi", "state.db")
	noError(t, os.WriteFile(metadata, []byte("private"), 0600))
	noError(t, os.Symlink(".magi", filepath.Join(root, ".magi-temp")))
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	data, err := os.ReadFile(metadata)
	noError(t, err)
	if string(data) != "private" {
		t.Fatal("cleanup changed metadata")
	}
	probe := filepath.Join(root, ".magi-temp", "probe", "nested")
	noError(t, os.MkdirAll(probe, 0700))
	noError(t, os.WriteFile(filepath.Join(probe, "main.go"), []byte("scratch"), 0600))
	noError(t, os.Symlink(filepath.Join(root, ".magi"), filepath.Join(probe, "metadata")))
	noError(t, os.Symlink(metadata, filepath.Join(root, ".magi-temp", "metadata")))
	noError(t, runtime.PrepareRound(context.Background()))
	entries, err := os.ReadDir(filepath.Join(root, ".magi-temp"))
	noError(t, err)
	if len(entries) != 1 || entries[0].Name() != ".gitignore" {
		t.Fatal("round preparation left temporary content", entries)
	}
	data, err = os.ReadFile(metadata)
	noError(t, err)
	if string(data) != "private" {
		t.Fatal("recursive cleanup followed a link into metadata")
	}
}

func TestBashOutputCapKeepsRealTailAndCommandStatus(t *testing.T) {
	for _, exit := range []int{0, 7} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			root := t.TempDir()
			runtime, err := NewRuntime(root, ExecutablePaths{})
			noError(t, err)
			noError(t, runtime.PrepareRound(context.Background()))
			request := execution(root, bashArgs{Command: fmt.Sprintf("printf 012345678901234567890123456789END; printf stderr-012345678901234567890123END >&2; exit %d", exit)})
			request.MaxOutputBytes, request.inlineBytes, request.callID = 24, 8, "call"
			outcome, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), request)
			text := string(outcome.Body)
			result := decodeReceipt[bashResult](t, outcome.Body)
			if (err != nil) != (exit != 0) || (outcome.Status == protocol.ToolSucceeded) != (exit == 0) || result.StdoutTail != "56789END" || result.StderrTail != "90123END" || !strings.Contains(text, "retained 24 of 33") {
				t.Fatalf("result=%s err=%v", text, err)
			}
			if result.Status != outcome.Status || result.ExitCode != exit || result.StdoutTotal != 33 || result.StderrTotal != 34 || result.StdoutDump != ".magi-temp/"+request.callID+".stdout" || result.StderrDump != ".magi-temp/"+request.callID+".stderr" {
				t.Fatalf("lost execution or output evidence: %s", text)
			}
			data, err := os.ReadFile(filepath.Join(root, ".magi-temp", request.callID+".stdout"))
			noError(t, err)
			if string(data) != "012345678901234567890123" {
				t.Fatalf("retained prefix=%q", data)
			}
		})
	}
}

func TestBashOutputStorageFailureStillDrainsAndReportsExit(t *testing.T) {
	root := t.TempDir()
	noError(t, os.WriteFile(filepath.Join(root, ".magi-temp"), []byte("not a directory"), 0600))
	request := execution(root, bashArgs{Command: "printf 01234567890123456789END"})
	request.MaxOutputBytes, request.inlineBytes, request.callID = 64, 8, "call"
	outcome, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), request)
	noError(t, err)
	text := string(outcome.Body)
	result := decodeReceipt[bashResult](t, outcome.Body)
	if outcome.Status != protocol.ToolSucceeded || result.StdoutDump != "" || result.StdoutTail != "56789END" || !strings.Contains(result.Diagnostic, "output file unavailable") || result.Status != protocol.ToolSucceeded || result.ExitCode != 0 {
		t.Fatalf("result=%s", text)
	}
}

func outputRequest() protocol.ToolRequest {
	return protocol.ToolRequest{ID: "call", Name: "bash", Arguments: json.RawMessage(`{}`)}
}

func decodeReceipt[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("invalid JSON receipt: %s: %v", body, err)
	}
	return value
}
