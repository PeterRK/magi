package characters

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

// Batch returns the ordinary response validation already performed by callModel.
// Nested compaction does not replace this batch or its receipts. Calls are read-only.
func (record *Invocation) Batch() ([]protocol.ValidatedToolCall, error) {
	return record.batch, record.batchErr
}

// validateBatch follows the request's output mode, independent of role or purpose.
func validateBatch(tools []protocol.Tool, response protocol.Response) ([]protocol.ValidatedToolCall, error) {
	if len(tools) == 0 {
		if validateTextResponse(response) != nil {
			return nil, protocol.ToolCallBatchError{Message: "a complete nonempty text response without tools is required"}
		}
		return nil, nil
	}
	return protocol.ValidateToolResponse(tools, response)
}

func validateTextResponse(response protocol.Response) error {
	if err := protocol.ValidateResponse(response); err != nil {
		return err
	}
	if response.FinishReason != "stop" || len(response.Message.ToolCalls) != 0 || strings.TrimSpace(response.Message.Content) == "" {
		return fmt.Errorf("%w: summary requires a complete nonempty text response without tools", protocol.ErrModelOutput)
	}
	return nil
}

// SaveReceipt validates and retains a model-visible receipt, even after cancellation.
func (record *Invocation) SaveReceipt(call protocol.ValidatedToolCall, result []byte, callErr error) error {
	receipt := toolReceipt{CallID: call.Call.ID}
	if callErr != nil {
		refusal, ok := protocol.SemanticToolCallError(callErr).(protocol.ToolCallError)
		if !ok {
			return callErr
		}
		result = refusal.Receipt()
	}
	receipt.Body = string(result)
	return record.saveReceipt(call.Index, receipt)
}

// ModelTools builds fixed tool definitions; each role chooses its own allowlist.
func ModelTools(names ...string) ([]protocol.Tool, error) {
	tools := make([]protocol.Tool, 0, len(names))
	for _, name := range names {
		schema, err := protocol.Contract(name)
		if err != nil {
			return nil, err
		}
		tools = append(tools, protocol.Tool{Type: "function", Function: protocol.Function{Name: name, Description: prompt.ToolDescription(name), Parameters: schema}})
	}
	return tools, nil
}

type toolReceipt struct {
	CallID string
	Body   string
}

// saveReceipt retains known outcomes in original call order, including after cancellation.
func (record *Invocation) saveReceipt(index int, receipt toolReceipt) error {
	if index < 0 || index >= len(record.message.ToolCalls) {
		return protocol.IntegrityFailure("receipt differs from its role invocation")
	}
	call := record.message.ToolCalls[index]
	if !utf8.ValidString(receipt.Body) {
		return protocol.IntegrityFailure("invalid role receipt")
	}
	if call.ID != receipt.CallID {
		return protocol.IntegrityFailure("receipt differs from its model call")
	}
	if record.receipts == nil {
		record.receipts = make([]toolReceipt, len(record.message.ToolCalls))
	}
	if record.receipts[index].CallID != "" {
		return protocol.IntegrityFailure("role receipt recorded twice")
	}
	record.receipts[index] = receipt
	return nil
}

func (record *Invocation) discardOutput() {
	record.message = nil
	record.batch, record.batchErr = nil, nil
	record.receipts = nil
	record.appended = 0
}

// pendingMessages contains complete, known outcomes not yet appended to Context.
// Pending output survives Worker execution expiry for delivery during wrap-up.
func (record *Invocation) pendingMessages() ([]protocol.Message, error) {
	if record.message == nil {
		return nil, nil
	}
	if len(record.receipts) != len(record.message.ToolCalls) {
		return nil, protocol.Fault{Code: protocol.CodeOutcomeUnknown, Message: "role response lacks complete tool receipts"}
	}
	group := []protocol.Message{*record.message}
	for _, receipt := range record.receipts {
		if receipt.CallID == "" {
			return nil, protocol.Fault{Code: protocol.CodeOutcomeUnknown, Message: "role response lacks complete tool receipts"}
		}
		group = append(group, protocol.Message{Role: "tool", ToolCallID: receipt.CallID, Content: receipt.Body})
	}
	return group[record.appended:], nil
}

// AppendPending resumes after partial delivery and releases output once Context
// owns the whole group. Full during Append must preserve this pending output.
func (record *Invocation) AppendPending(ctx context.Context, window *corecontext.Context) error {
	messages, err := record.pendingMessages()
	if err != nil {
		return err
	}
	for _, message := range messages {
		if err := window.Append(ctx, message); err != nil {
			return err
		}
		record.appended++
	}
	record.discardOutput()
	return nil
}

// RejectBatch records identifiable calls as rejected; malformed output is
// discarded and receives a protocol reminder. The role decides whether to retry.
func (record *Invocation) RejectBatch(ctx context.Context, window *corecontext.Context, batchErr error) error {
	var rejected protocol.ToolCallBatchError
	if !errors.As(batchErr, &rejected) || !rejected.Locatable {
		record.discardOutput()
		return window.Append(ctx, protocol.Message{Role: "system", Content: prompt.ProtocolReminder(batchErr.Error())})
	}
	body := string(rejected.Receipt())
	for index, call := range record.message.ToolCalls {
		if err := record.saveReceipt(index, toolReceipt{CallID: call.ID, Body: body}); err != nil {
			return err
		}
	}
	return record.AppendPending(ctx, window)
}

// ToolOutput admits settled outcomes or preserves a pre-execution refusal.
func ToolOutput(result protocol.ToolResult, err error) ([]byte, error) {
	switch result.Status {
	case protocol.ToolSucceeded, protocol.ToolFailed, protocol.ToolTimedOut, protocol.ToolInterrupted:
		// Known outcomes survive provider errors and late cancellation.
		// Receipt storage checks the body before admitting it to the conversation.
		return result.Body, nil
	case "":
		if err != nil {
			return nil, err
		}
		return nil, protocol.IntegrityFailure("tool execution returned no outcome")
	default:
		return nil, protocol.IntegrityFailure("tool execution returned an invalid outcome")
	}
}
