package debug

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"magi/internal/core/protocol"
)

func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func enableProbe(t *testing.T, mode, pause string) string {
	t.Helper()
	dir := t.TempDir()
	noError(t, os.Chmod(dir, 0700))
	t.Setenv("MAGI_DEBUG_API_DIR", dir)
	t.Setenv("MAGI_DEBUG_API_MODE", mode)
	t.Setenv("MAGI_DEBUG_API_PAUSE", pause)
	return dir
}
func probeContext(session string) context.Context {
	return protocol.WithDiagnosticIdentity(context.Background(), protocol.DiagnosticIdentity{
		InvocationID: "invocation", WorkflowID: "workflow", SessionID: session,
		Role: protocol.RoleWorker, Purpose: "worker", PromptVersion: "fixture",
	})
}
func readMetadata(t *testing.T, path string) requestMetadata {
	t.Helper()
	raw, err := os.ReadFile(path)
	noError(t, err)
	var meta requestMetadata
	noError(t, json.Unmarshal(raw, &meta))
	return meta
}
func fixtureResponse() *protocol.Response {
	reasoning := "private reasoning"
	return &protocol.Response{FinishReason: "stop", Message: protocol.Message{Role: "assistant", Content: "private response", ReasoningContent: &reasoning}, Usage: protocol.Usage{InputTokens: 100, OutputTokens: 20}}
}

const fixtureRequest = `{"model":"fixture","thinking":{"type":"enabled"},"reasoning_effort":"high","max_tokens":1024,"messages":[{"role":"user","content":"private request"}]}`

func TestBaseCaptureAndFullRequestRecords(t *testing.T) {
	for _, mode := range []string{"minimal", "full"} {
		t.Run(mode, func(t *testing.T) {
			pause := ""
			if mode == "minimal" {
				pause = "invalid"
			}
			dir := enableProbe(t, mode, pause)
			ctx := probeContext("worker:P1")
			session := filepath.Join(dir, "worker:P1")
			observer := Observer{}
			noError(t, observer.BeforeModelRequest(ctx, "call", []byte(fixtureRequest)))
			now := time.Now().UTC()
			observer.RecordModelRequest(ctx, &protocol.RequestDiagnostic{RequestBody: []byte(fixtureRequest), CallID: "call", Started: now, FirstByte: &now, Ended: &now, HTTPStatus: 200, ResponseBytes: 123}, fixtureResponse(), nil)
			observer.DumpContext(ctx, "worker_exit", []byte(`{"messages":[]}`))
			record := protocol.ToolDiagnostic{CallID: "../tool", Tool: "read", Started: now, Ended: now, Status: protocol.ToolSucceeded}
			var group sync.WaitGroup
			for range 4 {
				group.Go(func() { observer.RecordToolExecution(ctx, &record) })
			}
			group.Wait()
			paths, err := filepath.Glob(filepath.Join(session, "tools", "*.json"))
			noError(t, err)
			if len(paths) != 4 {
				t.Fatal("lost parallel tool records", len(paths))
			}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				noError(t, err)
				var data struct {
					protocol.DiagnosticIdentity
					protocol.ToolDiagnostic
				}
				noError(t, json.Unmarshal(raw, &data))
				if data.ToolDiagnostic != record || data.SessionID != "worker:P1" || strings.Contains(string(raw), "private") {
					t.Fatal("invalid tool statistics")
				}
			}
			checkpoints, err := filepath.Glob(filepath.Join(session, "contexts", "*.json"))
			noError(t, err)
			if len(checkpoints) != 1 {
				t.Fatal("missing Context dump")
			}
			var checkpoint struct {
				Event    string          `json:"event"`
				Snapshot json.RawMessage `json:"snapshot"`
			}
			raw, err := os.ReadFile(checkpoints[0])
			noError(t, err)
			noError(t, json.Unmarshal(raw, &checkpoint))
			if checkpoint.Event != "worker_exit" || string(checkpoint.Snapshot) != `{"messages":[]}` {
				t.Fatal("Context dump changed")
			}
			if mode == "full" {
				meta := readMetadata(t, filepath.Join(session, "call.json"))
				if meta.Status != "completed" || meta.HTTPStatus != 200 || meta.ResponseBytes != 123 || meta.FirstByte == nil || meta.Usage.Total() != 120 || meta.SessionID != "worker:P1" || meta.PromptVersion != "fixture" || meta.Thinking != "enabled" || meta.ReasoningEffort != "high" {
					t.Fatalf("incomplete request metadata: %+v", meta)
				}
				raw, err := os.ReadFile(filepath.Join(session, "requests/call.json"))
				noError(t, err)
				if strings.TrimSpace(string(raw)) != fixtureRequest {
					t.Fatal("request body changed")
				}
				for _, path := range []string{"call.json", "requests/call.json", "responses/call.json"} {
					info, err := os.Stat(filepath.Join(session, path))
					noError(t, err)
					if info.Mode().Perm() != 0600 {
						t.Fatal("debug record is not private")
					}
				}
			} else {
				for _, name := range []string{"call.json", "requests", "responses"} {
					if _, err := os.Stat(filepath.Join(session, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("minimal capture created request payloads", name, err)
					}
				}
			}
		})
	}
}

