package model

import (
	"context"
	"crypto/rand"
	"time"

	"magi/internal/core/protocol"
)

// Only request boundaries notify debug. Transport statistics belong to this
// stack frame; concurrent attempts require no shared tracking state.
func (c *client) invokeObserved(ctx context.Context, wire []byte) (result protocol.Response, err error) {
	observer := protocol.Observer
	if observer == nil {
		return c.invokeHTTP(ctx, wire, nil)
	}
	diagnostic := protocol.RequestDiagnostic{CallID: rand.Text(), Started: time.Now().UTC(), RequestBody: wire}
	defer func() {
		if ctx.Err() != nil {
			result, err = protocol.Response{}, ctx.Err()
		}
		now := time.Now().UTC()
		diagnostic.Ended = &now
		observer.RecordModelRequest(ctx, &diagnostic, &result, err)
	}()
	if err := observer.BeforeModelRequest(ctx, diagnostic.CallID, wire); err != nil {
		return protocol.Response{}, err
	}
	// Exclude debug setup and an explicit pause from network execution time.
	diagnostic.Started = time.Now().UTC()
	return c.invokeHTTP(ctx, wire, &diagnostic)
}
