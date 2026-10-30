package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/protocol"
)

type observationFuncs struct {
	started  func(context.Context, string, []byte) error
	finished func(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error)
}

func (o observationFuncs) BeforeModelRequest(ctx context.Context, id string, body []byte) error {
	if o.started != nil {
		return o.started(ctx, id, body)
	}
	return nil
}
func (o observationFuncs) RecordModelRequest(ctx context.Context, data *protocol.RequestDiagnostic, response *protocol.Response, err error) {
	if o.finished != nil {
		o.finished(ctx, data, response, err)
	}
}
func (observationFuncs) NotifyReplyRequired()                                          {}
func (observationFuncs) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
func (observationFuncs) DumpContext(context.Context, string, []byte)                   {}

type attemptObservation struct {
	protocol.RequestDiagnostic
	completed int
	pause     error
	result    protocol.Response
	err       error
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelObservationIsIndependentOfImplementation(t *testing.T) {
	previous := protocol.Observer
	t.Cleanup(func() { protocol.Observer = previous })
	for _, mode := range []string{"success", "broken-stream", "pause-refusal", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			observation := &attemptObservation{}
			if mode == "pause-refusal" {
				observation.pause = errors.New("pause unavailable")
			}
			type opaqueKey struct{}
			protocol.Observer = observationFuncs{
				started: func(ctx context.Context, id string, wire []byte) error {
					if ctx.Value(opaqueKey{}) != "opaque" || id == "" || !strings.Contains(string(wire), `"model":"deepseek-flash"`) {
						t.Fatal("lost opaque context, request identity or frozen wire body")
					}
					return observation.pause
				},
				finished: func(_ context.Context, data *protocol.RequestDiagnostic, response *protocol.Response, err error) {
					observation.RequestDiagnostic = *data
					observation.completed++
					observation.result, observation.err = *response, err
				},
			}
			if mode == "disabled" {
				protocol.Observer = nil
			}
			client := &client{attempts: 1, key: "secret", url: endpoint, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				body := event(toolCallStart) + event(toolCallFinish) + event("[DONE]")
				if mode == "broken-stream" {
					body = event(toolCallStart)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			ctx := context.WithValue(context.Background(), opaqueKey{}, "opaque")
			response, err := client.Invoke(ctx, requestFixture())
			if mode == "pause-refusal" {
				if calls != 0 || err == nil || observation.completed != 1 {
					t.Fatal("pause refusal allowed transport", calls, err)
				}
				return
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			if mode == "broken-stream" {
				if err == nil || observation.StreamFailure == "" || observation.err == nil {
					t.Fatal("stream failure not observed", err)
				}
				return
			}
			if err != nil || response.FinishReason != "tool_calls" {
				t.Fatal(response, err)
			}
			if mode == "disabled" {
				if observation.completed != 0 {
					t.Fatal("disabled observation ran")
				}
				return
			}
			if observation.completed != 1 || observation.HTTPStatus != 200 || observation.ResponseBytes == 0 || observation.FirstByte == nil || observation.Ended == nil || observation.result.FinishReason != "tool_calls" {
				t.Fatal("incomplete observation", observation)
			}
		})
	}
}

func TestClientOwnsRetryBudgetAndCancellation(t *testing.T) {
	for _, mode := range []string{"recover", "exhausted", "cancel-wait"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				previous := protocol.Observer
				protocol.Observer = nil
				t.Cleanup(func() { protocol.Observer = previous })
				ctx := context.Background()
				if mode == "cancel-wait" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				}
				calls := 0
				client := &client{key: "secret", url: endpoint, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					status, body := 503, `{"error":{"message":"private supplier detail"}}`
					if mode == "recover" && calls == 2 {
						status = 200
						body = event(toolCallStart) + event(toolCallFinish) + event("[DONE]")
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}}
				response, err := client.Invoke(ctx, requestFixture())
				switch mode {
				case "recover":
					if calls != 2 || err != nil || response.Usage.OutputTokens != 20 {
						t.Fatal(calls, response, err)
					}
				case "exhausted":
					if calls != 6 || !errors.Is(err, protocol.ErrModelUnavailable) || strings.Contains(err.Error(), "private") {
						t.Fatal(calls, err)
					}
				case "cancel-wait":
					if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal(calls, err)
					}
				}
			})
		})
	}
}

func TestHTTPFailureObservationRetainsBoundedBodyAndRequest(t *testing.T) {
	previous := protocol.Observer
	t.Cleanup(func() { protocol.Observer = previous })
	var observed protocol.RequestDiagnostic
	protocol.Observer = observationFuncs{finished: func(_ context.Context, d *protocol.RequestDiagnostic, _ *protocol.Response, _ error) {
		observed = *d
	}}
	c := &client{attempts: 1, key: "secret-key", url: endpoint, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader("invalid secret-key " + strings.Repeat("x", 70<<10)))}, nil
	})}}
	_, err := c.Invoke(context.Background(), requestFixture())
	if !errors.Is(err, protocol.ErrModelUnavailable) || strings.Contains(err.Error(), "invalid") {
		t.Fatal("provider text escaped into public error", err)
	}
	if observed.HTTPStatus != 400 || !strings.HasPrefix(observed.HTTPErrorBody, "invalid [redacted]") || len(observed.HTTPErrorBody) > (64<<10)+10 || strings.Contains(observed.HTTPErrorBody, c.key) {
		t.Fatal("missing bounded, redacted provider body")
	}
	if !strings.Contains(string(observed.RequestBody), `"messages"`) || strings.Contains(string(observed.RequestBody), c.key) {
		t.Fatal("missing frozen request or credential exposure")
	}
}
