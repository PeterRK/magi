// Package mock provides an in-memory storage backend for core tests.
// It depends only on protocol and does not simulate roles or engine state.
package mock

import (
	"context"
	"fmt"
	"sync"

	"magi/internal/core/protocol"
)

// Storage simulates allocation, history and usage without persistence or format
// validation. Other operations must be supplied through the embedded backend.
type Storage struct {
	protocol.StorageBackend
	mu        sync.Mutex
	numbers   map[string]uint64
	fragments map[string]string
	usage     map[string]protocol.TokenUsage
}

func (s *Storage) Allocate(source string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.numbers == nil {
		s.numbers = map[string]uint64{}
	}
	s.numbers[source]++
	return fmt.Sprintf("%c%d", source[0], s.numbers[source]), nil
}

func (*Storage) ReserveNumbers(ctx context.Context) error { return ctx.Err() }

func (s *Storage) WriteFragment(ctx context.Context, ref string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fragments == nil {
		s.fragments = map[string]string{}
	}
	s.fragments[ref] = string(body)
	return nil
}

func (s *Storage) ReadFragment(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	body, found := s.fragments[ref]
	if !found {
		return nil, protocol.InvalidCommand("numbered history reference does not exist")
	}
	return []byte(body), nil
}

func (s *Storage) ObserveUsage(role string, u protocol.TokenUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage == nil {
		s.usage = map[string]protocol.TokenUsage{}
	}
	v := s.usage[role]
	s.usage[role] = protocol.TokenUsage{Hit: v.Hit + u.Hit, Miss: v.Miss + u.Miss, Output: v.Output + u.Output}
	return nil
}

// Usage returns the observations received through the support interface.
func (s *Storage) Usage() map[string]protocol.TokenUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]protocol.TokenUsage, len(s.usage))
	for role, usage := range s.usage {
		result[role] = usage
	}
	return result
}
