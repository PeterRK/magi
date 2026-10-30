package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"magi/internal/core/protocol"
)

func TestStreamAcceptsLargeEventsAndKeepalives(t *testing.T) {
	// Exercise the former event and wire limits without changing the response.
	content := strings.Repeat("x", (4<<20)+1)
	chunk := strings.Replace(toolCallStart, `"role":"assistant"`, fmt.Sprintf(`"role":"assistant","content":%q`, content), 1)
	for _, tc := range []struct{ name, data, content string }{
		{"single_line", chunk, content},
		// The same JSON event may span many small data lines.
		{"multiline", strings.Repeat(strings.Repeat(" ", 1024)+"\ndata: ", (4<<20)/1024+1) + toolCallStart, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := strings.Repeat(":"+strings.Repeat(" ", 1024)+"\n\n", (32<<20)/1024+1)
			stream := io.MultiReader(strings.NewReader(prefix), strings.NewReader(event(tc.data)+event(toolCallFinish)+event("[DONE]")))
			response, err := readStream(stream)
			if err != nil {
				t.Fatal(err)
			}
			if response.Message.Content != tc.content || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Arguments != `{"ref":"P1"}` {
				t.Fatal("large stream changed content or tool arguments")
			}
		})
	}
}

func TestStreamHandlesFragmentedCRLF(t *testing.T) {
	stream := event(toolCallStart) + event(toolCallFinish) + event("[DONE]")
	stream = strings.ReplaceAll(stream, "\n", "\r\n")
	response, err := readStream(iotest.OneByteReader(strings.NewReader(stream)))
	if err != nil || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Function.Arguments != `{"ref":"P1"}` {
		t.Fatalf("fragmented CRLF stream failed: %v", err)
	}
}

func TestStreamLeavesMalformedArgumentsForRuntimeRepair(t *testing.T) {
	stream := event(toolCallStart) + event(strings.Replace(toolCallFinish, `\"P1\"}`, `broken`, 1)) + event("[DONE]")
	response, err := readStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 1 || json.Valid([]byte(response.Message.ToolCalls[0].Function.Arguments)) {
		t.Fatal("transport lost or repaired the malformed arguments")
	}
}

func TestStreamInterleavedCallsSurviveBufferGrowth(t *testing.T) {
	const count = 257 // Cross the former 128-call limit while growing argument buffers.
	var stream strings.Builder
	stream.WriteString(event(toolCallStart))
	for i := 1; i < count; i++ {
		stream.WriteString(event(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call-%d","type":"function","function":{"name":"history_read","arguments":"{\"ref\":"}}]}}]}`, i, i)))
	}
	// Complete arguments in reverse order after all the buffers have grown.
	for i := count - 1; i > 0; i-- {
		arguments := fmt.Sprintf(`"P%d"}`, i+1)
		stream.WriteString(event(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%q}}]}}]}`, i, arguments)))
	}
	stream.WriteString(event(toolCallFinish) + event("[DONE]"))
	response, err := readStream(strings.NewReader(stream.String()))
	if err != nil {
		t.Fatal(err)
	}
	calls, err := protocol.ValidateToolResponse(requestFixture().Tools, response)
	if err != nil || len(calls) != count {
		t.Fatalf("batch validation: calls=%d err=%v", len(calls), err)
	}
	for i, call := range response.Message.ToolCalls {
		want := fmt.Sprintf(`{"ref":"P%d"}`, i+1)
		if got := call.Function.Arguments; got != want {
			t.Fatalf("call %d arguments: got %q want %q", i, got, want)
		}
	}
}

