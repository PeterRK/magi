package storage

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"

	"magi/internal/core/protocol"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Store tests supply the layout normally created and published by Project.
func storeDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"history", "safe-points"} {
		must(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	return dir
}

func TestMissingHistoryReferenceDoesNotImplyDamagedState(t *testing.T) {
	ctx, dir := context.Background(), storeDirectory(t)
	s, err := initStore(ctx, dir)
	must(t, err)
	defer s.Close()
	if _, err = s.ReadFragment(ctx, "P1"); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
		t.Fatal("nonexistent reference was treated as corruption", err)
	}
	for _, ref := range []string{"P0", "P01", "X1", "../project.json", "P1/child"} {
		if _, err := s.ReadFragment(ctx, ref); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
			t.Fatal("accepted an invalid read label", ref, err)
		}
		if err := s.WriteFragment(ctx, ref, []byte("invalid")); !protocol.IsCode(err, protocol.CodeInvalidCommand) {
			t.Fatal("accepted an invalid write label", ref, err)
		}
	}
	// An existing unreadable entry remains a storage integrity failure.
	path := filepath.Join(dir, "history", "P1")
	must(t, os.MkdirAll(path, 0700))
	if _, err = s.ReadFragment(ctx, "P1"); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("real read failure was softened into a missing reference", err)
	}
}

func TestRoundsRetentionAndIndependentRestore(t *testing.T) {
	ctx := context.Background()
	dir := storeDirectory(t)
	s, err := initStore(ctx, dir)
	must(t, err)

	var prior *protocol.StoredRound
	var history []protocol.StoredRound
	for n := uint64(0); n < 5; n++ {
		body := []byte{byte(n), 'f', 'u', 'l', 'l'}
		r, err := s.Publish(ctx, protocol.StoredRound{Number: n}, body, prior)
		must(t, err)
		history = append(history, r)
		prior = &history[len(history)-1]
		must(t, s.Prune(ctx))
	}
	spill := filepath.Join(dir, "history", "P1")
	must(t, os.WriteFile(spill, []byte("original"), 0400))
	must(t, s.Prune(ctx))
	must(t, s.Close())
	for i, r := range history {
		_, err = os.Stat(filepath.Join(dir, "safe-points", r.Hash))
		if (i < 2) != os.IsNotExist(err) {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	s, err = openStore(ctx, dir)
	must(t, err)
	defer s.Close()
	latest, found, err := s.Latest(ctx)
	must(t, err)
	if !found || latest.Number != 4 {
		t.Fatalf("latest=%+v found=%v", latest, found)
	}
	body, err := s.Read(ctx, latest.Hash)
	must(t, err)
	if string(body) != string([]byte{4, 'f', 'u', 'l', 'l'}) {
		t.Fatalf("snapshot=%q", body)
	}
	rounds, err := s.rounds(ctx, 0)
	must(t, err)
	if len(rounds) != 5 {
		t.Fatal(len(rounds))
	}
	_, err = os.Stat(spill)
	must(t, err)
	must(t, os.Remove(filepath.Join(dir, "safe-points", latest.Hash)))
	if _, err = s.Read(ctx, latest.Hash); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatalf("latest missing: %v", err)
	}
	r, found, err := s.Latest(ctx)
	must(t, err)
	if !found || r.Number != 4 {
		t.Fatal("fell back to an older round")
	}
}

func TestPruneLeavesOlderResidualsAndProtectsSharedRetainedSnapshot(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "older-residual", true: "shared-retained"}[shared], func(t *testing.T) {
			ctx, dir := context.Background(), storeDirectory(t)
			s, err := initStore(ctx, dir)
			must(t, err)
			defer s.Close()
			var history []protocol.StoredRound
			var prior *protocol.StoredRound
			for n := uint64(0); n < 5; n++ {
				body := []byte{byte(n)}
				if shared && n == 4 {
					body = []byte{1}
				}
				r, err := s.Publish(ctx, protocol.StoredRound{Number: n}, body, prior)
				must(t, err)
				history = append(history, r)
				prior = &history[len(history)-1]
			}
			// Simulate missed cleanup; only the newly expired fourth row is targeted.
			must(t, s.Prune(ctx))
			for i, r := range history {
				_, err = os.Stat(filepath.Join(dir, "safe-points", r.Hash))
				if i == 1 && !shared {
					if !os.IsNotExist(err) {
						t.Fatalf("fourth latest snapshot remains: %v", err)
					}
				} else {
					must(t, err)
				}
			}
			must(t, s.Prune(ctx)) // Missing expired file is harmless on retry.
		})
	}
}

