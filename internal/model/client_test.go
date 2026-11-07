package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/protocol"
)

func TestClientRejectsInvalidRequestBeforeTransport(t *testing.T) {
	// Request validation itself belongs to protocol; check the transport boundary.
	if _, err := (&client{}).Invoke(context.Background(), protocol.Request{}); err == nil {
		t.Fatal("invalid request reached transport")
	}
}

func TestFromEnvAndRedirectPolicy(t *testing.T) {
	for _, key := range []string{"", "secret\n", " secret", "密钥"} {
		t.Setenv("DEEPSEEK_API_KEY", key)
		if _, err := FromEnv(); err == nil {
			t.Fatal("accepted absent/malformed key")
		}
	}
	t.Setenv("DEEPSEEK_API_KEY", "test-secret")
	provider, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	client := provider.(*client)
	if client.http.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect allowed")
	}
	if client.InputCapacity()+modelLongMaxTokens >= modelContextTokens {
		t.Fatal("usable input capacity leaves no compaction output room")
	}
}

func TestHTTPProjectionAndFragmentedTool(t *testing.T) {
	request := requestFixture()
	request.Messages[0].Content += " <T> A&B\u2028\u2029"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("incorrect HTTP request")
		}
		var body struct {
			wireRequest
			TopP json.RawMessage `json:"top_p"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`, `\u2028`, `\u2029`} {
			if strings.Contains(string(raw), escape) {
				t.Errorf("extra escape in HTTP request: %s", escape)
			}
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		if body.Model != modelName || body.Thinking.Type != "enabled" || body.Effort != "high" || len(body.TopP) != 0 || !body.Stream || !body.StreamOptions.IncludeUsage || body.MaxTokens != modelMaxTokens {
			t.Error("provider configuration changed")
		}
		if !reflect.DeepEqual(body.Messages, request.Messages) {
			t.Error("native reasoning/history changed")
		}
		before, _ := json.Marshal(request.Tools)
		after, _ := json.Marshal(body.Tools)
		if string(before) != string(after) {
			t.Error("native tools changed")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for _, part := range []string{": keepalive\n\n", event(toolCallStart), event(toolCallFinish), event("[DONE]")} {
			_, _ = io.WriteString(w, part)
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	client := &client{attempts: 1, key: "test-secret", url: server.URL, http: server.Client()}
	response, err := client.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.Total() != 120 || response.Usage.OutputTokens != 20 || *response.Message.ReasoningContent != "检查。" {
		t.Fatalf("bad response: %+v", response)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Arguments != `{"ref":"P1"}` {
		t.Fatalf("raw tool call changed: %+v", response.Message.ToolCalls)
	}
}

func TestOutputLimitFallback(t *testing.T) {
	// Cover each initial effort and downgrade edge; one long-output chain
	// checks that its budget survives every downgrade without a Cartesian matrix.
	for _, tc := range []struct {
		name       string
		effort     protocol.Effort
		longOutput bool
		finishes   []string
		err        error
	}{
		{"high recover", protocol.EffortHigh, false, []string{"length", "tool_calls"}, nil},
		{"high hint recover", protocol.EffortHigh, false, []string{"length", "length", "tool_calls"}, nil},
		{"high exhausted", protocol.EffortHigh, false, []string{"length", "length", "length"}, protocol.ErrModelOutput},
		{"low recover", protocol.EffortLow, false, []string{"length", "tool_calls"}, nil},
		{"low exhausted", protocol.EffortLow, false, []string{"length", "length"}, protocol.ErrModelOutput},
		{"max hint recover", protocol.EffortMax, true, []string{"length", "length", "tool_calls"}, nil},
		{"transport failure", protocol.EffortHigh, false, []string{"length", "transport_error"}, protocol.ErrModelUnavailable},
		{"content filter", protocol.EffortHigh, false, []string{"content_filter"}, nil},
		{"incomplete stream", protocol.EffortHigh, false, []string{"incomplete"}, protocol.ErrModelOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := requestFixture()
			request.Effort, request.LongOutput = tc.effort, tc.longOutput
			wantMax := 102_400
			if tc.longOutput {
				wantMax = 204_800
			}
			// Leave spare capacity: the retry must not overwrite its caller's storage.
			backing := append(request.Messages, protocol.Message{Role: "user", Content: "outside request"})
			request.Messages = backing[:len(backing)-1]
			original := protocol.CloneRequest(request)
			var sent []wireRequest
			client := &client{attempts: 1, url: endpoint, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body wireRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				stages := map[protocol.Effort][]string{
					protocol.EffortHigh: {"high", "low", "low"},
					protocol.EffortMax:  {"max", "low", "low"},
					protocol.EffortLow:  {"low", "low"},
				}[tc.effort]
				index := len(sent)
				if index >= len(tc.finishes) {
					index = 0 // A later invocation must start at its original effort.
				}
				if index >= len(stages) {
					t.Fatal("retried beyond the output limit")
				}
				if body.Effort != stages[index] || body.Thinking.Type != "enabled" || body.MaxTokens != wantMax {
					t.Fatal("incorrect generation settings", body.Effort, body.Thinking.Type, body.MaxTokens)
				}
				if len(sent) > 0 {
					want := sent[0]
					want.Effort, want.Thinking.Type = body.Effort, body.Thinking.Type
					if index > 0 && index == len(stages)-1 {
						want.Messages = append(append([]protocol.Message(nil), want.Messages...), protocol.Message{Role: "system", Content: outputLimitHint})
					}
					if !reflect.DeepEqual(body, want) {
						t.Fatal("fallback changed input beyond the temporary hint")
					}
				}
				sent = append(sent, body)
				finish := "tool_calls" // A later independent invocation completes normally.
				if len(sent) <= len(tc.finishes) {
					finish = tc.finishes[len(sent)-1]
				}
				status := http.StatusOK
				stream := event(toolCallStart) + event(strings.Replace(toolCallFinish, `"finish_reason":"tool_calls"`, `"finish_reason":"`+finish+`"`, 1)) + event("[DONE]")
				switch finish {
				case "length":
					stream = strings.ReplaceAll(stream, "call-next", "discarded-call")
				case "incomplete":
					stream = strings.TrimSuffix(strings.ReplaceAll(stream, "incomplete", "length"), event("[DONE]"))
				case "transport_error":
					status, stream = http.StatusServiceUnavailable, ""
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})}}
			response, err := client.Invoke(context.Background(), request)
			if !errors.Is(err, tc.err) || len(sent) != len(tc.finishes) {
				t.Fatal("unexpected retry outcome", len(sent), err)
			}
			if tc.err != nil {
				if !reflect.DeepEqual(response, protocol.Response{}) {
					t.Fatal("failed generation leaked output or usage")
				}
			} else if response.FinishReason != tc.finishes[len(tc.finishes)-1] {
				t.Fatal("finish reason changed", response.FinishReason)
			} else if response.FinishReason == "tool_calls" {
				if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].ID != "call-next" || response.Usage.OutputTokens != 20 {
					t.Fatal("discarded generation contaminated the completed result")
				}
				if len(tc.finishes) > 1 {
					if _, err := client.Invoke(context.Background(), request); err != nil || len(sent) != len(tc.finishes)+1 {
						t.Fatal("independent invocation failed", err)
					}
				}
			}
			if !reflect.DeepEqual(request, original) || backing[len(backing)-1].Content != "outside request" {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestOutputLimitFallbackKeepsTransportBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := &client{attempts: 2, url: endpoint, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			var body wireRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if calls == 3 && body.Effort != "low" {
				t.Fatal("lost fallback effort")
			}
			status, stream := http.StatusServiceUnavailable, ""
			if calls == 2 {
				status = http.StatusOK
				stream = event(toolCallStart) + event(strings.Replace(toolCallFinish, `"finish_reason":"tool_calls"`, `"finish_reason":"length"`, 1)) + event("[DONE]")
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
		})}}
		response, err := client.Invoke(context.Background(), requestFixture())
		if calls != 3 || !errors.Is(err, protocol.ErrModelUnavailable) || !reflect.DeepEqual(response, protocol.Response{}) {
			t.Fatal("fallback reset the transport retry budget", calls, err)
		}
	})
}

func TestHTTPPropagatesRetryAfterForStatusAndStreamErrors(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &client{url: endpoint, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Retry-After": {"17"}, "Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(event(`{"error":{"type":"overloaded_error"}}`))),
				}, nil
			})}}
			_, err := client.invokeObserved(context.Background(), []byte(`{}`))
			var failure modelFailure
			if !errors.Is(err, protocol.ErrModelUnavailable) || !errors.As(err, &failure) || !failure.Retryable || failure.RetryAfter != 17*time.Second {
				t.Fatalf("lost provider retry policy: %v", err)
			}
		})
	}
}
