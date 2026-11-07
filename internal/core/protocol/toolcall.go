package protocol

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
)

type ValidatedToolCall struct {
	Call               ToolCall
	Index              int
	CanonicalArguments []byte
}

// ToolCallBatchError records whether rejected calls can receive paired receipts.
type ToolCallBatchError struct {
	Locatable bool
	Message   string
	Hint      string
}

func (e ToolCallBatchError) Error() string { return e.Message }

func (e ToolCallBatchError) FeedbackMessage() string {
	if e.Hint != "" {
		return e.Message + ": " + e.Hint
	}
	return e.Message
}

// Receipt formats the shared refusal body; the caller owns call pairing.
func (e ToolCallBatchError) Receipt() []byte {
	return (ToolCallError{Code: CodeInvalidCommand, Detail: "ARGUMENT_INVALID", Message: e.FeedbackMessage()}).Receipt()
}

func isControlTool(name string) bool {
	switch name {
	case "submit_plan", "submit_review", "finish_job", "show_user":
		return true
	}
	return false
}

// ValidateToolResponse validates ordinary role execution.
func ValidateToolResponse(tools []Tool, response Response) ([]ValidatedToolCall, error) {
	if err := ValidateResponse(response); err != nil {
		return nil, ToolCallBatchError{Message: "malformed assistant response"}
	}
	return validateToolCalls(tools, response.Message.ToolCalls, response.FinishReason)
}

// validateToolCalls validates selection, finish reason and arguments without
// executing a tool. The enclosing model response must already be valid.
func validateToolCalls(tools []Tool, calls []ToolCall, finishReason string) ([]ValidatedToolCall, error) {
	if finishReason == "length" || finishReason == "content_filter" ||
		finishReason == "insufficient_system_resource" || finishReason == "aborted" {
		return nil, ToolCallBatchError{Message: "incomplete assistant generation"}
	}
	if len(calls) == 0 {
		return nil, ToolCallBatchError{Message: "a native tool call is required; assistant text does not submit an action"}
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if seen[call.ID] {
			return nil, ToolCallBatchError{Message: "duplicate tool call ID"}
		}
		seen[call.ID] = true
	}
	reject := func(message string) ([]ValidatedToolCall, error) {
		return nil, ToolCallBatchError{Locatable: true, Message: message}
	}
	if finishReason != "tool_calls" {
		return reject("tool calls require tool_calls finish reason")
	}
	for _, call := range calls {
		if isControlTool(call.Function.Name) && len(calls) != 1 {
			return reject("control call must be the only call in the batch")
		}
	}
	allowed := map[string]Tool{}
	for _, tool := range tools {
		allowed[tool.Function.Name] = tool
	}
	validated := make([]ValidatedToolCall, 0, len(calls))
	for index, call := range calls {
		tool, ok := allowed[call.Function.Name]
		if !ok {
			hint := "no tools are available in this request"
			if len(tools) > 0 {
				names := make([]string, len(tools))
				for i, tool := range tools {
					names[i] = tool.Function.Name
				}
				hint = "allowed tools: " + strings.Join(names, ", ")
			}
			return nil, ToolCallBatchError{Locatable: true, Message: "tool is not allowed in this request", Hint: hint}
		}
		value, err := decodeValidatedJSON(tool.Function.Parameters, []byte(call.Function.Arguments))
		if err != nil {
			var violation JSONSchemaViolation
			if errors.As(err, &violation) && len(violation.Error()) <= 512 {
				return nil, ToolCallBatchError{Locatable: true, Message: "tool arguments do not satisfy the frozen schema", Hint: violation.Error()}
			}
			return reject("tool arguments do not satisfy the frozen schema")
		}
		canonical, err := jsonv2.Marshal(normalizeJSON(value), jsonv2.Deterministic(true))
		if err != nil {
			return reject("invalid tool arguments")
		}
		validated = append(validated, ValidatedToolCall{Call: call, Index: index, CanonicalArguments: canonical})
	}
	return validated, nil
}

// ToolCallError is a model-repairable refusal; other failures retain their own types.
type ToolCallError struct {
	Code    ErrorCode
	Detail  string
	Message string
	Field   string
}

func (e ToolCallError) Error() string { return string(e.Code) + ": " + e.Message }

// Receipt exposes only the specific correction; execution policy stays internal.
func (e ToolCallError) Receipt() []byte {
	code := e.Detail
	if code == "" {
		code = string(e.Code)
	}
	body, _ := jsonv2.Marshal(struct {
		Error   string `json:"error"`
		Field   string `json:"field,omitempty"`
		Message string `json:"message"`
	}{strings.ToLower(code), e.Field, e.Message})
	return body
}

// SemanticToolCallError converts explicitly reviewed candidate feedback into
// a model-visible tool error. Joined failures retain their original identity:
// selecting a repairable branch must never hide an independent failure.
func SemanticToolCallError(err error) error {
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if _, joined := cause.(interface{ Unwrap() []error }); joined {
			return err
		}
	}
	var already ToolCallError
	if errors.As(err, &already) {
		return already
	}
	var feedback CandidateFeedback
	var fault Fault
	if !errors.As(err, &feedback) || feedback.PublicMessage == "" || !errors.As(feedback.Err, &fault) {
		return err
	}
	var detail string
	switch fault.Code {
	case CodeInvalidCommand:
		detail = "ARGUMENT_INVALID"
	case CodePermissionDenied:
		detail = "ACCESS_DENIED"
	default:
		return err
	}
	return ToolCallError{Code: fault.Code, Detail: detail, Message: feedback.PublicMessage}
}