func TestPublishRejectsRepeatedAndConflictingRounds(t *testing.T) {
	ctx := context.Background()
	s, err := initStore(ctx, storeDirectory(t))
	must(t, err)
	defer s.Close()

	r, err := s.Publish(ctx, protocol.StoredRound{Number: 0}, []byte("complete"), nil)
	must(t, err)
	if _, err := s.Publish(ctx, protocol.StoredRound{Number: 0}, []byte("complete"), nil); !protocol.IsCode(err, protocol.CodeLifecycleConflict) {
		t.Fatal("repeated publication was accepted", err)
	}
	if _, err = s.Publish(ctx, protocol.StoredRound{Number: 0}, []byte("changed"), nil); err == nil {
		t.Fatal("replaced committed round")
	}
	if _, err = s.Publish(ctx, protocol.StoredRound{Number: 1}, []byte("next"), nil); err == nil {
		t.Fatal("accepted wrong predecessor")
	}
	wrong := r
	wrong.Hash = hashBytes([]byte("another baseline"))
	if _, err = s.Publish(ctx, protocol.StoredRound{Number: 1}, []byte("next"), &wrong); err == nil {
		t.Fatal("accepted predecessor with a different snapshot")
	}
	if _, err = s.Publish(ctx, protocol.StoredRound{Number: 2}, []byte("next"), &r); err == nil {
		t.Fatal("skipped a round")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Publish(canceled, protocol.StoredRound{Number: 1}, []byte("next"), &r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	latest, _, err := s.Latest(ctx)
	must(t, err)
	if latest != r {
		t.Fatal("failure changed latest")
	}
}

func TestCountersAndBatchedRoleUsage(t *testing.T) {
	ctx := context.Background()
	dir := storeDirectory(t)
	s, err := initStore(ctx, dir)
	must(t, err)
	for i, source := range []string{"PLANNER", "WORKER", "REVIEWER", "USER"} {
		for range (i + 1) * 10 {
			_, err := s.Allocate(source)
			must(t, err)
		}
	}
	must(t, s.ReserveNumbers(ctx))
	must(t, s.ReserveNumbers(ctx))
	must(t, s.ObserveUsage("PLANNER", protocol.TokenUsage{Hit: 2, Miss: 3, Output: 4}))
	must(t, s.ObserveUsage("PLANNER", protocol.TokenUsage{Hit: 5, Miss: 6, Output: 7}))
	must(t, s.ObserveUsage("WORKER", protocol.TokenUsage{Hit: 8, Miss: 9, Output: 10}))
	must(t, s.ObserveUsage("REVIEWER", protocol.TokenUsage{Hit: 11, Miss: 12, Output: 13}))
	g, err := s.Counters(ctx)
	must(t, err)
	if g.Planner != (protocol.TokenUsage{}) {
		t.Fatal("usage wrote before flush")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if !errors.Is(s.FlushUsage(canceled), context.Canceled) {
		t.Fatal("canceled flush")
	}
	must(t, s.FlushUsage(ctx))
	must(t, s.FlushUsage(ctx))
	must(t, s.Close())
	s, err = openStore(ctx, dir)
	must(t, err)
	defer s.Close()
	g, err = s.Counters(ctx)
	must(t, err)
	if g.Planner != (protocol.TokenUsage{Hit: 7, Miss: 9, Output: 11}) || g.Worker != (protocol.TokenUsage{Hit: 8, Miss: 9, Output: 10}) || g.Reviewer != (protocol.TokenUsage{Hit: 11, Miss: 12, Output: 13}) {
		t.Fatalf("usage=%+v", g)
	}
	u, err := s.Allocate("USER")
	must(t, err)
	if u != "U41" {
		t.Fatal("user numbering did not resume above durable highwater", u)
	}
	if g.PlannerNumber != 10 || g.WorkerNumber != 20 || g.ReviewerNumber != 30 || g.UserNumber != 40 {
		t.Fatal("counter went backward")
	}
}

func TestUsageOverflowRollsBackBatchAndKeepsPendingUsage(t *testing.T) {
	ctx := context.Background()
	s, err := initStore(ctx, storeDirectory(t))
	must(t, err)
	defer s.db.Close()
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		role, err := readRole(tx, roleKeys[1])
		if err != nil {
			return err
		}
		role.Hit = math.MaxInt64
		return putRole(tx, roleKeys[1], role)
	}))
	must(t, s.ObserveUsage("PLANNER", protocol.TokenUsage{Hit: 5}))
	must(t, s.ObserveUsage("WORKER", protocol.TokenUsage{Hit: 1}))
	if err := s.FlushUsage(ctx); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("accepted overflowing usage", err)
	}
	g, err := s.Counters(ctx)
	must(t, err)
	if g.Planner.Hit != 0 || g.Worker.Hit != math.MaxInt64 || s.pending[0].Hit != 5 || s.pending[1].Hit != 1 {
		t.Fatal("partially committed or lost pending usage", g, s.pending)
	}
}

