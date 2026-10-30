package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"magi/internal/core/protocol"
)

const historyPrefixes = "PWRU"

func historySourceIndex(source string) (int, error) {
	switch source {
	case "PLANNER":
		return 0, nil
	case "WORKER":
		return 1, nil
	case "REVIEWER":
		return 2, nil
	case "USER":
		return 3, nil
	}
	return 0, protocol.InvalidCommand("invalid history source")
}

// ReadFragment resolves a complete label directly in the workspace history directory.
func (s *Store) ReadFragment(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, _, err := protocol.ParseHistoryRef(ref); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(s.dir, "history", ref))
	if os.IsNotExist(err) {
		return nil, protocol.InvalidCommand("numbered history reference does not exist")
	}
	if err != nil {
		return nil, protocol.IntegrityFailure("numbered history is unreadable")
	}
	return body, nil
}

// Allocate returns a complete label without I/O; each source has its own highwater.
func (s *Store) Allocate(source string) (string, error) {
	i, err := historySourceIndex(source)
	if err != nil {
		return "", err
	}
	for {
		n := s.numbers[i].Load()
		if n >= math.MaxInt64 {
			return "", protocol.IntegrityFailure("history number exhausted")
		}
		if s.numbers[i].CompareAndSwap(n, n+1) {
			return fmt.Sprintf("%c%d", historyPrefixes[i], n+1), nil
		}
	}
}

// WriteFragment reserves the durable upper bounds before any numbered file is
// created. A failed or interrupted write consumes its number permanently.
func (s *Store) WriteFragment(ctx context.Context, ref string, body []byte) error {
	prefix, n, err := protocol.ParseHistoryRef(ref)
	if err != nil {
		return err
	}
	i := strings.IndexByte(historyPrefixes, prefix)
	if n > s.numbers[i].Load() {
		return protocol.IntegrityFailure("history number was not allocated")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = s.ReserveNumbers(ctx); err != nil {
		return err
	}
	rel := filepath.Join("history", ref)
	if err = s.validate(rel); err != nil {
		return err
	}
	path := filepath.Join(s.dir, rel)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if os.IsExist(err) {
		return protocol.IntegrityFailure("history number already has a file")
	}
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	// Project initialization already published the history directory durably.
	return syncDir(filepath.Dir(path))
}
