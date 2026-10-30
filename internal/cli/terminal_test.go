package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"magi/internal/core/characters"
	"magi/internal/core/engine"
	"magi/internal/core/protocol"
)

func openTestTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	noError(t, err)
	t.Cleanup(func() { _ = master.Close() })
	noError(t, unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0))
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	noError(t, err)
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	noError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// Bracketed paste preserves multiline messages; Ctrl-D submits once.
func writeTerminalMessage(master *os.File, text string) (int, error) {
	if text == "" {
		return io.WriteString(master, "\x04")
	}
	return io.WriteString(master, "\x1b[200~"+text+"\x1b[201~\x04")
}

type terminalOutput struct {
	mu    sync.Mutex
	body  bytes.Buffer
	ready chan struct{}
}

func (w *terminalOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.body.Write(p)
	notify := strings.Contains(string(p), "What task?") || strings.Contains(string(p), "Which constraint?") || strings.Contains(string(p), "Restore round:")
	w.mu.Unlock()
	if notify {
		w.ready <- struct{}{}
	}
	return n, err
}
func (w *terminalOutput) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

type terminalResultWriter struct {
	io.Writer
	result func()
}

func (w *terminalResultWriter) Write(p []byte) (int, error) {
	if w.result != nil && bytes.HasPrefix(p, []byte("{")) {
		w.result()
	}
	return w.Writer.Write(p)
}

func TestCLIStopsInputBeforeFinalResult(t *testing.T) {
	isolateBindings(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output := &terminalResultWriter{Writer: io.Discard}
	runner := New(output, io.Discard)
	noError(t, runner.Run(ctx, []string{"init"}))
	model := newScriptedModel(map[string][]protocol.Response{
		"PLANNER/planner":     {controlCallResponse("submit_plan", `{"orders":[],"memo":""}`)},
		"PLANNER/interaction": {controlResponse("show_user", showUserArgs{Message: "Done"})},
	})
	runner.modelFactory = func() (protocol.Model, error) { return model, nil }
	_, slave := openTestTerminal(t)
	runner.In = slave
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	noError(t, err)
	results := 0
	output.result = func() {
		results++
		after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		noError(t, err)
		if *before != *after {
			t.Error("CLI changed terminal settings")
		}
		select {
		case _, ok := <-protocol.UserMessages:
			if ok {
				t.Error("unexpected user message")
			}
		default:
			t.Error("input reader was still running during result output")
		}
	}
	noError(t, runner.Run(ctx, nil))
	if results != 1 {
		t.Fatal("missing final result", results)
	}
}

func TestTerminalEditsAndPreservesWholeMessages(t *testing.T) {
	master, slave := openTestTerminal(t)
	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	noError(t, err)
	input := newStdin(slave)
	defer input.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := newWorkflowInput(ctx, input, nil, io.Discard, cancel)
	noError(t, err)
	defer func() {
		noError(t, source.close())
		after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		noError(t, err)
		if *before != *after {
			t.Fatal("CLI changed terminal settings")
		}
	}()
	// The editor handles character deletion and explicit multiline input.
	_, err = io.WriteString(master, "ab\x7fc\r中文界\x7f第二行\r\x04")
	noError(t, err)
	if got := terminalMessage(t, ctx, source.messages); got != "ac\n中文第二行\n" {
		t.Fatalf("message changed: %q", got)
	}
	_, err = io.WriteString(master, "next")
	noError(t, err)
	_, err = io.WriteString(master, "\x1b[200~\x1b[201~") // Empty paste must preserve the current edit.
	noError(t, err)
	noError(t, source.ShowUser("Progress while editing"))
	select {
	case got := <-source.messages:
		t.Fatalf("partial input escaped: %q", got)
	default:
	}
	_, err = io.WriteString(master, " reply\x04")
	noError(t, err)
	if got := terminalMessage(t, ctx, source.messages); got != "next reply" {
		t.Fatalf("message changed: %q", got)
	}
	// A long paragraph bypasses the kernel's canonical line cap. An untouched
	// paste also preserves source tabs, CRLF and more than 99 logical lines.
	large := strings.Repeat("长段落", 2000) + "\t\r\n" + strings.Repeat("next\n", 120)
	_, err = writeTerminalMessage(master, large)
	noError(t, err)
	if got := terminalMessage(t, ctx, source.messages); got != large {
		t.Fatalf("long paste changed: got %d bytes, want %d", len(got), len(large))
	}
	// Multiple submissions may fill the public channel. Output must still work
	// while the next accepted message waits for Planner to consume the first.
	_, err = writeTerminalMessage(master, "first queued")
	noError(t, err)
	_, err = writeTerminalMessage(master, "second queued")
	noError(t, err)
	noError(t, source.ShowUser("Progress with queued replies"))
	_, err = writeTerminalMessage(master, "")
	noError(t, err)
	for _, want := range []string{"first queued", "second queued"} {
		if got := terminalMessage(t, ctx, source.messages); got != want {
			t.Fatalf("queued message changed order: got %q, want %q", got, want)
		}
	}
	select {
	case _, ok := <-source.messages:
		if ok {
			t.Fatal("empty EOF fabricated a message")
		}
	case <-ctx.Done():
		t.Fatal("EOF did not close input")
	}
}

func TestTerminalCancellationJoinsInput(t *testing.T) {
	for _, keyboard := range []bool{false, true} {
		t.Run(fmt.Sprint(keyboard), func(t *testing.T) {
			master, slave := openTestTerminal(t)
			input := newStdin(slave)
			defer input.close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var output io.Writer = io.Discard
			if keyboard {
				output = slave // Also exercise detection of a real terminal output.
			}
			source, err := newWorkflowInput(ctx, input, nil, output, cancel)
			noError(t, err)
			_, err = io.WriteString(master, "unfinished")
			noError(t, err)
			if keyboard {
				_, err = io.WriteString(master, "\x03")
				noError(t, err)
				<-ctx.Done()
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatal("Ctrl-C did not cancel the run", ctx.Err())
				}
			} else {
				cancel()
			}
			err = source.close()
			if err != nil && !(keyboard && errors.Is(err, context.Canceled)) {
				t.Fatal(err)
			}
			if _, ok := <-source.messages; ok {
				t.Fatal("cancellation published partial input")
			}
		})
	}
}