func TestSnapshotPrivatePaths(t *testing.T) {
	ctx := context.Background()
	dir := storeDirectory(t)
	s, err := initStore(ctx, dir)
	must(t, err)
	defer s.Close()
	outside := t.TempDir()
	must(t, os.Remove(filepath.Join(dir, "safe-points")))
	must(t, os.Symlink(outside, filepath.Join(dir, "safe-points")))
	if _, err = s.Publish(ctx, protocol.StoredRound{Number: 0}, []byte("snapshot"), nil); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("wrote through private symlink")
	}
}

func TestWritesDoNotRecreateMissingPrivateDirectories(t *testing.T) {
	for _, directory := range []string{"history", "safe-points"} {
		t.Run(directory, func(t *testing.T) {
			ctx := context.Background()
			project, err := InitProject(ctx, t.TempDir())
			must(t, err)
			defer project.Close()
			disk := project.Disk()
			path := filepath.Join(project.Root(), ".magi", directory)
			must(t, os.Remove(path))
			if directory == "history" {
				ref, allocateErr := disk.Allocate("WORKER")
				must(t, allocateErr)
				err = disk.WriteFragment(ctx, ref, []byte("report"))
			} else {
				_, err = disk.Publish(ctx, protocol.StoredRound{Number: 0}, []byte("snapshot"), nil)
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("write did not report the missing directory", err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("write recreated a missing private directory", err)
			}
			if _, found, err := disk.Latest(ctx); err != nil || found {
				t.Fatal("failed write published a safety point", err)
			}
		})
	}
}

func TestConcurrentNumberingDurableBeforeFilesAndNoReuse(t *testing.T) {
	ctx := context.Background()
	dir := storeDirectory(t)
	s, err := initStore(ctx, dir)
	must(t, err)
	var wg sync.WaitGroup
	refs := make(chan string, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := s.Allocate("WORKER")
			if err != nil {
				t.Error(err)
				return
			}
			if err = s.WriteFragment(ctx, ref, []byte("original")); err != nil {
				t.Error(err)
				return
			}
			refs <- ref
		}()
	}
	wg.Wait()
	close(refs)
	seen := map[string]bool{}
	for ref := range refs {
		if seen[ref] {
			t.Fatal("duplicate label")
		}
		seen[ref] = true
	}
	if len(seen) != 24 {
		t.Fatal("missing fragments", len(seen))
	}
	entries, err := os.ReadDir(filepath.Join(dir, "history"))
	must(t, err)
	if len(entries) != len(seen) {
		t.Fatal("history files were not stored in one directory")
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !seen[entry.Name()] {
			t.Fatal("history filename differs from its complete label", entry.Name())
		}
	}
	g, err := s.Counters(ctx)
	must(t, err)
	if g.WorkerNumber < 24 {
		t.Fatal("files preceded durable upper bound")
	}
	if err = s.WriteFragment(ctx, "W1", []byte("replacement")); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal(err)
	}
	// Allocate without writing. A later file flush may reserve the gap too.
	gap, err := s.Allocate("WORKER")
	must(t, err)
	last, err := s.Allocate("WORKER")
	must(t, err)
	err = s.WriteFragment(ctx, last, []byte("last"))
	must(t, err)
	must(t, s.Close())
	s, err = openStore(ctx, dir)
	must(t, err)
	defer s.Close()
	next, err := s.Allocate("WORKER")
	must(t, err)
	if gap != "W25" || last != "W26" || next != "W27" {
		t.Fatal("reused persisted gap", next, gap, last)
	}
	// Failure after the counter commit must consume its number too.
	p, err := s.Allocate("PLANNER")
	must(t, err)
	must(t, os.Symlink(t.TempDir(), filepath.Join(dir, "history", "P1")))
	if err = s.WriteFragment(ctx, p, []byte("blocked")); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal(err)
	}
	g, err = s.Counters(ctx)
	must(t, err)
	if p != "P1" || g.PlannerNumber != 1 {
		t.Fatal("failed write did not reserve number")
	}
}

