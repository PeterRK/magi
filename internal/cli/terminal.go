package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// Bubbles owns editing and Bubble Tea owns raw mode, paste decoding and redraw.
// Only complete accepted messages cross the existing workflow input boundary.
type terminalInput struct {
	area      textarea.Model
	source    *workflowInput
	program   *tea.Program
	output    *terminalWriter
	ready     chan struct{}
	original  string // Preserve an untouched whole-message paste verbatim.
	err       error
	queued    []string
	ending    bool
	dispatch  chan string
	interrupt context.CancelFunc
}

type showTerminalMessage struct {
	text string
	done chan error
}

type terminalMessageShown struct{ done chan error }
type terminalMessageDelivered struct{}

func (w *workflowInput) startTerminal(ctx context.Context, input *stdin, interrupt context.CancelFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w.output, "Enter adds a newline; Ctrl-D submits the message, or ends input when empty. Ctrl-C cancels."); err != nil {
		return err
	}
	readerCtx, cancel := context.WithCancel(ctx)
	// Bubble Tea interrupts and joins terminal reads itself. Its graceful Quit
	// path must settle the reader before closing the cancellation descriptors.
	cleanup, err := input.open(context.WithoutCancel(ctx))
	if err != nil {
		cancel()
		return err
	}
	area := textarea.New()
	area.Prompt, area.ShowLineNumbers = "> ", false
	area.CharLimit, area.MaxHeight, area.MaxWidth = 0, 0, 0
	area.SetWidth(80)
	area.SetHeight(3)
	area.Focus()
	area.SetVirtualCursor(false)
	area.KeyMap.Paste.SetEnabled(false) // Terminal paste only; no system clipboard command.
	t := &terminalInput{area: area, source: w, output: &terminalWriter{writer: w.output}, ready: make(chan struct{}), dispatch: make(chan string, 1), interrupt: interrupt}
	var output io.Writer = t.output
	if file, ok := w.output.(*os.File); ok {
		output = terminalFileWriter{t.output, file}
	}
	t.program = tea.NewProgram(t, tea.WithInput(input.source), tea.WithOutput(output), tea.WithoutSignalHandler())
	w.terminal, w.cancel, w.done = t, cancel, make(chan struct{})
	quitDone := make(chan struct{})
	stopQuit := context.AfterFunc(readerCtx, func() {
		t.program.Quit()
		close(quitDone)
	})
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		for {
			select {
			case text := <-t.dispatch:
				if !w.deliver(readerCtx, text) {
					return
				}
				t.program.Send(terminalMessageDelivered{})
			case <-readerCtx.Done():
				return
			}
		}
	}()
	go func() {
		_, err := t.program.Run()
		if !stopQuit() {
			<-quitDone
		}
		interrupted := readerCtx.Err() != nil
		cancel()
		<-deliveryDone
		if interrupted {
			err = nil // Run cancellation belongs to the caller, not input failure.
		}
		w.readErr = errors.Join(t.err, err, t.output.failure())
		cleanup()
		// Both producers have stopped. Abandoned editor submissions can no
		// longer be delivered; wake finish rather than leave stale send counts.
		w.mu.Lock()
		w.closing = true
		if w.pending > 0 {
			w.pending = 0
			close(w.settled)
		}
		close(w.messages)
		w.mu.Unlock()
		close(w.done)
	}()
	select {
	case <-t.ready:
		return nil
	case <-w.done:
		cancel()
		return w.readErr
	}
}

func (t *terminalInput) Init() tea.Cmd {
	close(t.ready)
	return nil
}

func (t *terminalInput) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case terminalMessageDelivered:
		t.queued[0] = ""
		t.queued = t.queued[1:]
		if len(t.queued) > 0 {
			t.dispatch <- t.queued[0]
			return t, nil
		}
		if t.ending {
			return t, tea.Quit
		}
		return t, nil
	case showTerminalMessage:
		return t, tea.Sequence(tea.Println(msg.text), func() tea.Msg { return terminalMessageShown{msg.done} })
	case terminalMessageShown:
		msg.done <- t.output.failure()
		return t, nil
	case tea.WindowSizeMsg:
		t.area.SetWidth(msg.Width)
		t.area.SetHeight(max(1, min(6, msg.Height-2)))
	case tea.KeyPressMsg:
		if t.ending && msg.String() != "ctrl+c" {
			return t, nil
		}
		switch msg.String() {
		case "ctrl+c":
			t.err = context.Canceled
			if t.interrupt != nil {
				t.interrupt()
			}
			return t, tea.Quit
		case "ctrl+d":
			text := t.original
			if text == "" {
				text = t.area.Value()
			}
			if text == "" {
				if len(t.queued) > 0 {
					t.ending = true
					return t, nil
				}
				return t, tea.Quit
			}
			if !t.source.accept() {
				return t, tea.Quit
			}
			t.queued = append(t.queued, text)
			t.area.Reset()
			t.original = ""
			if len(t.queued) == 1 {
				t.dispatch <- t.queued[0]
				return t, nil
			}
			return t, nil
		}
	case tea.PasteMsg:
		if t.ending || msg.Content == "" {
			return t, nil
		}
		if err := validateInputText([]byte(msg.Content)); err != nil {
			t.err = err
			return t, tea.Quit
		}
		if t.area.Value() == "" {
			t.original = msg.Content
		} else {
			t.original = ""
		}
		// Present CRLF as one line break. An unedited paste keeps its original
		// bytes on submission; edits submit the text displayed by the editor.
		msg.Content = strings.ReplaceAll(msg.Content, "\r\n", "\n")
		var cmd tea.Cmd
		t.area, cmd = t.area.Update(msg)
		return t, cmd
	}
	var before string
	if t.original != "" {
		before = t.area.Value()
	}
	var cmd tea.Cmd
	t.area, cmd = t.area.Update(message)
	if t.original != "" && t.area.Value() != before {
		t.original = ""
	}
	return t, cmd
}

func (t *terminalInput) View() tea.View {
	view := tea.NewView(t.area.View())
	view.Cursor = t.area.Cursor()
	return view
}

func (t *terminalInput) show(text string) error {
	select {
	case <-t.source.done:
		_, err := fmt.Fprintln(t.source.output, text)
		return err
	default:
	}
	done := make(chan error, 1)
	t.program.Send(showTerminalMessage{text, done})
	select {
	case err := <-done:
		return err
	case <-t.source.done:
		if t.source.readErr != nil {
			return t.source.readErr
		}
		return io.EOF
	}
}

// Bubble Tea does not propagate every renderer write failure. Retain the first
// one so show_user and shutdown keep the CLI's ordinary I/O error contract.
type terminalWriter struct {
	mu     sync.Mutex
	writer io.Writer
	err    error
}

func (w *terminalWriter) Write(body []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(body)
	if w.err == nil {
		w.err = err
	}
	return n, err
}

func (w *terminalWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Preserve the file interface so Bubble Tea can detect and resize real terminals.
type terminalFileWriter struct {
	*terminalWriter
	*os.File
}

func (w terminalFileWriter) Write(body []byte) (int, error) {
	return w.terminalWriter.Write(body)
}
