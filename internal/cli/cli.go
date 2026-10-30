// Package cli owns the current-directory entry, startup and terminal input.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"magi/internal/core/protocol"
	"magi/internal/storage"
)

type Runner struct {
	In       io.Reader
	Out, Err io.Writer

	modelFactory  func() (protocol.Model, error)
	recoveryInput io.Reader // Tests inject control input separately from workflow stdin.
}

func New(out, err io.Writer) *Runner { return &Runner{Out: out, Err: err} }

func (r *Runner) Run(ctx context.Context, args []string) (returnErr error) {
	name := "magi"
	initialize := len(args) > 0 && args[0] == "init"
	inspect := len(args) > 0 && args[0] == "inspect"
	if initialize {
		name = "magi init"
		args = args[1:]
	}
	if inspect {
		name = "magi inspect"
		args = args[1:]
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(r.Err)
	usageOnly := false
	if inspect {
		flags.BoolVar(&usageOnly, "usage", false, "output only batched role token usage and cache hit rates")
	}
	flags.Usage = func() {
		fmt.Fprintln(r.Err, "Usage: magi init | magi inspect [--usage] | magi\nUse the current directory. inspect emits read-only JSON. Plain magi cold-starts or resumes; Planner interaction uses stdin.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return protocol.InvalidCommand("only magi init, magi inspect and plain magi are supported")
	}
	if inspect {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		report, err := storage.Inspect(ctx, root, usageOnly)
		if err != nil {
			return err
		}
		if usageOnly {
			return writeJSON(r.Out, report.Usage)
		}
		return writeJSON(r.Out, report)
	}
	if initialize {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := storage.InitProject(ctx, root)
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, store.Close()) }()
		return writeJSON(r.Out, map[string]any{"project_id": store.Metadata().ProjectID, "project_root": store.Root(), "format_version": store.Metadata().FormatVersion})
	}
	return r.launch(ctx)
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := New(os.Stdout, os.Stderr)
	runner.In = os.Stdin
	if err := runner.Run(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
