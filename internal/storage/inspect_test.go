package storage

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"magi/internal/core/protocol"
)

func privateFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	must(t, filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[path] = hashBytes(body)
		}
		return nil
	}))
	return result
}

func TestInspectReadsUsageAndRoundIntegrityWithoutWriting(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	p, err := InitProject(ctx, root)
	must(t, err)
	s := p.Disk()
	must(t, s.ObserveUsage("PLANNER", protocol.TokenUsage{Hit: 60, Miss: 40, Output: 20}))
	must(t, s.ObserveUsage("WORKER", protocol.TokenUsage{Miss: 100, Output: 30}))
	must(t, s.ObserveUsage("REVIEWER", protocol.TokenUsage{Hit: 40, Miss: 60, Output: 50}))
	n, err := s.Allocate("WORKER")
	must(t, err)
	err = s.WriteFragment(ctx, n, []byte("private report"))
	must(t, err)
	u, err := s.Allocate("USER")
	must(t, err)
	err = s.WriteFragment(ctx, u, []byte("private user reply"))
	must(t, err)
	var prior *protocol.StoredRound
	var rounds []protocol.StoredRound
	for n := uint64(0); n < 5; n++ {
		r, err := s.Publish(ctx, protocol.StoredRound{Number: n}, []byte{byte(n)}, prior)
		must(t, err)
		rounds = append(rounds, r)
		prior = &rounds[len(rounds)-1]
		must(t, s.Prune(ctx))
	}
	must(t, p.Close())
	dir := filepath.Join(root, ".magi")
	must(t, os.Chmod(filepath.Join(dir, "safe-points", rounds[3].Hash), 0600))
	must(t, os.WriteFile(filepath.Join(dir, "safe-points", rounds[3].Hash), []byte("corrupt"), 0400))
	must(t, os.Remove(filepath.Join(dir, "safe-points", rounds[2].Hash)))
	// Inspection must not run OpenProject's quarantine behavior.
	stage := filepath.Join(root, ".magi-init-abandoned")
	must(t, os.Mkdir(stage, 0700))
	must(t, writeInitializationMarker(stage, stage))
	before := privateFiles(t, dir)
	previous := protocol.Storage
	report, err := Inspect(ctx, root, false)
	must(t, err)
	if protocol.Storage != previous {
		t.Fatal("inspection changed active bindings")
	}
	if report.FormatVersion != protocol.StorageFormatVersion || report.Counters.WorkerNumber != 1 || report.Counters.UserNumber != 1 || report.ProjectID != p.Metadata().ProjectID {
		t.Fatal("incorrect metadata", report)
	}
	if report.Usage.Total.TotalTokens.Uint64() != 400 || report.Usage.Total.CacheHitTokens.Uint64() != 100 || *report.Usage.Total.CacheHitRate != 1.0/3 {
		t.Fatal("incorrect usage", report.Usage)
	}
	if report.Usage.Worker.CacheHitRate == nil || *report.Usage.Worker.CacheHitRate != 0 {
		t.Fatal("incorrect worker usage", report.Usage)
	}
	body, err := json.Marshal(report)
	must(t, err)
	var encoded struct {
		Counters map[string]json.RawMessage `json:"counters"`
		Usage    map[string]json.RawMessage `json:"usage"`
	}
	must(t, json.Unmarshal(body, &encoded))
	for _, role := range []string{"planner", "worker", "reviewer"} {
		if encoded.Counters[role] != nil || encoded.Usage[role] == nil {
			t.Fatal("role usage must appear only in the usage report", role)
		}
	}
	if len(report.Rounds) != 5 {
		t.Fatal(report.Rounds)
	}
	for i, status := range []string{"valid", "corrupt", "missing", "pruned", "pruned"} {
		if report.Rounds[i].SnapshotStatus != status || report.Rounds[i].Retained != (i < 3) || report.Rounds[i].Metadata.Number != uint64(4-i) {
			t.Fatal("incorrect snapshot assessment", report.Rounds)
		}
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("inspection quarantined initialization", err)
	}
	if !reflect.DeepEqual(before, privateFiles(t, dir)) {
		t.Fatal("inspection wrote state")
	}
}