func TestFullCaptureDisabledThinking(t *testing.T) {
	dir := enableProbe(t, "full", "")
	ctx := probeContext("reviewer:task")
	body := []byte(`{"model":"fixture","thinking":{"type":"disabled"},"max_tokens":1024}`)
	observer := Observer{}
	noError(t, observer.BeforeModelRequest(ctx, "disabled", body))
	observer.RecordModelRequest(ctx, &protocol.RequestDiagnostic{RequestBody: body, CallID: "disabled"}, fixtureResponse(), nil)
	meta := readMetadata(t, filepath.Join(dir, "reviewer:task", "disabled.json"))
	if meta.Thinking != "disabled" || meta.ReasoningEffort != "" {
		t.Fatal("non-thinking mode was not preserved in metadata", meta.Thinking, meta.ReasoningEffort)
	}
}

func TestMinimalCaptureSavesFailedRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		finish string
	}{
		{"http-error", protocol.ErrModelUnavailable, ""},
		{"canceled", context.Canceled, ""},
		{"output-limit", nil, "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := enableProbe(t, "minimal", "")
			ctx := probeContext("reviewer:task")
			diagnostic := protocol.RequestDiagnostic{CallID: "failed", RequestBody: []byte(fixtureRequest), HTTPStatus: 400,
				HTTPErrorBody: `{"error":{"message":"invalid message sequence"}}`}
			(Observer{}).RecordModelRequest(ctx, &diagnostic, &protocol.Response{FinishReason: tc.finish}, tc.err)
			session := filepath.Join(dir, "reviewer:task")
			meta := readMetadata(t, filepath.Join(session, "failed.json"))
			if meta.Status != "failed" || meta.HTTPErrorBody != diagnostic.HTTPErrorBody || meta.Model != "fixture" || meta.SessionID != "reviewer:task" {
				t.Fatal("missing failure diagnostics", meta)
			}
			path := filepath.Join(session, "requests/failed.json")
			raw, err := os.ReadFile(path)
			noError(t, err)
			if strings.TrimSpace(string(raw)) != fixtureRequest {
				t.Fatal("failed request was reconstructed or lost")
			}
			info, err := os.Stat(path)
			noError(t, err)
			if info.Mode().Perm() != 0600 {
				t.Fatal("failed request is not private")
			}
		})
	}
}

func TestReplyNotificationIgnoresModelPause(t *testing.T) {
	for _, mode := range []string{"", "full"} {
		t.Run(mode, func(t *testing.T) {
			dir := enableProbe(t, mode, "invalid")
			previous := ""
			for range 2 {
				(Observer{}).NotifyReplyRequired()
				raw, err := os.ReadFile(filepath.Join(dir, "reply-request.json"))
				noError(t, err)
				var request struct {
					ID string `json:"id"`
				}
				noError(t, json.Unmarshal(raw, &request))
				if request.ID == "" || request.ID == previous {
					t.Fatal("invalid reply request")
				}
				previous = request.ID
			}
		})
	}
}

