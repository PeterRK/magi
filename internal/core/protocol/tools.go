package protocol

import (
	"context"
	"encoding/json"
)

// These types carry tool definitions and calls across model and role boundaries.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool describes a model-visible function, without execution authority.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolRequest identifies one invocation independently of its calling role.
type ToolRequest struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ToolCallStatus string

const (
	ToolSucceeded   ToolCallStatus = "SUCCEEDED"
	ToolFailed      ToolCallStatus = "FAILED"
	ToolTimedOut    ToolCallStatus = "TIMED_OUT"
	ToolInterrupted ToolCallStatus = "INTERRUPTED"
)

// ToolResult transports execution state and the tool's own model-visible text.
// Roles forward Body directly; this transport is never a context envelope.
type ToolResult struct {
	Status ToolCallStatus
	Body   []byte
}

type ToolExecutor interface {
	// PrepareRound resets temporary output before any role begins a new round.
	PrepareRound(context.Context) error
	// Execute receives schema-validated, canonical arguments and must not mutate
	// them. Providers decode arguments and enforce execution constraints.
	// An empty result with an error means execution
	// was rejected before effects; a populated result records
	// the observed outcome, including provider errors. Roles own scheduling
	// and result handling.
	Execute(context.Context, ToolRequest) (ToolResult, error)
}
