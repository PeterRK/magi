package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"magi/internal/core/protocol"
)

// modelFailure contains supplier/transport facts used only by this implementation.
// Core receives sanitized errors and relies on the protocol error categories.
type modelFailure struct {
	Err        error
	Retryable  bool
	RetryAfter time.Duration
	HTTPStatus int
}

func (e modelFailure) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("%s: DeepSeek HTTP %d", e.Err, e.HTTPStatus)
	}
	return e.Err.Error()
}
func (e modelFailure) Unwrap() error { return e.Err }

type providerError struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func httpFailure(resp *http.Response, diagnostic *protocol.RequestDiagnostic) error {
	// Provider text is available only to local debug, never to public errors.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if diagnostic != nil {
		diagnostic.HTTPErrorBody = string(body)
	}
	var payload struct {
		Error providerError `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	if (resp.StatusCode == 400 || resp.StatusCode == 413 || resp.StatusCode == 422) && payload.Error.contextOverflow() {
		return modelFailure{Err: protocol.ErrContextOverflow}
	}
	return modelFailure{
		Err:        protocol.ErrModelUnavailable,
		HTTPStatus: resp.StatusCode,
		Retryable:  resp.StatusCode == 429 || resp.StatusCode >= 500,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}
}

// DeepSeek also reports overflow as invalid_request_error with an explicit
// maximum-context message. Consume that text only for classification.
func (e providerError) contextOverflow() bool {
	return e.Code == "context_length_exceeded" || e.Type == "context_length_exceeded" ||
		((e.Code == "invalid_request_error" || e.Type == "invalid_request_error") &&
			strings.Contains(strings.ToLower(e.Message), "maximum context length"))
}

// Unknown structured in-band errors default to transient server failures.
// Only classification fields are consumed; provider messages never escape.
func inBandFailure(raw json.RawMessage) modelFailure {
	var detail providerError
	failure := modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &detail) != nil {
		return modelFailure{Err: protocol.ErrModelOutput}
	}
	if detail.contextOverflow() {
		return modelFailure{Err: protocol.ErrContextOverflow}
	}
	for _, label := range []string{detail.Code, detail.Type} {
		switch label {
		case "authentication_error", "permission_error", "invalid_api_key", "insufficient_quota", "quota_exceeded", "insufficient_balance", "invalid_request_error", "invalid_request":
			failure.Retryable = false
		}
	}
	return failure
}

func publicError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if err == context.DeadlineExceeded {
			return err
		}
		// Preserve a non-pure deadline classification without exposing adapter
		// diagnostics. A joined probe/audit failure must not become resumable.
		return errors.Join(context.DeadlineExceeded, protocol.ErrModelUnavailable)
	}
	// Adapter diagnostics may contain provider bodies, prompts or credentials.
	var failure modelFailure
	if errors.As(err, &failure) {
		if errors.Is(failure.Err, protocol.ErrContextOverflow) {
			return protocol.ErrContextOverflow
		}
		if errors.Is(failure.Err, protocol.ErrModelOutput) {
			return protocol.ErrModelOutput
		}
		if failure.HTTPStatus != 0 {
			return fmt.Errorf("%w: DeepSeek HTTP %d", protocol.ErrModelUnavailable, failure.HTTPStatus)
		}
	}
	return protocol.ErrModelUnavailable
}
