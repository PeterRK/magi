package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type interruptibleInput struct {
	entered chan struct{}
	closed  chan struct{}
	exited  chan struct{}
}

func (r *interruptibleInput) Read([]byte) (int, error) {
	close(r.entered)
	<-r.closed
	close(r.exited)
	return 0, io.ErrClosedPipe
}

func (r *interruptibleInput) Close() error { close(r.closed); return nil }

func TestInputCancelWaitsForBlockedReadToExit(t *testing.T) {
	r := &interruptibleInput{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	input := newStdin(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := input.readAll(ctx); done <- err }()
	<-r.entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-r.exited:
		default:
			t.Fatal("cancel returned before the input reader exited")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked input was not interrupted")
	}
}

func TestFileInputCancelThenReadKeepsCallerFileOpen(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	input := newStdin(r)
	defer input.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = input.readLine(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("first\r\nsecond")); err != nil {
		t.Fatal(err)
	}
	noError(t, w.Close())
	ctx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	for _, want := range []string{"first", "second", ""} {
		body, err := input.readLine(ctx)
		if err != nil || string(body) != want {
			t.Fatalf("next input stolen or stale cancellation: %q, %v", body, err)
		}
	}
	input.close()
	if _, err = r.Stat(); err != nil {
		t.Fatalf("owned input closed caller's file: %v", err)
	}
}

func TestFileInputPreservesOffsetAndMultilineBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("skip\nfirst\nsecond\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	input := newStdin(f)
	body, err := input.readAll(context.Background())
	input.close()
	if err != nil || string(body) != "first\nsecond\r\n" {
		t.Fatalf("redirected input altered: %q, %v", body, err)
	}
	if _, err = f.Stat(); err != nil {
		t.Fatal("caller file closed", err)
	}
}

func TestNullInputIsEmptyAndDoesNotCloseCaller(t *testing.T) {
	f, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	input := newStdin(f)
	if input.terminal {
		t.Fatal("nonterminal character device treated as a terminal")
	}
	body, err := input.readAll(context.Background())
	input.close()
	if err != nil || len(body) != 0 {
		t.Fatal("empty device changed", body, err)
	}
	if _, err = f.Stat(); err != nil {
		t.Fatal("caller file closed", err)
	}
}

func TestInputReaderRetainsErrorsAfterQueuedMessage(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	input := newStdin(reader)
	input.terminal = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	source, err := newWorkflowInput(ctx, input, nil, io.Discard, cancel)
	noError(t, err)
	for _, text := range []string{"first", "second", "third"} {
		_, err = io.WriteString(writer, "\x1b[200~"+text+"\x1b[201~\x04")
		noError(t, err)
	}
	// Leave the public buffer full and another submission waiting in the
	// editor when input fails. Neither can make further progress after exit.
	for {
		source.mu.Lock()
		pending := source.pending
		source.mu.Unlock()
		if pending >= 2 {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			t.Fatal("input did not queue the submissions")
		}
	}
	failure := errors.New("input failed")
	noError(t, writer.CloseWithError(failure))
	select {
	case <-source.done:
	case <-ctx.Done():
		t.Fatal("read failure did not stop input")
	}
	if got := terminalMessage(t, ctx, source.messages); got != "first" {
		t.Fatal("read failure lost the buffered message", got)
	}
	finished := make(chan bool, 1)
	go func() { finished <- source.finish() }()
	select {
	case ok := <-finished:
		if !ok {
			t.Fatal("stopped input requested another run")
		}
	case <-ctx.Done():
		t.Fatal("finish waited for an abandoned submission")
	}
	if err := source.close(); !errors.Is(err, failure) {
		t.Fatal("CLI lost the underlying input error", err)
	}
}

func TestInputFinishPreservesBufferedAndBlockedMessages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source, err := newWorkflowInput(context.Background(), newStdin(strings.NewReader("")), nil, io.Discard, nil)
		noError(t, err)
		defer func() { noError(t, source.close()) }()
		enqueue := func(text string) bool {
			return source.accept() && source.deliver(context.Background(), text)
		}
		if !enqueue("first") {
			t.Fatal("first message refused")
		}
		sent := make(chan bool, 1)
		go func() { sent <- enqueue("second") }()
		synctest.Wait()
		if source.finish() {
			t.Fatal("exit discarded buffered input")
		}
		if got := <-source.messages; got != "first" {
			t.Fatal(got)
		}
		// Finish must settle a sender racing with the last dequeue, rather
		// than miss its message or wake Planner on a stale pending flag.
		if source.finish() {
			t.Fatal("exit discarded the blocked complete message")
		}
		if got := <-source.messages; got != "second" || !<-sent {
			t.Fatal("second message lost", got)
		}
		if !source.finish() || enqueue("too late") {
			t.Fatal("normal exit did not close the acceptance boundary")
		}
	})
}
