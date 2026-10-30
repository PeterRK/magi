package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"magi/internal/core/protocol"
)

type stdin struct {
	source   io.Reader
	opened   bool
	owned    *os.File
	terminal bool
}

func newStdin(in io.Reader) *stdin {
	s := &stdin{source: in}
	if f, ok := in.(*os.File); ok {
		_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
		s.terminal = err == nil
	}
	return s
}

func (s *stdin) close() {
	if s.owned != nil {
		_ = s.owned.Close()
	}
}

// open prepares a cancellable reader without changing terminal settings.
// The caller joins the reader before cleanup and closing the owned descriptor.
func (s *stdin) open(ctx context.Context) (cleanup func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.opened {
		if f, ok := s.source.(*os.File); ok {
			info, err := f.Stat()
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				// Keep nonblocking mode and deadlines independent of caller stdin.
				owned, err := os.OpenFile("/proc/self/fd/"+strconv.Itoa(int(f.Fd())), os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					return nil, err
				}
				s.owned, s.source = owned, owned
			}
		}
		s.opened = true
	}
	var interrupt func()
	if f, ok := s.source.(*os.File); ok {
		if err := f.SetReadDeadline(time.Time{}); err == nil {
			interrupt = func() { _ = f.SetReadDeadline(time.Now()) }
		} else if !errors.Is(err, os.ErrNoDeadline) {
			return nil, err
		}
	} else if closer, ok := s.source.(io.ReadCloser); ok {
		interrupt = func() { _ = closer.Close() }
	}
	join := func() {}
	if interrupt != nil {
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { interrupt(); close(done) })
		join = func() {
			if !stop() {
				<-done
			}
			if s.owned != nil {
				_ = s.owned.SetReadDeadline(time.Time{})
			}
		}
	}
	return join, nil
}

// readAll accepts redirected startup input as one complete message.
func (s *stdin) readAll(ctx context.Context) (body []byte, err error) {
	if s.source == nil {
		return nil, ctx.Err()
	}
	cleanup, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	body, err = io.ReadAll(s.source)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return body, err
}

// Recovery reads only its own line, leaving any subsequent input for the editor.
func (s *stdin) readLine(ctx context.Context) ([]byte, error) {
	if s.source == nil {
		return nil, ctx.Err()
	}
	cleanup, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var body []byte
	var one [1]byte
	for {
		n, err := s.source.Read(one[:])
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if n > 0 {
			body = append(body, one[0])
		}
		if n > 0 && one[0] == '\n' || errors.Is(err, io.EOF) {
			body = bytes.TrimSuffix(body, []byte("\n"))
			return bytes.TrimSuffix(body, []byte("\r")), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// A single terminal editor forms complete messages for the whole run. A waiting
// consumer may leave without cancelling it; partial text and queued replies survive.
type workflowInput struct {
	mu       sync.Mutex
	pending  int // Complete messages accepted but not yet delivered.
	settled  chan struct{}
	closing  bool
	output   io.Writer
	messages chan string
	cancel   context.CancelFunc
	done     chan struct{}
	readErr  error
	terminal *terminalInput
}

func newWorkflowInput(ctx context.Context, input *stdin, held []byte, output io.Writer, interrupt context.CancelFunc) (*workflowInput, error) {
	w := &workflowInput{output: output, messages: make(chan string, 1)}
	if len(held) > 0 {
		w.messages <- string(held)
	}
	if !input.terminal {
		return w, nil
	}
	if err := w.startTerminal(ctx, input, interrupt); err != nil {
		return nil, err
	}
	return w, nil
}

// accept and finish share the acceptance boundary: once finish succeeds, no
// new complete message can enter the channel. Only the editor accepts messages.
func (w *workflowInput) accept() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing {
		return false
	}
	if w.pending == 0 {
		w.settled = make(chan struct{})
	}
	w.pending++
	return true
}

func (w *workflowInput) deliver(ctx context.Context, text string) bool {
	defer func() {
		w.mu.Lock()
		w.pending--
		close(w.settled)
		if w.pending > 0 {
			w.settled = make(chan struct{})
		}
		w.mu.Unlock()
	}()
	select {
	case w.messages <- text:
		return true
	case <-ctx.Done():
		return false
	}
}

// finish permits normal exit only after Planner has consumed all accepted
// messages. A blocked sender counts as pending, even if the buffer was drained.
func (w *workflowInput) finish() bool {
	for {
		w.mu.Lock()
		if len(w.messages) != 0 {
			w.mu.Unlock()
			return false
		}
		if w.pending > 0 {
			pending := w.settled
			w.mu.Unlock()
			// The buffer is empty, so this send can finish without Planner.
			// Settle it before deciding whether a wake-up is needed.
			<-pending
			continue
		}
		w.closing = true
		w.mu.Unlock()
		return true
	}
}

func (w *workflowInput) close() error {
	if w.cancel == nil {
		close(w.messages)
		return nil
	}
	w.cancel()
	<-w.done
	return w.readErr
}

func validateInputText(body []byte) error {
	if len(body) == 0 {
		return protocol.InvalidCommand("empty input")
	}
	if !utf8.Valid(body) {
		return protocol.InvalidCommand("text input must be UTF-8")
	}
	return nil
}

func (w *workflowInput) ShowUser(message string) error {
	if w.terminal != nil {
		return w.terminal.show(message)
	}
	_, err := fmt.Fprintln(w.output, message)
	return err
}
