package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/protocol"
)

func TestHTTPErrorClassificationAndRedaction(t *testing.T) {
	for _, status := range []int{400, 401, 402, 422, 429, 500, 503, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test-secret"}}`))}, nil
			})
			classified := httpFailure(&http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test-secret"}}`))}, nil)
			if _, retry := retryDelay(classified, 0, 5); retry != (status == 429 || status >= 500) {
				t.Fatal("HTTP retry policy changed", classified)
			}
			client := &client{attempts: 1, key: "test-secret", url: endpoint, http: &http.Client{Transport: transport}}
			_, err := client.Invoke(context.Background(), requestFixture())
			if !errors.Is(err, protocol.ErrModelUnavailable) || err.Error() != fmt.Sprintf("model unavailable: DeepSeek HTTP %d", status) {
				t.Fatalf("wrong failure: %v", err)
			}
		})
	}
	err := httpFailure(&http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"context_length_exceeded"}}`))}, nil)
	var failure modelFailure
	if !errors.As(err, &failure) || !errors.Is(err, protocol.ErrContextOverflow) || failure.Retryable {
		t.Fatalf("overflow=%v", err)
	}
}

func TestInBandErrorsDistinguishTransientFromPermanent(t *testing.T) {
	for _, tc := range []struct {
		label           string
		retry, overflow bool
	}{
		{"api_error", true, false}, {"overloaded_error", true, false},
		{"rate_limit_error", true, false}, {"unknown_server_error", true, false},
		{"authentication_error", false, false}, {"permission_error", false, false},
		{"insufficient_quota", false, false}, {"insufficient_balance", false, false},
		{"invalid_request_error", false, false}, {"context_length_exceeded", false, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			prefixes := []string{""}
			if tc.label == "api_error" || tc.label == "authentication_error" {
				prefixes = append(prefixes, event(toolCallStart), event(toolCallStart)+event(toolCallFinish))
			}
			for _, prefix := range prefixes {
				body := fmt.Sprintf(`{"error":{"code":%q,"message":"private-provider-text"}}`, tc.label)
				response, err := readStream(strings.NewReader(prefix + event(body)))
				var failure modelFailure
				if !errors.As(err, &failure) || failure.Retryable != tc.retry || errors.Is(err, protocol.ErrContextOverflow) != tc.overflow || strings.Contains(err.Error(), "private-provider-text") {
					t.Fatalf("classification lost: %+v %v", failure, err)
				}
				if response.Message.Content != "" || response.Message.ReasoningContent != nil || len(response.Message.ToolCalls) != 0 {
					t.Fatal("provider error retained partial output")
				}
			}
		})
	}
	_, err := readStream(strings.NewReader(event(`{"error":"private-provider-text"}`)))
	if !errors.Is(err, protocol.ErrModelOutput) {
		t.Fatal("malformed error treated as transient")
	}
}

func TestDeepSeekMaximumContextErrorClassification(t *testing.T) {
	// Exact error fields from the retained real synthetic rejection; no prompt,
	// provider request identity, credential, or benchmark answer is needed.
	real := `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1166393 tokens (1100857 in the messages, 65536 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
	for _, tc := range []struct {
		name, body string
		status     int
		overflow   bool
	}{
		{"real", real, 400, true},
		{"structured", `{"error":{"code":"context_length_exceeded"}}`, 422, true},
		{"explicit413", real, 413, true},
		{"ordinary_invalid", `{"error":{"code":"invalid_request_error","message":"max_tokens exceeds output limit"}}`, 400, false},
		{"unstructured_body_limit", `{"error":{"code":"invalid_request_error","message":"request body too large"}}`, 413, false},
		{"auth", real, 401, false},
		{"server", real, 500, false},
		{"malformed", `{"error":`, 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := httpFailure(&http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil)
			var f modelFailure
			if !errors.As(err, &f) || errors.Is(err, protocol.ErrContextOverflow) != tc.overflow || f.Retryable != (tc.status >= 500) || strings.Contains(err.Error(), "maximum context") {
				t.Fatalf("classification=%+v err=%v", f, err)
			}
		})
	}
	for _, prefix := range []string{"", event(toolCallStart), event(toolCallStart) + event(toolCallFinish)} {
		_, err := readStream(strings.NewReader(prefix + event(real)))
		var f modelFailure
		if !errors.As(err, &f) || !errors.Is(err, protocol.ErrContextOverflow) || f.Retryable || strings.Contains(err.Error(), "maximum context") {
			t.Fatalf("SSE classification=%+v err=%v", f, err)
		}
	}
}