func TestOpenRequiresInitializedDatabase(t *testing.T) {
	ctx, dir := context.Background(), storeDirectory(t)
	if store, err := openStore(ctx, dir); err == nil {
		store.Close()
		t.Fatal("Open created missing database")
	}
	if _, err := os.Lstat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("Open left a database: %v", err)
	}
	store, err := initStore(ctx, dir)
	must(t, err)
	must(t, store.ObserveUsage("WORKER", protocol.TokenUsage{Miss: 7}))
	must(t, store.Close())
	if duplicate, err := initStore(ctx, dir); err == nil {
		duplicate.Close()
		t.Fatal("Init replaced an existing database")
	}
	store, err = openStore(ctx, dir)
	must(t, err)
	defer store.Close()
	counters, err := store.Counters(ctx)
	must(t, err)
	if counters.Worker.Miss != 7 {
		t.Fatalf("existing database changed: %+v", counters)
	}
}

func TestDecodeRecordRejectsMalformedFields(t *testing.T) {
	for name, body := range map[string]string{
		"fraction":        `{"number":0.5,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
		"overflow":        `{"number":18446744073709551616,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
		"wrong-type":      `{"number":"0","cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
		"negative":        `{"number":-1,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
		"null":            `{"number":null,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
		"missing-field":   `{"number":0,"cache_hit_tokens":0,"cache_miss_tokens":0}`,
		"unknown-field":   `{"number":0,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0,"extra":0}`,
		"duplicate-field": `{"number":0,"number":1,"cache_hit_tokens":0,"cache_miss_tokens":0,"output_tokens":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			var record roleRecord
			if err := decodeRecord([]byte(body), map[string]any{"number": &record.Number, "cache_hit_tokens": &record.Hit, "cache_miss_tokens": &record.Miss, "output_tokens": &record.Output}); err == nil {
				t.Fatal("accepted malformed role record")
			}
		})
	}
}