func terminalMessage(t *testing.T, ctx context.Context, messages <-chan string) string {
	t.Helper()
	select {
	case text, ok := <-messages:
		if !ok {
			t.Fatal("input ended before a complete message")
		}
		return text
	case <-ctx.Done():
		t.Fatal("message never arrived", ctx.Err())
		return ""
	}
}

type lateReplyStorage struct {
	protocol.StorageBackend
	onReview func()
}

func (s *lateReplyStorage) Allocate(source string) (string, error) {
	if source == "REVIEWER" && s.onReview != nil {
		notify := s.onReview
		s.onReview = nil
		notify()
	}
	return s.StorageBackend.Allocate(source)
}

func TestCLIProcessesReplyReceivedAfterFinalInteraction(t *testing.T) {
	isolateBindings(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := &terminalOutput{ready: make(chan struct{}, 1)}
	var result bytes.Buffer
	runner := New(&result, output)
	noError(t, runner.Run(ctx, []string{"init"}))
	result.Reset()
	master, slave := openTestTerminal(t)
	runner.In = slave
	model := newScriptedModel(map[string][]protocol.Response{
		"PLANNER/planner": {
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
		},
		"PLANNER/interaction": {
			controlResponse("show_user", showUserArgs{Message: "Done"}),

			controlResponse("show_user", showUserArgs{Message: "What next?"}),
			controlResponse("show_user", showUserArgs{Message: "Received"}),

			controlResponse("show_user", showUserArgs{Message: "Done"}),
		},

		"PLANNER/interaction_summary": {textResponse("User supplied a late request; see [[U1]].")},
		"REVIEWER/review":             {controlResponse("submit_review", reviewDraft{Summary: "Late request reviewed"})},
	})
	runner.modelFactory = func() (protocol.Model, error) {
		protocol.Storage = &lateReplyStorage{StorageBackend: protocol.Storage, onReview: func() {
			// Empty review happens after Interact returned but before the
			// dormant safety point and CLI's input shutdown.
			_, err := writeTerminalMessage(master, "late user request\n")
			noError(t, err)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for len(protocol.UserMessages) == 0 {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal("terminal did not queue the late reply")
				}
			}
		}}
		return model, nil
	}
	noError(t, runner.Run(ctx, nil))
	app := openTestProject(t, ctx, ".")
	defer app.Close()
	body, err := app.Disk().ReadFragment(ctx, "U1")
	if err != nil || string(body) != "late user request\n" {
		t.Fatal("late reply was discarded instead of archived by Planner", string(body), err)
	}
	var final engine.Workflow
	noError(t, json.Unmarshal(result.Bytes(), &final))
	if final.Phase != engine.PhaseDormant || final.Round != 4 {
		t.Fatal("CLI did not finish the pending-input rounds", final)
	}
}