func TestRetryDelayBoundaries(t *testing.T) {
	retryable := modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true}
	for _, tc := range []struct {
		name  string
		err   error
		retry uint64
		want  bool
		delay time.Duration
	}{
		{"transient", retryable, 0, true, 0},
		{"last allowance", retryable, 4, true, 0},
		{"exhausted", retryable, 5, false, 0},
		{"unknown", errors.New("unknown provider failure"), 0, false, 0},
		{"permanent", modelFailure{Err: protocol.ErrModelOutput}, 0, false, 0},
		{"overflow", modelFailure{Err: protocol.ErrContextOverflow, Retryable: true}, 0, false, 0},
		{"retry after", modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true, RetryAfter: 17 * time.Second}, 0, true, 17 * time.Second},
		{"retry after cap", modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true, RetryAfter: 32 * time.Second}, 0, true, 32 * time.Second},
		{"retry after too long", modelFailure{Err: protocol.ErrModelUnavailable, Retryable: true, RetryAfter: 32*time.Second + time.Nanosecond}, 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, retry := retryDelay(tc.err, tc.retry, 5)
			if retry != tc.want || tc.delay > 0 && delay != tc.delay {
				t.Fatalf("delay=%s retry=%v", delay, retry)
			}
			if retry && tc.delay == 0 {
				base := 2 * time.Second << min(tc.retry, 4)
				if delay < base*9/10 || delay > base {
					t.Fatalf("retry %d wait %s outside downward jitter", tc.retry, delay)
				}
			}
		})
	}
}

func TestRetryWaitHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- waitRetry(ctx, time.Hour) }()
		synctest.Wait() // Cancel after the retry has entered its timer wait.
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("retry wait ignored cancellation", err)
		}
		if err := waitRetry(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled context became ready: %v", err)
		}
	})
}

func TestPublicModelErrorPreservesSafeClassification(t *testing.T) {
	for _, cause := range []error{protocol.ErrModelOutput, protocol.ErrModelUnavailable, protocol.ErrContextOverflow} {
		err := publicError(modelFailure{Err: fmt.Errorf("%w: private provider payload", cause)})
		if !errors.Is(err, cause) || err.Error() != cause.Error() {
			t.Fatalf("unsafe or lost classification: %v", err)
		}
	}
}

func TestHTTPRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"2", 2 * time.Second}, {"0.5", 500 * time.Millisecond},
		{"10", 10 * time.Second}, {"17", 17 * time.Second}, {"32", 32 * time.Second},
		{"32.000000001", 33 * time.Second}, {"33", 33 * time.Second},
		{now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second},
		{now.Add(17 * time.Second).Format(http.TimeFormat), 17 * time.Second},
		{now.Add(32 * time.Second).Format(http.TimeFormat), 32 * time.Second},
		{now.Add(33 * time.Second).Format(http.TimeFormat), 33 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"0", 0}, {"-1", 0}, {"bad-header", 0}, {"Inf", 0}, {"NaN", 0}, {"9999999999999999999999999", 33 * time.Second},
	} {
		if got := retryAfter(tc.header, now); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.header, got, tc.want)
		}
	}
	err := httpFailure(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private-body"}}`))}, nil)
	var failure modelFailure
	if !errors.As(err, &failure) || !failure.Retryable || failure.RetryAfter != 2*time.Second || strings.Contains(err.Error(), "private-body") {
		t.Fatal("HTTP retry delay/redaction changed", err)
	}
}
