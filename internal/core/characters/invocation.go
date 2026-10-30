// Package characters provides shared role execution and handoff contracts.
package characters

import (
	"context"
	"strconv"
	"sync/atomic"

	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
)

var identities atomic.Uint64

// NextID allocates a process-local identity shared by all roles and rounds.
// The sequence is neither reset during the process nor persisted across restarts.
func NextID() string {
	return strconv.FormatUint(identities.Add(1), 10)
}

// InvocationRequest binds model input to role execution metadata inside characters.
type InvocationRequest struct {
	protocol.Request
	WorkflowID    protocol.WorkflowID `json:"workflow_id"`
	Role          protocol.Role       `json:"role"`
	SessionID     string              `json:"session_id"`
	Purpose       string              `json:"purpose"`
	CycleKey      string              `json:"cycle_key"`
	PromptVersion string              `json:"prompt_version"`
	WorkerWrapUp  bool                `json:"worker_wrap_up,omitempty"`
}

type invocationKey struct{}

// Invocation belongs to one sequential role execution. Compaction shares
// its error state, while ordinary responses and receipts stay available to the role.
// Request metadata is private; roles change execution mode through EnterWrapUp.
type Invocation struct {
	owner        InvocationRequest
	invocationID string
	message      *protocol.Message
	batch        []protocol.ValidatedToolCall
	batchErr     error
	errorStreak  uint64
	receipts     []toolReceipt
	appended     int
}

func CurrentInvocation(ctx context.Context) *Invocation {
	return ctx.Value(invocationKey{}).(*Invocation)
}

// ContextModel binds response validation, settlement and role usage to provider calls.
// The process binding owns this wrapper; role Contexts retain only a tier.
// Full receives validated text without replacing ordinary pending output.
func ContextModel(provider protocol.Model) protocol.Model { return contextModel{provider} }

type contextModel struct{ protocol.Model }

func (m contextModel) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	return CurrentInvocation(ctx).callModel(ctx, input, m.Model.Invoke)
}

// Context attaches this sequential execution to the current parent context.
// Interaction resumes with a new parent without resetting error state or receipts.
func (record *Invocation) Context(ctx context.Context) context.Context {
	if protocol.Observer != nil {
		ctx = protocol.WithDiagnosticIdentity(ctx, diagnosticIdentity(record.owner, record.invocationID))
	}
	return context.WithValue(ctx, invocationKey{}, record)
}

// ToolContext snapshots the submitting call's identity before tool execution.
// Ordinary execution carries no extra diagnostic value when observation is off.
func ToolContext(ctx context.Context, toolCallID string) context.Context {
	if protocol.Observer == nil {
		return ctx
	}
	record := CurrentInvocation(ctx)
	identity := diagnosticIdentity(record.owner, record.invocationID)
	identity.ToolCallID = toolCallID
	return protocol.WithDiagnosticIdentity(ctx, identity)
}

func diagnosticIdentity(owner InvocationRequest, invocationID string) protocol.DiagnosticIdentity {
	return protocol.DiagnosticIdentity{
		InvocationID: invocationID, WorkflowID: owner.WorkflowID, Role: owner.Role,
		SessionID: owner.SessionID, Purpose: owner.Purpose, CycleKey: owner.CycleKey,
		PromptVersion: owner.PromptVersion, WorkerWrapUp: owner.WorkerWrapUp,
	}
}

func (record *Invocation) callModel(callCtx context.Context, input protocol.Request, invoke func(context.Context, protocol.Request) (protocol.Response, error)) (protocol.Response, error) {
	compact := corecontext.IsCompacting(callCtx)
	if compact && record.owner.WorkerWrapUp {
		return protocol.Response{}, protocol.Fault{Code: protocol.CodeCapacityLimited, Message: "Worker wrap-up cannot start context compaction"}
	}
	if record.errorStreak >= maxConsecutiveErrors {
		return protocol.Response{}, protocol.Fault{Code: protocol.CodeCapacityLimited, Message: "consecutive request error limit exhausted"}
	}
	if err := callCtx.Err(); err != nil {
		return protocol.Response{}, err
	}
	invocationID := NextID()
	// Context already detached this request. Model implementations treat it as read-only.
	if protocol.Observer != nil {
		callCtx = protocol.WithDiagnosticIdentity(callCtx, diagnosticIdentity(record.owner, invocationID))
	}
	response, err := invoke(callCtx, input)
	if err == nil {
		err = callCtx.Err()
	}
	if err != nil {
		return response, err
	}
	var calls []protocol.ValidatedToolCall
	var batchErr error
	if compact {
		batchErr = validateTextResponse(response)
	} else {
		calls, batchErr = validateBatch(input.Tools, response)
	}
	if err := observeUsage(record.owner.Role, response.Usage, response.TokenDetails); err != nil {
		return response, err
	}
	if batchErr == nil {
		record.errorStreak = 0
	} else {
		record.errorStreak++
	}
	if compact {
		// Context owns the checkpoint; preserve the ordinary response and receipts.
		return response, batchErr
	}
	record.batch, record.batchErr = calls, batchErr
	// Only the ordinary call identity is needed after transport completes.
	record.invocationID = invocationID
	message := protocol.CloneMessage(response.Message)
	record.message = &message
	record.receipts = nil
	record.appended = 0
	return response, nil
}

func NewInvocation(owner InvocationRequest) *Invocation {
	owner.Messages = nil
	owner.Request = protocol.CloneRequest(owner.Request)
	return &Invocation{owner: owner}
}

func InvocationContext(ctx context.Context, owner InvocationRequest) context.Context {
	return NewInvocation(owner).Context(ctx)
}

// Owner returns detached configuration for starting a related invocation.
func (record *Invocation) Owner() InvocationRequest {
	request := record.owner
	request.Request = protocol.CloneRequest(request.Request)
	return request
}

// EnterWrapUp changes Worker execution policy without replacing pending results.
func (record *Invocation) EnterWrapUp(tools []protocol.Tool) {
	record.owner.WorkerWrapUp = true
	record.owner.Tools = protocol.CloneRequest(protocol.Request{Tools: tools}).Tools
}

func observeUsage(role protocol.Role, usage protocol.Usage, details *protocol.TokenDetails) error {
	if !usage.Valid() || !details.Valid(usage) {
		return protocol.IntegrityFailure("invalid model usage metadata")
	}
	count := protocol.TokenUsage{Miss: uint64(usage.InputTokens), Output: uint64(usage.OutputTokens)}
	if details != nil {
		if details.CacheHitTokens != nil {
			count.Hit = uint64(*details.CacheHitTokens)
			count.Miss = uint64(usage.InputTokens) - count.Hit
		}
		if details.CacheMissTokens != nil {
			count.Miss = uint64(*details.CacheMissTokens)
			count.Hit = uint64(usage.InputTokens) - count.Miss
		}
	}
	return protocol.Storage.ObserveUsage(string(role), count)
}
