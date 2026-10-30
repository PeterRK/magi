package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"magi/internal/core/engine"
)

func (r *Runner) recoverBeforeEngine(ctx context.Context, store *workspaceState, input *stdin, fault error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	candidates, err := engine.RecoveryCandidates(ctx, store.Disk(), store.Metadata().ProjectID)
	if err != nil {
		return errors.Join(fault, err)
	}
	if _, err = fmt.Fprintf(r.Err, "Magi startup state check failed: %v\nRetained complete safety points:\n", fault); err != nil {
		return errors.Join(fault, err)
	}
	usable := false
	for _, candidate := range candidates {
		status := "valid"
		if candidate.Err != nil {
			status = "unavailable: " + candidate.Err.Error()
		} else {
			usable = true
		}
		if _, err = fmt.Fprintf(r.Err, "  round %d  %s  %s  %s\n", candidate.Point.Number, candidate.Point.CommittedAt.Format("2006-01-02T15:04:05Z07:00"), candidate.Point.Hash, status); err != nil {
			return errors.Join(fault, err)
		}
	}
	if !usable {
		return fmt.Errorf("%w; no valid retained safety point is available", fault)
	}
	control := input
	if r.recoveryInput != nil {
		control = newStdin(r.recoveryInput)
		defer control.close()
	} else if !input.terminal {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return fmt.Errorf("%w; recovery requires an interactive terminal; workflow stdin was not consumed", fault)
		}
		defer tty.Close()
		control = newStdin(tty)
		defer control.close()
	}
	if _, err = fmt.Fprintln(r.Err, "Recover from an explicitly selected round number, or press Enter to abort. Agent memory may move back; workspace effects and global numbering will remain. Restore round:"); err != nil {
		return errors.Join(fault, err)
	}
	body, err := control.readLine(ctx)
	if err != nil {
		return err
	}
	choice := strings.TrimSpace(string(body))
	if choice == "" {
		return fault
	}
	number, err := strconv.ParseUint(choice, 10, 64)
	if err != nil {
		return fmt.Errorf("%w; invalid recovery selection", fault)
	}
	if err = engine.Recover(ctx, store.Disk(), store.Metadata().ProjectID, number); err != nil {
		return err
	}
	if err = store.checkState(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.Err, "Recovered from round %d; startup state recheck passed.\n", number)
	return err
}