type terminalModel struct {
	protocol.Model
	answers  []string
	seen     map[string]bool
	requests []characters.InvocationRequest
}

func (m *terminalModel) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	req := characters.CurrentInvocation(ctx).Owner()
	req.Request = input
	captured := req
	captured.Messages = protocol.CloneMessages(req.Messages)
	m.requests = append(m.requests, captured)

	if req.Purpose == "interaction" {
		if m.seen == nil {
			m.seen = map[string]bool{}
		}
		for _, message := range req.Messages {
			ref := messageField(message.Content, "ref")
			if message.Role == "user" && strings.HasPrefix(ref, "U") && !m.seen[ref] {
				m.answers = append(m.answers, messageField(message.Content, "message"))
				m.seen[ref] = true
			}
		}
	}
	return m.Model.Invoke(ctx, req.Request)
}

func TestTerminalRepliesReachOneUserInput(t *testing.T) {
	isolateBindings(t)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out bytes.Buffer
	output := &terminalOutput{ready: make(chan struct{}, 8)}
	runner := New(&out, output)

	noError(t, runner.Run(ctx, []string{"init"}))
	out.Reset()

	const original = "Add a --quiet option.\nKeep exit codes 0 and 2; do not modify config files.\n"
	const followup = "Follow the original request."
	const analysis = "Add --quiet while retaining exit codes 0 and 2 and leaving config files unchanged. The later answer confirms the original request."
	model := newScriptedModel(map[string][]protocol.Response{
		"PLANNER/planner": {
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
			controlCallResponse("submit_plan", `{"orders":[],"memo":""}`),
		},
		"PLANNER/interaction": {
			controlResponse("show_user", showUserArgs{Message: "What task?", WaitForReply: true}),
			controlResponse("show_user", showUserArgs{Message: "Which constraint?", WaitForReply: true}),
			controlResponse("show_user", showUserArgs{Message: "Answers received."}),

			controlResponse("show_user", showUserArgs{Message: "The work is ready."}),
		},

		"PLANNER/interaction_summary": {textResponse(analysis)},
		"REVIEWER/review": {
			controlResponse("submit_review", reviewDraft{Summary: "Input reviewed."}),
		},
	})
	observed := &terminalModel{Model: model}
	runner.modelFactory = func() (protocol.Model, error) { return observed, nil }
	master, slave := openTestTerminal(t)
	runner.In = slave
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, nil) }()
	for _, body := range []string{original, followup} {
		select {
		case <-output.ready:
		case <-ctx.Done():
			t.Fatal("missing interaction question")
		}
		_, err := writeTerminalMessage(master, body)
		noError(t, err)
	}
	select {
	case err := <-done:
		noError(t, err)
	case <-ctx.Done():
		t.Fatal("dialogue never closed")
	}
	if len(observed.answers) != 2 || observed.answers[0] != original || observed.answers[1] != followup {
		t.Fatalf("Planner lost or changed a same-round reply: %q", observed.answers)
	}
	found := false
	for _, req := range observed.requests {
		for _, message := range req.Messages {
			if req.Purpose == "review" && messageField(message.Content, "user_input") == analysis {
				found = true
			}
			if req.Purpose == "planner" && messageField(message.Content, "user_input") != "" {
				t.Fatal("user_input summary was copied into Planner")
			}
		}
	}
	if !found {
		t.Fatal("Reviewer did not receive the single interaction summary")
	}

}

// Non-JSON instructions and source text have no engine fields.
func messageField(content, name string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &fields) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(fields[name], &value) == nil {
		return value
	}
	return string(fields[name])
}
