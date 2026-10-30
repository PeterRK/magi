// Package storage manages project storage and persists allocation state,
// numbered history, usage and completed rounds.
// It has no knowledge of orders, model calls or unfinished execution.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"

	"magi/internal/core/protocol"
)

const retainedRoundCount = 3

var _ protocol.StorageBackend = (*Store)(nil)

type Store struct {
	db       *bolt.DB
	dir      string
	identity os.FileInfo
	mu       sync.Mutex
	pending  [3]protocol.TokenUsage
	numbers  [4]atomic.Uint64
	durable  [4]uint64
}

// initStore creates the database in an existing private staging directory.
// Project initialization owns publication of that directory.
func initStore(ctx context.Context, dir string) (*Store, error) {
	return open(ctx, dir, true, false)
}

// openStore requires an existing initialized database.
func openStore(ctx context.Context, dir string) (*Store, error) {
	return open(ctx, dir, false, false)
}

func open(ctx context.Context, dir string, create, readOnly bool) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, protocol.IntegrityFailure("invalid state directory")
	}
	s := &Store{dir: dir, identity: info}
	if err = s.validate(databaseName); err != nil {
		return nil, err
	}
	timeout := time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
		if timeout <= 0 {
			return nil, context.DeadlineExceeded
		}
	}
	s.db, err = bolt.Open(filepath.Join(dir, databaseName), 0600, &bolt.Options{
		ReadOnly: readOnly, Timeout: timeout,
		OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
			if create {
				flags |= os.O_EXCL
			} else {
				flags &^= os.O_CREATE
			}
			f, err := os.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err == nil {
				err = validatePrivateEntry(name, info)
			}
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			return f, nil
		},
	})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, protocol.LifecycleConflict("state database is in use; wait for the active Magi process to exit, then retry")
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) { _ = s.db.Close(); return nil, err }
	if create {
		err = s.db.Update(func(tx *bolt.Tx) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return initializeMetadata(tx)
		})
		if err != nil {
			return fail(err)
		}
		if err = syncDir(dir); err != nil {
			return fail(err)
		}
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		if err := validateBuckets(tx); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return fail(err)
	}
	g, err := s.Counters(ctx)
	if err != nil {
		return fail(err)
	}
	s.numbers[0].Store(g.PlannerNumber)
	s.numbers[1].Store(g.WorkerNumber)
	s.numbers[2].Store(g.ReviewerNumber)
	s.numbers[3].Store(g.UserNumber)
	s.durable = [4]uint64{g.PlannerNumber, g.WorkerNumber, g.ReviewerNumber, g.UserNumber}
	return s, nil
}

func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.FlushUsage(ctx)
	return errors.Join(err, s.db.Close())
}

func (s *Store) Counters(ctx context.Context) (Counters, error) {
	var g Counters
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		g, err = readCounters(tx)
		return err
	})
	return g, err
}

