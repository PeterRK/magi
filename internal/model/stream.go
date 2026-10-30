package model

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"magi/internal/core/protocol"
)

// These fixed labels describe the failed guard, never provider text or identities.
const (
	streamEOF             = "eof"
	streamReadError       = "read_error"
	streamIdle            = "idle_timeout"
	streamInvalidChunk    = "invalid_chunk"
	streamProviderError   = "provider_error"
	streamEmptyResponse   = "empty_response"
	streamServerFinish    = "server_finish"
	streamAfterFinish     = "after_finish"
	streamChoiceCount     = "choice_count"
	streamChoiceIndex     = "choice_index"
	streamChoiceRole      = "choice_role"
	streamToolIndexRange  = "tool_index_range"
	streamToolIndexGap    = "tool_index_gap"
	streamRepeatedID      = "identity_repeat_id"
	streamConflictingID   = "identity_conflict_id"
	streamRepeatedType    = "identity_repeat_type"
	streamConflictingType = "identity_conflict_type"
	streamRepeatedName    = "identity_repeat_name"
	streamConflictingName = "identity_conflict_name"
	streamUsageNoFinish   = "usage_without_finish"
	streamUsageMissing    = "usage_missing_fields"
	streamUsageInvalid    = "usage_invalid_totals"
	streamDoneNoFinish    = "done_missing_finish"
	streamDoneNoUsage     = "done_missing_usage"
	streamEnvelope        = "envelope"
)

func decodeStream(reader io.Reader, diagnostic *protocol.RequestDiagnostic) (protocol.Response, error) {
	buffer := bufio.NewReader(reader)
	var state streamDecoder
	state.result.Message.Role = "assistant"
	var event strings.Builder
	fail := func(reason string) (protocol.Response, error) {
		if diagnostic != nil {
			diagnostic.StreamFailure = reason
		}
		return state.failure(reason)
	}
	for {
		line, err := buffer.ReadString('\n')
		if err != nil {
			// An unterminated tail never completes an event or admits tools.
			switch {
			case errors.Is(err, io.EOF):
				return fail(streamEOF)
			case errors.Is(err, errStreamIdleTimeout):
				return fail(streamIdle)
			default:
				return fail(streamReadError)
			}
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if event.Len() == 0 {
				continue
			}
			data := strings.TrimSuffix(event.String(), "\n")
			event.Reset()
			if data == "[DONE]" {
				if reason := state.complete(); reason != "" {
					return fail(reason)
				}
				return state.result, nil
			}
			if reason := state.consume(data); reason != "" {
				return fail(reason)
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			event.WriteByte('\n')
		}
	}
}
