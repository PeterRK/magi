package protocol

import (
	"context"
	"time"
)

// StorageFormatVersion changes when the persisted workspace format changes.
const StorageFormatVersion = 61

type TokenUsage struct {
	Hit    uint64 `json:"cache_hit_tokens"`
	Miss   uint64 `json:"cache_miss_tokens"`
	Output uint64 `json:"output_tokens"`
}

type StoredRound struct {
	Number      uint64    `json:"number"`
	Hash        string    `json:"hash"`
	CommittedAt time.Time `json:"committed_at"`
	Dormant     bool      `json:"dormant"`
}

// StorageBackend exposes durable operations, never database handles or open transactions.
// Publish writes the snapshot before checking and advancing its index atomically.
type StorageBackend interface {
	Allocate(string) (string, error)
	ReadFragment(context.Context, string) ([]byte, error)
	ReserveNumbers(context.Context) error
	WriteFragment(context.Context, string, []byte) error
	ObserveUsage(string, TokenUsage) error
	FlushUsage(context.Context) error
	Latest(context.Context) (StoredRound, bool, error)
	Retained(context.Context) ([]StoredRound, error)
	Publish(context.Context, StoredRound, []byte, *StoredRound) (StoredRound, error)
	Read(context.Context, string) ([]byte, error)
	Prune(context.Context) error
}
