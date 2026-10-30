package protocol

import (
	"context"
	"time"
)

// DiagnosticIdentity contains no messages, tools or mutable execution state.
// Characters attach it only for enabled observations; debug consumes it.
type DiagnosticIdentity struct {
	InvocationID  string     `json:"invocation_id"`
	ToolCallID    string     `json:"tool_call_id,omitempty"` // Original model call; absent for model observations.
	WorkflowID    WorkflowID `json:"workflow_id"`
	Role          Role       `json:"role"`
	SessionID     string     `json:"session_id"`
	Purpose       string     `json:"purpose"`
	CycleKey      string     `json:"cycle_key"`
	PromptVersion string     `json:"prompt_version"`
	WorkerWrapUp  bool       `json:"worker_wrap_up,omitempty"`
}

type diagnosticIdentityKey struct{}

func WithDiagnosticIdentity(ctx context.Context, identity DiagnosticIdentity) context.Context {
	return context.WithValue(ctx, diagnosticIdentityKey{}, identity)
}

func DiagnosticIdentityFromContext(ctx context.Context) DiagnosticIdentity {
	identity, _ := ctx.Value(diagnosticIdentityKey{}).(DiagnosticIdentity)
	return identity
}

// ToolDiagnostic is execution metadata, never a model-visible tool receipt.
type ToolDiagnostic struct {
	CallID     string         `json:"call_id"`
	Tool       string         `json:"tool"`
	Started    time.Time      `json:"started_at"`
	Ended      time.Time      `json:"ended_at"`
	DurationMS float64        `json:"duration_ms"`
	Status     ToolCallStatus `json:"status"`
}

// RequestDiagnostic is collected locally by model for one HTTP attempt.
// It contains no role information; debug obtains identity from ctx.
type RequestDiagnostic struct {
	RequestBody    []byte     `json:"-"`                         // Borrowed frozen request; only persisted by debug.
	HTTPErrorBody  string     `json:"http_error_body,omitempty"` // Bounded provider body; credentials redacted by model.
	CallID         string     `json:"call_id"`
	Started        time.Time  `json:"started_at"`
	FirstByte      *time.Time `json:"first_byte_at,omitempty"`
	Ended          *time.Time `json:"ended_at,omitempty"`
	HTTPStatus     int        `json:"http_status,omitempty"`
	ResponseBytes  int64      `json:"response_bytes,omitempty"`
	StreamFailure  string     `json:"stream_failure_reason,omitempty"`
	TransportError error      `json:"-"`
}

// Observation is bound once for the active workspace. Calls may be concurrent.
// Inputs are borrowed for the synchronous call: implementations must not mutate
// or retain them, nor change normal execution results. Pointer parameters are nonnil.
type Observation interface {
	// BeforeModelRequest records the provider body and may pause before sending.
	// Only an explicitly configured pause may reject the request.
	BeforeModelRequest(ctx context.Context, callID string, requestBody []byte) error
	// RecordModelRequest records final statistics, response and error for one HTTP attempt.
	RecordModelRequest(ctx context.Context, diagnostic *RequestDiagnostic, response *Response, err error)
	// RecordToolExecution records statistics for a finished external tool, including failures.
	RecordToolExecution(ctx context.Context, diagnostic *ToolDiagnostic)
	// NotifyReplyRequired precedes display only when show_user requires an answer.
	NotifyReplyRequired()
	// DumpContext records a diagnostic snapshot, independently of durable safe points.
	DumpContext(ctx context.Context, event string, snapshot []byte)
}