func TestFullCaptureSessionIsolationAndPause(t *testing.T) {
	root := enableProbe(t, "full", "before")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var group sync.WaitGroup
	t.Cleanup(group.Wait)
	done := make(chan error, 2)
	for _, session := range []string{"worker:P1", "worker:P2"} {
		callCtx := protocol.WithDiagnosticIdentity(ctx, protocol.DiagnosticIdentityFromContext(probeContext(session)))
		group.Go(func() {
			err := (Observer{}).BeforeModelRequest(callCtx, "call", []byte(fixtureRequest))
			now := time.Now().UTC()
			(Observer{}).RecordModelRequest(callCtx, &protocol.RequestDiagnostic{RequestBody: []byte(fixtureRequest), CallID: "call", Started: now, Ended: &now}, fixtureResponse(), err)
			done <- err
		})
	}
	for _, session := range []string{"worker:P1", "worker:P2"} {
		path := filepath.Join(root, session, "call.json")
		until := time.Now().Add(3 * time.Second)
		for {
			raw, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(raw), "waiting_before") {
				break
			}
			if time.Now().After(until) {
				cancel()
				t.Fatal("pause did not appear")
			}
			time.Sleep(time.Millisecond)
		}
	}
	marker := filepath.Join(root, "worker:P1/call.continue")
	noError(t, os.WriteFile(marker, nil, 0600))
	select {
	case err := <-done:
		noError(t, err)
	case <-time.After(time.Second):
		cancel()
		t.Fatal("release stalled")
	}
	if readMetadata(t, filepath.Join(root, "worker:P1/call.json")).Status != "completed" || readMetadata(t, filepath.Join(root, "worker:P2/call.json")).Status != "waiting_before" {
		t.Fatal("session release affected another request")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release marker was not consumed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pause did not cancel")
	}
	if readMetadata(t, filepath.Join(root, "worker:P2/call.json")).Error != "canceled" {
		t.Fatal("cancellation lost")
	}
}

func TestCaptureFailureOnlyBlocksExplicitPause(t *testing.T) {
	for _, pause := range []string{"", "before"} {
		t.Run(pause, func(t *testing.T) {
			root := enableProbe(t, "full", pause)
			ctx := probeContext("worker:P1")
			session, err := sessionDirectory(ctx)
			noError(t, err)
			// Request payloads must not follow a pre-existing symlink.
			target := t.TempDir()
			noError(t, os.Symlink(target, filepath.Join(session, "requests")))
			err = (Observer{}).BeforeModelRequest(ctx, "call", []byte(fixtureRequest))
			if (pause != "") != errors.Is(err, errCapture) {
				t.Fatal("wrong observation failure boundary", err)
			}
			paths, _ := filepath.Glob(filepath.Join(target, "*"))
			if len(paths) != 0 {
				t.Fatal("payload followed symlink")
			}
			if _, err := os.Stat(filepath.Join(root, "worker:P1/call.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published an incomplete request")
			}
		})
	}
}

func TestTransportDiagnosticsNeverSaveErrorTextOrPartialResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"dns", &net.DNSError{Err: "private detail", Name: "private host"}},
		{"tls", tls.RecordHeaderError{Msg: "private detail"}},
		{"timeout", context.DeadlineExceeded},
		{"connection_reset", syscall.ECONNRESET},
		{"eof", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := enableProbe(t, "full", "")
			ctx := probeContext("worker:P1")
			noError(t, (Observer{}).BeforeModelRequest(ctx, "call", []byte(fixtureRequest)))
			now := time.Now().UTC()
			(Observer{}).RecordModelRequest(ctx, &protocol.RequestDiagnostic{RequestBody: []byte(fixtureRequest), CallID: "call", Started: now, Ended: &now, TransportError: tc.err}, &protocol.Response{}, protocol.ErrModelUnavailable)
			meta := readMetadata(t, filepath.Join(root, "worker:P1/call.json"))
			if meta.TransportFailure != tc.name || meta.Error != "model_unavailable" || meta.Usage != nil {
				t.Fatal("wrong failure classification", meta)
			}
			raw, err := os.ReadFile(filepath.Join(root, "worker:P1/call.json"))
			noError(t, err)
			if strings.Contains(string(raw), "private") {
				t.Fatal("metadata leaked error text")
			}
			if _, err := os.Stat(filepath.Join(root, "worker:P1/responses/call.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed transport published response")
			}
		})
	}
}

func TestCaptureDisabledAndInvalidDirectory(t *testing.T) {
	t.Setenv("MAGI_DEBUG_API_MODE", "full")
	t.Setenv("MAGI_DEBUG_API_DIR", "")
	t.Setenv("MAGI_DEBUG_API_PAUSE", "invalid")
	noError(t, (Observer{}).BeforeModelRequest(context.Background(), "call", []byte(fixtureRequest)))
	t.Setenv("MAGI_DEBUG_API_DIR", "relative")
	if err := (Observer{}).BeforeModelRequest(context.Background(), "call", []byte(fixtureRequest)); err == nil {
		t.Fatal("explicit pause setup error was ignored")
	}
	t.Setenv("MAGI_DEBUG_API_PAUSE", "")
	noError(t, (Observer{}).BeforeModelRequest(context.Background(), "call", []byte(fixtureRequest)))
}