func TestInspectMissingAndBusyStateDoesNotCreateOrWaitIndefinitely(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	if _, err := Inspect(ctx, root, false); err == nil {
		t.Fatal("inspected uninitialized project")
	}
	if _, err := os.Stat(filepath.Join(root, ".magi")); !os.IsNotExist(err) {
		t.Fatal("inspection created state")
	}
	p, err := InitProject(ctx, root)
	must(t, err)
	start := time.Now()
	if _, err = Inspect(ctx, root, true); !protocol.IsCode(err, protocol.CodeLifecycleConflict) {
		t.Fatal("busy state was not rejected", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("unbounded database lock wait")
	}
	must(t, p.Close())
	must(t, os.Remove(filepath.Join(root, ".magi", databaseName)))
	if _, err := Inspect(ctx, root, true); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(filepath.Join(root, ".magi", databaseName)); !os.IsNotExist(err) {
		t.Fatal("inspection created database")
	}
}

func TestInspectValidatesOnlyAccessedPaths(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	p, err := InitProject(ctx, root)
	must(t, err)
	must(t, p.Close())
	dir := filepath.Join(root, ".magi")
	// Neither inspection mode reads history; a broken history link must not
	// prevent diagnosing the independent database and snapshot metadata.
	must(t, os.Symlink("missing", filepath.Join(dir, "history", "broken")))
	for _, usageOnly := range []bool{true, false} {
		_, err := Inspect(ctx, root, usageOnly)
		must(t, err)
	}
	// The project marker is actually read, so links remain forbidden there.
	marker := filepath.Join(dir, "project.json")
	backup := filepath.Join(root, "project-marker")
	must(t, os.Rename(marker, backup))
	must(t, os.Symlink(backup, marker))
	if _, err := Inspect(ctx, root, true); err == nil {
		t.Fatal("inspection followed a project marker link")
	}
}

func TestBboltRejectsCorruptCountersAndRoundMetadata(t *testing.T) {
	cases := map[string]func(*bolt.Tx) error{
		"bucket":        func(tx *bolt.Tx) error { return tx.DeleteBucket(roundsBucket) },
		"missing-user":  func(tx *bolt.Tx) error { return tx.Bucket(projectBucket).Delete(userKey) },
		"user-length":   func(tx *bolt.Tx) error { return tx.Bucket(projectBucket).Put(userKey, []byte{0}) },
		"user-overflow": func(tx *bolt.Tx) error { return tx.Bucket(projectBucket).Put(userKey, numberKey(math.MaxUint64)) },
		"missing-role":  func(tx *bolt.Tx) error { return tx.Bucket(projectBucket).Delete(roleKeys[2]) },
		"round-key":     func(tx *bolt.Tx) error { return tx.Bucket(roundsBucket).Put([]byte("bad"), []byte(`{}`)) },
		"round": func(tx *bolt.Tx) error {
			return tx.Bucket(roundsBucket).Put(numberKey(0), []byte(`{"hash":"bad","committed_at":"2026-10-12T00:00:00Z","dormant":true}`))
		},
	}
	cases["role-overflow"] = func(tx *bolt.Tx) error {
		return tx.Bucket(projectBucket).Put(roleKeys[1], []byte(`{"number":0,"cache_hit_tokens":9223372036854775808,"cache_miss_tokens":0,"output_tokens":0}`))
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, root := context.Background(), t.TempDir()
			p, err := InitProject(ctx, root)
			must(t, err)
			must(t, p.Disk().db.Update(damage))
			must(t, p.Close())
			before := privateFiles(t, filepath.Join(root, ".magi"))
			if _, err := Inspect(ctx, root, false); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
				t.Fatal("damaged state accepted", err)
			}
			if !reflect.DeepEqual(before, privateFiles(t, filepath.Join(root, ".magi"))) {
				t.Fatal("inspection changed corrupt state")
			}
		})
	}
}

func TestUsageReportPreservesLargeTotals(t *testing.T) {
	u := protocol.TokenUsage{Hit: math.MaxInt64, Miss: math.MaxInt64, Output: math.MaxInt64}
	report := usageReport(Counters{Planner: u, Worker: u, Reviewer: u})
	if report.Total.TotalTokens.String() != "83010348331692982263" {
		t.Fatal("usage total overflow", report.Total)
	}
	body, err := json.Marshal(report)
	must(t, err)
	if _, err := protocol.DecodeJSON(body); err != nil {
		t.Fatal("invalid exact totals", err)
	}
	if *report.Total.CacheHitRate != 0.5 {
		t.Fatal(report.Total.CacheHitRate)
	}
	if usageReport(Counters{}).Total.CacheHitRate != nil {
		t.Fatal("empty usage has hit rate")
	}
}