func TestStreamEmptyReasoningAndTruncation(t *testing.T) {
	for _, reasoning := range []string{"", `"reasoning_content":"",`} {
		for _, finish := range []string{"stop", "length", "content_filter"} {
			stream := event(fmt.Sprintf(`{"choices":[{"index":0,"delta":{%s"content":"text"},"finish_reason":%q}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, reasoning, finish)) + event("[DONE]")
			response, err := readStream(strings.NewReader(stream))
			if err != nil || (response.Message.ReasoningContent != nil) != (reasoning != "") || response.FinishReason != finish {
				t.Fatalf("%s: %+v %v", finish, response, err)
			}
			if response.Message.ReasoningContent != nil && *response.Message.ReasoningContent != "" {
				t.Fatal("empty reasoning changed")
			}
		}
	}
}

func TestInterruptedStreamDiscardsResponse(t *testing.T) {
	for _, finish := range []string{"insufficient_system_resource", "aborted"} {
		stream := event(toolCallStart) + event(strings.Replace(toolCallFinish, `"finish_reason":"tool_calls"`, `"finish_reason":"`+finish+`"`, 1)) + event("[DONE]")
		for _, body := range []string{stream, event(fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, finish)) + event("[DONE]")} {
			response, err := readStream(strings.NewReader(body))
			var failure modelFailure
			if !errors.As(err, &failure) || !failure.Retryable {
				t.Fatalf("lost retry classification: %v", err)
			}
			if !reflect.DeepEqual(response, protocol.Response{}) {
				t.Fatal("interrupted output retained")
			}
		}
	}
}

func TestEmptyCompletedResponseRetriesWithoutAdmittingReasoning(t *testing.T) {
	for _, thought := range []string{"", "private-thought"} {
		stream := event(fmt.Sprintf(`{"choices":[{"index":0,"delta":{"reasoning_content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, thought)) + event("[DONE]")
		response, err := readStream(strings.NewReader(stream))
		var failure modelFailure
		if !errors.As(err, &failure) || !failure.Retryable || !reflect.DeepEqual(response, protocol.Response{}) {
			t.Fatalf("empty response did not safely retry: %+v %v", response, err)
		}
	}
}

func TestKnownPermanentFinishDoesNotRetryMissingTail(t *testing.T) {
	for _, finish := range []string{"length", "content_filter", "unknown"} {
		stream := event(toolCallStart) + event(strings.Replace(toolCallFinish, `"finish_reason":"tool_calls"`, `"finish_reason":"`+finish+`"`, 1))
		response, err := readStream(strings.NewReader(stream))
		var failure modelFailure
		if !errors.As(err, &failure) || failure.Retryable || !errors.Is(err, protocol.ErrModelOutput) || len(response.Message.ToolCalls) != 0 {
			t.Fatalf("permanent finish retried: %s %v", finish, err)
		}
	}
}

func TestStreamPreservesProviderTokenDetails(t *testing.T) {
	stream := func(extra string) string {
		return event(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120`+extra+`}}`) + event("[DONE]")
	}
	for _, tc := range []struct {
		name, extra string
		valid       bool
	}{
		{"missing", "", true},
		{"complete", `,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40,"completion_tokens_details":{"reasoning_tokens":12}`, true},
		{"zero", `,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":100,"completion_tokens_details":{"reasoning_tokens":0}`, true},
		{"partial", `,"prompt_cache_hit_tokens":60`, true},
		{"negative", `,"prompt_cache_hit_tokens":-1`, false},
		{"fractional", `,"prompt_cache_hit_tokens":0.5`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := readStream(strings.NewReader(stream(tc.extra)))
			if !tc.valid {
				if !errors.Is(err, protocol.ErrModelOutput) {
					t.Fatalf("expected invalid model output, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage.Total() != 120 {
				t.Fatal("reasoning or cache counted twice")
			}
			if tc.name == "missing" {
				if result.TokenDetails != nil {
					t.Fatal("missing details became known zero")
				}
				return
			}
			details := result.TokenDetails
			if details == nil || details.CacheHitTokens == nil {
				t.Fatal("cache hit not retained")
			}
			if tc.name == "complete" && (*details.CacheHitTokens != 60 || details.CacheMissTokens == nil || *details.CacheMissTokens != 40 || details.ReasoningTokens == nil || *details.ReasoningTokens != 12) {
				t.Fatal("provider details changed")
			}
			if tc.name == "zero" && (*details.CacheHitTokens != 0 || details.ReasoningTokens == nil || *details.ReasoningTokens != 0) {
				t.Fatal("known zero became unknown")
			}
			if tc.name == "partial" && (details.CacheMissTokens != nil || details.ReasoningTokens != nil) {
				t.Fatal("unreported details synthesized")
			}
		})
	}
}

type streamFailureReader struct{}

func (streamFailureReader) Read([]byte) (int, error) {
	return 0, errors.New("private-reader-error-sentinel")
}

func checkStreamFailure(t *testing.T, reader io.Reader, reason string) protocol.RequestDiagnostic {
	t.Helper()
	observation := protocol.RequestDiagnostic{}
	response, err := decodeStream(reader, &observation)
	var failure modelFailure
	transport := reason == streamEOF || reason == streamReadError
	recoverable := transport || reason == streamProviderError
	expectedErr := protocol.ErrModelOutput
	if recoverable {
		expectedErr = protocol.ErrModelUnavailable
	}
	if !errors.As(err, &failure) || !errors.Is(err, expectedErr) || failure.Retryable != recoverable {
		t.Fatalf("failure classification changed: %v", err)
	}
	if !reflect.DeepEqual(response, protocol.Response{}) {
		t.Fatal("failed stream returned a usable response")
	}
	if observation.StreamFailure != reason {
		t.Fatalf("wrong failure event: %q", observation.StreamFailure)
	}
	for _, secret := range []string{"private-reader-error-sentinel", "private-wire-sentinel", "call-next", "history_read", "P1", "检查。", "reasoning_content"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("model error leaked %q", secret)
		}
	}
	return observation
}

func TestStreamFailureEOFAndReadError(t *testing.T) {
	t.Run("empty_eof", func(t *testing.T) {
		checkStreamFailure(t, strings.NewReader(""), streamEOF)
	})
	t.Run("partial_eof", func(t *testing.T) {
		checkStreamFailure(t, strings.NewReader(event(toolCallStart)), streamEOF)
	})
	t.Run("missing_done_after_finish", func(t *testing.T) {
		checkStreamFailure(t, strings.NewReader(event(toolCallStart)+event(toolCallFinish)), streamEOF)
	})
	t.Run("reader_error", func(t *testing.T) {
		checkStreamFailure(t, io.MultiReader(strings.NewReader(event(toolCallStart)), streamFailureReader{}), streamReadError)
	})
	t.Run("finish_then_read_error", func(t *testing.T) {
		checkStreamFailure(t, io.MultiReader(strings.NewReader(event(toolCallStart)+event(toolCallFinish)), streamFailureReader{}), streamReadError)
	})
	t.Run("finish_without_usage", func(t *testing.T) {
		checkStreamFailure(t, strings.NewReader(event(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)), streamEOF)
	})
}

func TestStreamRejectsUnterminatedTail(t *testing.T) {
	for _, tail := range []string{"data: [DONE]", "data: [DONE]\n", "data: [DONE]\r\n\r"} {
		stream := event(toolCallStart) + event(toolCallFinish) + tail
		checkStreamFailure(t, iotest.OneByteReader(strings.NewReader(stream)), streamEOF)
	}
}

func TestStreamFailureIdentityAndChoice(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reason     string
	}{
		{"duplicate_chunk_field", event(strings.Replace(toolCallStart, `"index":0`, `"index":0,"index":1`, 1)), streamInvalidChunk},
		{"invalid_unicode", event(strings.Replace(toolCallStart, "检查。", `\ud800`, 1)), streamInvalidChunk},
		{"partial_provider_error", event(toolCallStart) + event(`{"error":{"message":"private-wire-sentinel"}}`), streamProviderError},
		{"chunk_json", event(`{"choices":[],"choices":[]}`), streamInvalidChunk},
		{"provider_error", event(`{"error":{"message":"private-wire-sentinel"}}`), streamProviderError},
		{"choices_count", event(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), streamChoiceCount},
		{"choice_index", event(strings.Replace(toolCallStart, `"index":0`, `"index":1`, 1)), streamChoiceIndex},
		{"tool_index_missing", event(strings.Replace(toolCallStart, `"index":0,"id"`, `"id"`, 1)), streamToolIndexRange},
		{"tool_index_negative", event(strings.Replace(toolCallStart, `"index":0,"id"`, `"index":-1,"id"`, 1)), streamToolIndexRange},
		{"tool_index_gap", event(strings.Replace(toolCallStart, `"index":0,"id"`, `"index":2,"id"`, 1)), streamToolIndexGap},
		{"tool_index_large_gap", event(strings.Replace(toolCallStart, `"index":0,"id"`, `"index":2147483647,"id"`, 1)), streamToolIndexGap},
		{"repeated_id", event(toolCallStart) + event(toolCallStart), streamRepeatedID},
		{"conflicting_id", event(toolCallStart) + event(strings.Replace(toolCallStart, "call-next", "private-wire-sentinel", 1)), streamConflictingID},
		{"repeated_type", event(toolCallStart) + event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function"}]}}]}`), streamRepeatedType},
		{"conflicting_type", event(toolCallStart) + event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"private-wire-sentinel"}]}}]}`), streamConflictingType},
		{"repeated_name", event(toolCallStart) + event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"history_read"}}]}}]}`), streamRepeatedName},
		{"conflicting_name", event(toolCallStart) + event(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"private-wire-sentinel"}}]}}]}`), streamConflictingName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkStreamFailure(t, strings.NewReader(tc.body), tc.reason)
		})
	}
}

func TestStreamFailureUsageAndEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reason     string
	}{
		{"done_before_finish", event(toolCallStart) + event("[DONE]"), streamDoneNoFinish},
		{"done_without_usage", event(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) + event("[DONE]"), streamDoneNoUsage},
		{"usage_before_finish", event(`{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), streamUsageNoFinish},
		{"usage_missing_fields", event(toolCallStart) + event(strings.Replace(toolCallFinish, `"prompt_tokens":100,`, "", 1)), streamUsageMissing},
		{"usage_invalid_total", event(toolCallStart) + event(strings.Replace(toolCallFinish, `"total_tokens":120`, `"total_tokens":121`, 1)), streamUsageInvalid},
		{"after_finish", event(toolCallStart) + event(toolCallFinish) + event(toolCallFinish), streamAfterFinish},
		{"envelope_token_details", event(toolCallStart) + event(strings.Replace(toolCallFinish, `"reasoning_tokens":12`, `"reasoning_tokens":21`, 1)) + event("[DONE]"), streamEnvelope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkStreamFailure(t, strings.NewReader(tc.body), tc.reason)
		})
	}
}
