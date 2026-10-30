package model

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"magi/internal/core/protocol"
)

func (c *client) invokeHTTP(ctx context.Context, encoded []byte, diagnostic *protocol.RequestDiagnostic) (protocol.Response, error) {
	httpCtx, cancelHTTP := context.WithCancelCause(ctx)
	defer cancelHTTP(nil)
	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, c.url, bytes.NewReader(encoded))
	if err != nil {
		return protocol.Response{}, errors.New("invalid DeepSeek endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	interval := c.idleTimeout
	if interval <= 0 {
		interval = streamIdleTimeout
	}
	watch := newIdleWatch(cancelHTTP, interval)
	defer func() {
		watch.stop()
		// Preserve an HTTP-status error's existing retry policy while recording
		// that its body stopped arriving. Never replace a parser's finer reason.
		if ctx.Err() == nil && watch.timedOut() && diagnostic != nil {
			if diagnostic.StreamFailure == "" {
				diagnostic.StreamFailure = streamIdle
			}
		}
	}()
	resp, err := c.http.Do(req)
	if err != nil {
		if diagnostic != nil {
			diagnostic.TransportError = err
		}
		if ctx.Err() != nil {
			return protocol.Response{}, ctx.Err()
		}
		if watch.timedOut() {
			return protocol.Response{}, modelFailure{Err: errors.Join(protocol.ErrModelUnavailable, errStreamIdleTimeout), Retryable: true}
		}
		return protocol.Response{}, modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true}
	}
	defer resp.Body.Close()
	if diagnostic != nil {
		diagnostic.HTTPStatus = resp.StatusCode
	}
	resp.Body = responseBody{ReadCloser: resp.Body, watch: watch, diagnostic: diagnostic}
	if resp.StatusCode != http.StatusOK {
		err := httpFailure(resp, diagnostic)
		if diagnostic != nil && c.key != "" {
			diagnostic.HTTPErrorBody = strings.ReplaceAll(diagnostic.HTTPErrorBody, c.key, "[redacted]")
		}
		return protocol.Response{}, err
	}
	if strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])) != "text/event-stream" {
		return protocol.Response{}, modelFailure{Err: protocol.ErrModelOutput}
	}
	result, err := decodeStream(resp.Body, diagnostic)
	var failure modelFailure
	if errors.As(err, &failure) && failure.Retryable {
		failure.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
		err = failure
	}
	if ctx.Err() != nil {
		return protocol.Response{}, ctx.Err()
	}
	return result, err
}

// Reading one HTTP body updates both idle detection and optional observation.
type responseBody struct {
	io.ReadCloser
	watch      *idleWatch
	diagnostic *protocol.RequestDiagnostic
}

func (r responseBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.watch.received()
	}
	if r.watch.timedOut() {
		err = errStreamIdleTimeout
	}
	if n > 0 && r.diagnostic != nil {
		if r.diagnostic.FirstByte == nil {
			now := time.Now().UTC()
			r.diagnostic.FirstByte = &now
		}
		r.diagnostic.ResponseBytes += int64(n)
	}
	return n, err
}
