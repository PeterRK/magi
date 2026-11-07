package storage

import (
	"encoding/binary"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"math"
	"time"

	bolt "go.etcd.io/bbolt"

	"magi/internal/core/protocol"
)

const databaseName = "state.db"

var (
	projectBucket = []byte("project")
	roundsBucket  = []byte("rounds")
	userKey       = []byte("user")
	roleKeys      = [3][]byte{[]byte("planner"), []byte("worker"), []byte("reviewer")}
)

// Counters is an aggregate read view of user and role records, independent of snapshots.
// Inspection renders role usage separately from history numbering.
type Counters struct {
	PlannerNumber  uint64              `json:"planner_number"`
	WorkerNumber   uint64              `json:"worker_number"`
	ReviewerNumber uint64              `json:"reviewer_number"`
	UserNumber     uint64              `json:"user_number"`
	Planner        protocol.TokenUsage `json:"-"`
	Worker         protocol.TokenUsage `json:"-"`
	Reviewer       protocol.TokenUsage `json:"-"`
}

// Each role owns a durable history number and cumulative usage. Updating one
// preserves the other; failed batch transactions preserve all three roles.
type roleRecord struct {
	Number uint64 `json:"number"`
	Hit    uint64 `json:"cache_hit_tokens"`
	Miss   uint64 `json:"cache_miss_tokens"`
	Output uint64 `json:"output_tokens"`
}

type roundRecord struct {
	Hash        string    `json:"hash"`
	CommittedAt time.Time `json:"committed_at"`
	Dormant     bool      `json:"dormant"`
}

func initializeMetadata(tx *bolt.Tx) error {
	for _, name := range [][]byte{projectBucket, roundsBucket} {
		if _, err := tx.CreateBucket(name); err != nil {
			return err
		}
	}
	if err := tx.Bucket(projectBucket).Put(userKey, numberKey(0)); err != nil {
		return err
	}
	for _, key := range roleKeys {
		if err := putRole(tx, key, roleRecord{}); err != nil {
			return err
		}
	}
	return nil
}

func validateBuckets(tx *bolt.Tx) error {
	count := 0
	if err := tx.ForEach(func(_ []byte, _ *bolt.Bucket) error { count++; return nil }); err != nil {
		return err
	}
	project := tx.Bucket(projectBucket)
	if count != 2 || project == nil || tx.Bucket(roundsBucket) == nil || project.Stats().KeyN != 4 {
		return protocol.IntegrityFailure("invalid state buckets")
	}
	return nil
}

func numberKey(n uint64) []byte { return binary.BigEndian.AppendUint64(nil, n) }

func readCounter(value []byte) (uint64, error) {
	if len(value) != 8 || binary.BigEndian.Uint64(value) > math.MaxInt64 {
		return 0, protocol.IntegrityFailure("invalid state counter")
	}
	return binary.BigEndian.Uint64(value), nil
}

// Stored records require every named field and reject nulls, unknown and duplicate
// keys. Decode each field directly into its destination, without decoding the
// whole record again or maintaining a separate required-name list.
func decodeRecord(body []byte, targets map[string]any) error {
	var fields map[string]json.RawMessage
	if protocol.UnmarshalJSON(body, &fields) != nil || len(fields) != len(targets) {
		return protocol.IntegrityFailure("invalid state record")
	}
	for name, target := range targets {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return protocol.IntegrityFailure("missing state field")
		}
		if protocol.UnmarshalJSON(value, target) != nil {
			return protocol.IntegrityFailure("invalid state field")
		}
	}
	return nil
}

func readRole(tx *bolt.Tx, key []byte) (roleRecord, error) {
	var r roleRecord
	err := decodeRecord(tx.Bucket(projectBucket).Get(key), map[string]any{
		"number": &r.Number, "cache_hit_tokens": &r.Hit, "cache_miss_tokens": &r.Miss, "output_tokens": &r.Output,
	})
	if err == nil && (r.Number > math.MaxInt64 || r.Hit > math.MaxInt64 || r.Miss > math.MaxInt64 || r.Output > math.MaxInt64) {
		err = protocol.IntegrityFailure("role counter overflow")
	}
	return r, err
}

func putRole(tx *bolt.Tx, key []byte, r roleRecord) error {
	body, _ := jsonv2.Marshal(r)
	return tx.Bucket(projectBucket).Put(key, body)
}

func readCounters(tx *bolt.Tx) (Counters, error) {
	number, err := readCounter(tx.Bucket(projectBucket).Get(userKey))
	if err != nil {
		return Counters{}, err
	}
	g := Counters{UserNumber: number}
	for i, target := range []struct {
		number *uint64
		usage  *protocol.TokenUsage
	}{{&g.PlannerNumber, &g.Planner}, {&g.WorkerNumber, &g.Worker}, {&g.ReviewerNumber, &g.Reviewer}} {
		r, err := readRole(tx, roleKeys[i])
		if err != nil {
			return g, err
		}
		*target.number = r.Number
		*target.usage = protocol.TokenUsage{Hit: r.Hit, Miss: r.Miss, Output: r.Output}
	}
	return g, nil
}

func readRound(key, body []byte) (protocol.StoredRound, error) {
	var r protocol.StoredRound
	number, err := readCounter(key)
	if err != nil {
		return r, err
	}
	var record roundRecord
	if err := decodeRecord(body, map[string]any{
		"hash": &record.Hash, "committed_at": &record.CommittedAt, "dormant": &record.Dormant,
	}); err != nil {
		return r, err
	}
	r = protocol.StoredRound{Number: number, Hash: record.Hash, CommittedAt: record.CommittedAt, Dormant: record.Dormant}
	if r.CommittedAt.IsZero() || !validHash(r.Hash) {
		return r, protocol.IntegrityFailure("invalid committed round metadata")
	}
	return r, nil
}

func putRound(tx *bolt.Tx, r protocol.StoredRound) error {
	body, err := jsonv2.Marshal(roundRecord{Hash: r.Hash, CommittedAt: r.CommittedAt, Dormant: r.Dormant})
	if err != nil {
		return err
	}
	return tx.Bucket(roundsBucket).Put(numberKey(r.Number), body)
}