// ObserveUsage only changes this run's memory for a fixed persisted role.
// The caller assigns each invocation to its accounting owner.
func (s *Store) ObserveUsage(role string, usage protocol.TokenUsage) error {
	i := 0
	switch role {
	case "PLANNER":
	case "WORKER":
		i = 1
	case "REVIEWER":
		i = 2
	default:
		return protocol.InvalidCommand("invalid usage role")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &s.pending[i]
	if usage.Hit > math.MaxInt64-p.Hit || usage.Miss > math.MaxInt64-p.Miss || usage.Output > math.MaxInt64-p.Output {
		return protocol.IntegrityFailure("pending token usage overflow")
	}
	p.Hit += usage.Hit
	p.Miss += usage.Miss
	p.Output += usage.Output
	return nil
}

// FlushUsage commits one batch. Clear memory only after a confirmed commit.
func (s *Store) FlushUsage(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == [3]protocol.TokenUsage{} {
		return ctx.Err()
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i, p := range s.pending {
			if p == (protocol.TokenUsage{}) {
				continue
			}
			role, err := readRole(tx, roleKeys[i])
			if err != nil {
				return err
			}
			if p.Hit > math.MaxInt64-role.Hit || p.Miss > math.MaxInt64-role.Miss || p.Output > math.MaxInt64-role.Output {
				return protocol.IntegrityFailure("token usage total overflow")
			}
			role.Hit += p.Hit
			role.Miss += p.Miss
			role.Output += p.Output
			if err := putRole(tx, roleKeys[i], role); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err == nil {
		s.pending = [3]protocol.TokenUsage{}
	}
	return err
}

// ReserveNumbers persists the current allocation highwaters before fragment writes.
func (s *Store) ReserveNumbers(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	requested := [4]uint64{s.numbers[0].Load(), s.numbers[1].Load(), s.numbers[2].Load(), s.numbers[3].Load()}
	if requested == s.durable {
		return ctx.Err()
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if requested[3] > s.durable[3] {
			if err := tx.Bucket(projectBucket).Put(userKey, numberKey(requested[3])); err != nil {
				return err
			}
		}
		for i, n := range requested[:3] {
			if n <= s.durable[i] {
				continue
			}
			role, err := readRole(tx, roleKeys[i])
			if err != nil {
				return err
			}
			role.Number = n
			if err := putRole(tx, roleKeys[i], role); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err == nil {
		s.durable = requested
	}
	return err
}

func latestRound(tx *bolt.Tx) (protocol.StoredRound, bool, error) {
	key, body := tx.Bucket(roundsBucket).Cursor().Last()
	if key == nil {
		return protocol.StoredRound{}, false, nil
	}
	r, err := readRound(key, body)
	return r, err == nil, err
}

func (s *Store) Latest(ctx context.Context) (protocol.StoredRound, bool, error) {
	var r protocol.StoredRound
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		r, found, err = latestRound(tx)
		return err
	})
	return r, found, err
}

// Publish saves a complete snapshot before a short metadata transaction. The
// caller owns the envelope and its exact expected predecessor; no history is replayed.
func (s *Store) Publish(ctx context.Context, next protocol.StoredRound, body []byte, expected *protocol.StoredRound) (protocol.StoredRound, error) {
	if next.Number > math.MaxInt64 {
		return next, protocol.InvalidCommand("invalid round metadata")
	}
	next.Hash = hashBytes(body)
	if next.CommittedAt.IsZero() {
		next.CommittedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putSnapshot(ctx, next.Hash, body); err != nil {
		return next, err
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		latest, found, err := latestRound(tx)
		if err != nil {
			return err
		}
		if found && latest.Number == next.Number {
			return protocol.LifecycleConflict("committed round cannot be replaced")
		}
		if (expected == nil && found) || (expected != nil && (!found || latest.Number != expected.Number || latest.Hash != expected.Hash)) {
			return protocol.LifecycleConflict("snapshot predecessor changed")
		}
		if (!found && next.Number != 0) || (found && next.Number != latest.Number+1) {
			return protocol.LifecycleConflict("snapshot is not the next round")
		}
		return putRound(tx, next)
	})
	return next, err
}

func (s *Store) Read(ctx context.Context, hash string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validHash(hash) {
		return nil, protocol.IntegrityFailure("invalid snapshot hash")
	}
	rel := filepath.Join("safe-points", hash)
	if err := s.validate(rel); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(s.dir, rel))
	if err != nil || hashBytes(body) != hash {
		return nil, protocol.IntegrityFailure("committed snapshot is missing or corrupt")
	}
	return body, nil
}

// Prune removes the fourth latest committed snapshot. Index records and older leftovers remain intact.
func (s *Store) Prune(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rounds, err := s.rounds(ctx, retainedRoundCount+1)
	if err != nil {
		return err
	}
	if len(rounds) <= retainedRoundCount {
		return nil
	}
	h := rounds[retainedRoundCount].Hash
	for _, r := range rounds[:retainedRoundCount] {
		if r.Hash == h {
			return nil
		}
	}
	rel := filepath.Join("safe-points", h)
	if err = s.validate(rel); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(s.dir, rel)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return syncDir(filepath.Join(s.dir, "safe-points"))
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHash(h string) bool {
	b, err := hex.DecodeString(h)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == h
}

func (s *Store) validate(rel string) error {
	info, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(info, s.identity) {
		return protocol.IntegrityFailure("state directory changed")
	}
	path := s.dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return protocol.IntegrityFailure("invalid private path")
		}
		path = filepath.Join(path, part)
		info, err = os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := validatePrivateEntry(path, info); err != nil {
			return protocol.IntegrityFailure(err.Error())
		}
	}
	return nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (s *Store) putSnapshot(ctx context.Context, h string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rel := filepath.Join("safe-points", h)
	if err := s.validate(rel); err != nil {
		return err
	}
	path := filepath.Join(s.dir, rel)
	if existing, err := os.ReadFile(path); err == nil {
		if hashBytes(existing) != h {
			return protocol.IntegrityFailure("existing snapshot is corrupt")
		}
		// A previous attempt may have renamed the file but failed its sync.
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close(), syncDir(filepath.Dir(path)))
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0400); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Retained lists only committed candidates within the retention window.
func (s *Store) Retained(ctx context.Context) ([]protocol.StoredRound, error) {
	return s.rounds(ctx, retainedRoundCount)
}

// rounds returns newest first; zero limit includes all committed metadata.
func (s *Store) rounds(ctx context.Context, limit int) ([]protocol.StoredRound, error) {
	result := []protocol.StoredRound{}
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(roundsBucket).Cursor()
		for key, body := cursor.Last(); key != nil && (limit == 0 || len(result) < limit); key, body = cursor.Prev() {
			if err := ctx.Err(); err != nil {
				return err
			}
			r, err := readRound(key, body)
			if err != nil {
				return err
			}
			result = append(result, r)
		}
		return ctx.Err()
	})
	return result, err
}
