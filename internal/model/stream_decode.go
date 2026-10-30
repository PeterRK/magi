package model

import (
	"encoding/json"
	"errors"
	"strings"

	"magi/internal/core/protocol"
)

type streamChunk struct {
	Choices []struct {
		Index *int `json:"index"`
		Delta struct {
			Role      string  `json:"role"`
			Content   string  `json:"content"`
			Reasoning *string `json:"reasoning_content"`
			Calls     []struct {
				Index    *int                  `json:"index"`
				ID       string                `json:"id"`
				Type     string                `json:"type"`
				Function protocol.FunctionCall `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Finish *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Input             *int `json:"prompt_tokens"`
		Output            *int `json:"completion_tokens"`
		Total             *int `json:"total_tokens"`
		CacheHit          *int `json:"prompt_cache_hit_tokens"`
		CacheMiss         *int `json:"prompt_cache_miss_tokens"`
		CompletionDetails *struct {
			Reasoning *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type streamDecoder struct {
	result             protocol.Response
	content, reasoning strings.Builder
	calls              []protocol.ToolCall
	// Builders must never be copied after use, including when a slice grows.
	arguments           []*strings.Builder
	finished, usageSeen bool
	reasoningSeen       bool
	providerFailure     modelFailure
}

func (s *streamDecoder) consume(data string) string {
	var chunk streamChunk
	if protocol.UnmarshalJSON([]byte(data), &chunk) != nil {
		return streamInvalidChunk
	}
	if len(chunk.Error) != 0 && string(chunk.Error) != "null" {
		s.providerFailure = inBandFailure(chunk.Error)
		return streamProviderError
	}
	if s.finished {
		return streamAfterFinish
	}
	if len(chunk.Choices) != 1 {
		return streamChoiceCount
	}
	choice := chunk.Choices[0]
	if choice.Index == nil || *choice.Index != 0 {
		return streamChoiceIndex
	}
	if choice.Delta.Role != "" && choice.Delta.Role != "assistant" {
		return streamChoiceRole
	}
	s.content.WriteString(choice.Delta.Content)
	if choice.Delta.Reasoning != nil {
		s.reasoningSeen = true
		s.reasoning.WriteString(*choice.Delta.Reasoning)
	}
	for _, delta := range choice.Delta.Calls {
		if delta.Index == nil || *delta.Index < 0 {
			return streamToolIndexRange
		}
		i := *delta.Index
		if i > len(s.calls) {
			return streamToolIndexGap
		}
		if i == len(s.calls) {
			s.calls = append(s.calls, protocol.ToolCall{})
			s.arguments = append(s.arguments, new(strings.Builder))
		}
		call := &s.calls[i]
		// Identity fields are sent once; only arguments are text deltas.
		if reason := mergeIdentity(&call.ID, delta.ID, streamRepeatedID, streamConflictingID); reason != "" {
			return reason
		}
		if reason := mergeIdentity(&call.Type, delta.Type, streamRepeatedType, streamConflictingType); reason != "" {
			return reason
		}
		if reason := mergeIdentity(&call.Function.Name, delta.Function.Name, streamRepeatedName, streamConflictingName); reason != "" {
			return reason
		}
		s.arguments[i].WriteString(delta.Function.Arguments)
	}
	if chunk.Usage != nil {
		if choice.Finish == nil {
			return streamUsageNoFinish
		}
		u := chunk.Usage
		if u.Input == nil || u.Output == nil || u.Total == nil {
			return streamUsageMissing
		}
		s.result.Usage = protocol.Usage{InputTokens: *u.Input, OutputTokens: *u.Output}
		if !s.result.Usage.Valid() || s.result.Usage.Total() != *u.Total {
			return streamUsageInvalid
		}
		details := &protocol.TokenDetails{CacheHitTokens: u.CacheHit, CacheMissTokens: u.CacheMiss}
		if u.CompletionDetails != nil {
			details.ReasoningTokens = u.CompletionDetails.Reasoning
		}
		if details.CacheHitTokens != nil || details.CacheMissTokens != nil || details.ReasoningTokens != nil {
			s.result.TokenDetails = details
		}
		s.usageSeen = true
	}
	if choice.Finish != nil {
		s.result.FinishReason = *choice.Finish
		s.finished = true
	}
	return ""
}

func (s *streamDecoder) complete() string {
	if !s.finished {
		return streamDoneNoFinish
	}
	if s.result.FinishReason == "insufficient_system_resource" || s.result.FinishReason == "aborted" {
		return streamServerFinish
	}
	if !s.usageSeen {
		return streamDoneNoUsage
	}
	s.result.Message.Content = s.content.String()
	if s.reasoningSeen {
		thought := s.reasoning.String()
		s.result.Message.ReasoningContent = &thought
	}
	for i := range s.calls {
		s.calls[i].Function.Arguments = s.arguments[i].String()
	}
	s.result.Message.ToolCalls = s.calls
	if protocol.ValidateResponse(s.result) != nil {
		return streamEnvelope
	}
	if s.result.FinishReason == "stop" && strings.TrimSpace(s.result.Message.Content) == "" && len(s.calls) == 0 {
		return streamEmptyResponse
	}
	return ""
}

func (s *streamDecoder) failure(reason string) (protocol.Response, error) {
	// Failed requests discard both generated output and usage.
	if reason == streamProviderError {
		return protocol.Response{}, s.providerFailure
	}
	transport := reason == streamEOF || reason == streamReadError || reason == streamIdle
	recoverableFinish := !s.finished || s.result.FinishReason == "stop" || s.result.FinishReason == "tool_calls" || s.result.FinishReason == "insufficient_system_resource" || s.result.FinishReason == "aborted"
	retryable := (transport && recoverableFinish) || reason == streamEmptyResponse || reason == streamServerFinish
	failure := error(protocol.ErrModelOutput)
	if retryable {
		failure = protocol.ErrModelUnavailable
	}
	if reason == streamIdle {
		failure = errors.Join(failure, errStreamIdleTimeout)
	}
	return protocol.Response{}, modelFailure{Err: failure, Retryable: retryable}
}

func mergeIdentity(target *string, delta, repeated, conflicting string) string {
	if delta == "" {
		return ""
	}
	if *target != "" {
		if *target != delta {
			return conflicting
		}
		return repeated
	}
	*target = delta
	return ""
}
