package protocol

import (
	"bytes"
	"embed"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"sync"
	"unicode/utf8"
)

//go:embed contracts/*.json
var contractFiles embed.FS

// Only embedded schemas are retained, keyed by their exact bytes. The immutable
// rules can be shared by parallel calls; arbitrary schemas are never cached.
var contractSchemas = sync.OnceValues(func() (map[string]map[string]any, error) {
	entries, err := contractFiles.ReadDir("contracts")
	if err != nil {
		return nil, err
	}
	rules := make(map[string]map[string]any, len(entries))
	for _, entry := range entries {
		body, err := contractFiles.ReadFile("contracts/" + entry.Name())
		if err != nil {
			return nil, err
		}
		rule, err := parseJSONSchema(body)
		if err != nil {
			return nil, fmt.Errorf("contract %s: %w", entry.Name(), err)
		}
		rules[string(body)] = rule
	}
	return rules, nil
})

// Contract returns one embedded contract by its file stem. The embedded
// directory is the source of truth; adding a contract needs no second registry.
func Contract(name string) (json.RawMessage, error) {
	body, err := contractFiles.ReadFile("contracts/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("unknown contract %q", name)
	}
	return body, nil
}

// ValidateMessages checks complete settled groups only. Tool IDs are
// local to an assistant group, never global execution or submission identities.
func ValidateMessages(messages []Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("empty native message chain")
	}
	pending := map[string]bool{}
	for _, message := range messages {
		if err := validateMessage(message); err != nil {
			return err
		}
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				return fmt.Errorf("orphan or duplicate tool receipt")
			}
			delete(pending, message.ToolCallID)
			continue
		}
		if len(pending) != 0 {
			return fmt.Errorf("unsettled assistant tool calls")
		}
		for _, call := range message.ToolCalls {
			if pending[call.ID] {
				return fmt.Errorf("duplicate tool call identity")
			}
			pending[call.ID] = true
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("unsettled assistant tool calls")
	}
	return nil
}

func validMessageStrings(message Message) bool {
	if !utf8.ValidString(message.Role) || !utf8.ValidString(message.Content) || !utf8.ValidString(message.ToolCallID) || (message.ReasoningContent != nil && !utf8.ValidString(*message.ReasoningContent)) {
		return false
	}
	for _, call := range message.ToolCalls {
		if !utf8.ValidString(call.ID) || !utf8.ValidString(call.Type) || !utf8.ValidString(call.Function.Name) || !utf8.ValidString(call.Function.Arguments) {
			return false
		}
	}
	return true
}

// validateMessage checks the typed envelope, leaving argument JSON to batch
// validation. No conversation text is serialized or parsed again here.
func validateMessage(message Message) error {
	if !validMessageStrings(message) {
		return fmt.Errorf("invalid UTF-8 in native message")
	}
	switch message.Role {
	case "system", "user":
		if message.ReasoningContent != nil || len(message.ToolCalls) != 0 || message.ToolCallID != "" {
			return fmt.Errorf("unexpected fields in %s message", message.Role)
		}
	case "assistant":
		if message.ToolCallID != "" {
			return fmt.Errorf("invalid assistant message fields")
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
				return fmt.Errorf("invalid native tool call")
			}
		}
	case "tool":
		if message.ToolCallID == "" || message.ReasoningContent != nil || len(message.ToolCalls) != 0 {
			return fmt.Errorf("invalid tool receipt fields")
		}
	default:
		return fmt.Errorf("unknown native message role")
	}
	return nil
}

func ValidateResponse(r Response) error {
	if err := validateMessage(r.Message); err != nil {
		return fmt.Errorf("%w: %v", ErrModelOutput, err)
	}
	if r.Message.Role != "assistant" || !r.Usage.Valid() || !r.TokenDetails.Valid(r.Usage) {
		return fmt.Errorf("%w: invalid native response", ErrModelOutput)
	}
	switch r.FinishReason {
	case "stop", "tool_calls", "length", "content_filter", "insufficient_system_resource", "aborted":
		return nil
	default:
		return fmt.Errorf("%w: invalid finish reason", ErrModelOutput)
	}
}

func ValidateRequest(r Request) error {
	switch r.Effort {
	case EffortHigh, EffortLow, EffortMax:
	default:
		return fmt.Errorf("invalid reasoning effort")
	}
	if r.Tools == nil {
		return fmt.Errorf("native tools must be an array")
	}
	seen := map[string]bool{}
	for _, tool := range r.Tools {
		if tool.Type != "function" || tool.Function.Name == "" || !utf8.ValidString(tool.Function.Name) || !utf8.ValidString(tool.Function.Description) {
			return fmt.Errorf("invalid native tool definition")
		}
		parameters := tool.Function.Parameters
		if !jsontext.Value(parameters).IsValid() || bytes.TrimSpace(parameters)[0] != '{' {
			return fmt.Errorf("tool parameters must be a JSON object")
		}
		if seen[tool.Function.Name] {
			return fmt.Errorf("duplicate native tool")
		}
		seen[tool.Function.Name] = true
	}
	return ValidateMessages(r.Messages)
}
