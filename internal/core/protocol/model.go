package protocol

import (
	"context"
	"errors"
	"slices"
)

// Message carries model-visible content; assistant reasoning may be empty.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitzero"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// Effort selects reasoning intensity. Its zero value is High.
type Effort uint8

const (
	EffortHigh Effort = iota
	EffortLow
	EffortMax
)

// Request contains model input. Invocation identity is carried separately.
type Request struct {
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools"`
	LongOutput bool      `json:"-"` // Output tier; the provider owns the concrete budget.
	Effort     Effort    `json:"-"` // Reasoning tier, independent of the output budget.
}

// TokenDetails are optional provider counters, already included in Usage.
type TokenDetails struct {
	CacheHitTokens  *int `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens *int `json:"cache_miss_tokens,omitempty"`
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

type Response struct {
	TokenDetails *TokenDetails `json:"token_details,omitempty"`
	Message      Message       `json:"message"`
	Usage        Usage         `json:"usage"`
	FinishReason string        `json:"finish_reason"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Model is a configured provider instance. Configuration and communication
// retries belong to the implementation; core only needs its input capacity.
// Invoke must not mutate the request or reenter its owning Context.
type Model interface {
	Invoke(context.Context, Request) (Response, error)
	// InputCapacity is the usable input token budget, excluding provider reserves.
	InputCapacity() int
}

// Valid prevents provider metadata from decreasing or overflowing budgets.
func (u Usage) Valid() bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && u.InputTokens <= int(^uint(0)>>1)-u.OutputTokens
}

func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

var ErrModelOutput = errors.New("invalid model output")

func (d *TokenDetails) Valid(usage Usage) bool {
	if d == nil {
		return true
	}
	for _, n := range []*int{d.CacheHitTokens, d.CacheMissTokens} {
		if n != nil && (*n < 0 || *n > usage.InputTokens) {
			return false
		}
	}
	if d.CacheHitTokens != nil && d.CacheMissTokens != nil && *d.CacheHitTokens != usage.InputTokens-*d.CacheMissTokens {
		return false
	}
	return d.ReasoningTokens == nil || (*d.ReasoningTokens >= 0 && *d.ReasoningTokens <= usage.OutputTokens)
}

// CloneRequest detaches mutable containers while sharing immutable text. It is an
// in-process boundary copy, not a serialized archive or another context frame.
func CloneRequest(r Request) Request {
	r.Messages = CloneMessages(r.Messages)
	r.Tools = slices.Clone(r.Tools)
	for i := range r.Tools {
		r.Tools[i].Function.Parameters = slices.Clone(r.Tools[i].Function.Parameters)
	}
	return r
}

// CloneMessage detaches mutable containers while sharing immutable text.
func CloneMessage(message Message) Message {
	message.ToolCalls = slices.Clone(message.ToolCalls)
	if message.ReasoningContent != nil {
		reasoning := *message.ReasoningContent
		message.ReasoningContent = &reasoning
	}
	return message
}

func CloneMessages(messages []Message) []Message {
	result := slices.Clone(messages)
	for i := range result {
		result[i] = CloneMessage(result[i])
	}
	return result
}
