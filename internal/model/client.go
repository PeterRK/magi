// Package model implements the DeepSeek adapter and communication policy.
// Core owns role identity and settlement; provider configuration stays here.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"magi/internal/core/protocol"
)

const modelName = "deepseek-flash"
const endpoint = "https://api.deepseek.com/chat/completions"

var ErrMissingKey = errors.New("DEEPSEEK_API_KEY is required for model calls")

type client struct {
	key  string
	url  string
	http *http.Client
	// Zero selects the production default; tests inject a short local interval.
	idleTimeout time.Duration
	// Zero allows five transport retries; one permits no transport retries.
	attempts int
}

var _ protocol.Model = (*client)(nil)

// FromEnv never reads another application's configuration or persists a key.
func FromEnv() (protocol.Model, error) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return nil, ErrMissingKey
	}
	for _, c := range key {
		if c <= 32 || c >= 127 {
			return nil, errors.New("DEEPSEEK_API_KEY contains invalid characters")
		}
	}
	// HTTP/1 cancellation closes an interrupted stream's connection, while
	// complete responses can still reuse healthy connections.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return &client{key: key, url: endpoint, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

const (
	modelContextTokens = 1_000_000
	modelMaxTokens     = 100 * 1024
	modelLongMaxTokens = 200 * 1024
	modelEffort        = "high"
	outputLimitHint    = "Your previous response exceeded the output limit. Keep your reasoning and response concise, and focus on the essential result."
)

func (*client) InputCapacity() int { return modelContextTokens - modelLongMaxTokens - 4096/4 }

type wireRequest struct {
	Model    string             `json:"model"`
	Messages []protocol.Message `json:"messages"`
	Tools    []protocol.Tool    `json:"tools"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
	Effort        string `json:"reasoning_effort,omitempty"`
	MaxTokens     int    `json:"max_tokens"`
	Stream        bool   `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (c *client) Invoke(ctx context.Context, request protocol.Request) (result protocol.Response, err error) {
	if err := protocol.ValidateRequest(request); err != nil {
		return protocol.Response{}, errors.New("invalid frozen DeepSeek request")
	}
	body := wireRequest{Model: modelName, Messages: request.Messages, Tools: request.Tools,
		Effort: modelEffort, MaxTokens: modelMaxTokens, Stream: true}
	body.Thinking.Type = "enabled"
	switch request.Effort {
	case protocol.EffortLow:
		body.Effort = "low"
	case protocol.EffortMax:
		body.Effort = "max"
	}
	if request.LongOutput {
		body.MaxTokens = modelLongMaxTokens
	}
	body.StreamOptions.IncludeUsage = true
	encoded, err := json.Marshal(body)
	if err != nil {
		return protocol.Response{}, err
	}
	defer func() {
		if ctx.Err() != nil {
			result, err = protocol.Response{}, ctx.Err()
		}
	}()
	limit := c.attempts
	if limit <= 0 {
		limit = 6
	}
	hinted := false
	for attempt := 0; ; {
		if err := ctx.Err(); err != nil {
			return protocol.Response{}, err
		}
		// Each HTTP attempt owns its cancellable context and idle watch.
		response, callErr := c.invokeObserved(ctx, encoded)
		if callErr == nil {
			if response.FinishReason != "length" {
				return response, nil
			}
			// Discard truncated output and usage; keep retries local to this call.
			switch {
			case body.Effort == "low":
				if hinted {
					return protocol.Response{}, protocol.ErrModelOutput
				}
				body.Messages = append(append([]protocol.Message(nil), body.Messages...), protocol.Message{Role: "system", Content: outputLimitHint})
				hinted = true
			default:
				body.Effort = "low"
			}
			encoded, err = json.Marshal(body)
			if err != nil {
				return protocol.Response{}, err
			}
			continue
		}
		delay, retryable := retryDelay(callErr, uint64(attempt), uint64(limit-1))
		if ctx.Err() != nil || !retryable {
			return response, publicError(callErr)
		}
		if err := waitRetry(ctx, delay); err != nil {
			return protocol.Response{}, err
		}
		attempt++
	}
}
