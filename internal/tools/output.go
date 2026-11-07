package tools

import (
	"encoding/base64"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

func bashOutcome(raw processResult, limit int, runErr error) (protocol.ToolResult, error) {
	if limit <= 0 {
		limit = inlineOutputBytes
	}
	status := executionStatus(runErr)
	diagnostic := raw.Diagnostic
	if runErr != nil {
		diagnostic = strings.TrimSpace(diagnostic + "\n" + runErr.Error())
	}
	result := bashResult{Status: status, ExitCode: raw.ExitCode}
	result.StdoutTail = outputTail(raw.Stdout, raw.StdoutTotal, limit)
	result.StderrTail = outputTail(raw.Stderr, raw.StderrTotal, limit)
	if !outputComplete(raw.Stdout, raw.StdoutTotal, limit) {
		result.StdoutDump = raw.StdoutPath
		result.StdoutTotal = raw.StdoutTotal
	}
	if !outputComplete(raw.Stderr, raw.StderrTotal, limit) {
		result.StderrDump = raw.StderrPath
		result.StderrTotal = raw.StderrTotal
	}
	if raw.CaptureIncomplete || raw.StdoutOmitted > 0 {
		result.StdoutTotal = raw.StdoutTotal
	}
	if raw.CaptureIncomplete || raw.StderrOmitted > 0 {
		result.StderrTotal = raw.StderrTotal
	}
	if raw.CaptureIncomplete {
		diagnostic = strings.TrimSpace(diagnostic + "\noutput capture is incomplete; missing byte count is unknown")
	}
	for _, channel := range []struct {
		name           string
		total, omitted int64
	}{{"stdout", raw.StdoutTotal, raw.StdoutOmitted}, {"stderr", raw.StderrTotal, raw.StderrOmitted}} {
		if channel.omitted > 0 {
			diagnostic = strings.TrimSpace(diagnostic + fmt.Sprintf("\n%s retention incomplete: retained %d of %d captured bytes; inline tail is independent.", channel.name, channel.total-channel.omitted, channel.total))
		}
	}
	result.Diagnostic = diagnostic
	body, _ := jsonv2.Marshal(result)
	return protocol.ToolResult{Status: status, Body: body}, runErr
}

// Completeness refers to the captured bytes actually represented inline, not
// to whether a command's output stream was fully drained.
func outputComplete(body []byte, total int64, limit int) bool {
	if !utf8.Valid(body) {
		limit = base64.RawStdEncoding.DecodedLen(limit)
	} else if tailLineStart(body) > 0 {
		return false
	}
	return int64(len(body)) == total && len(body) <= limit
}

func outputTail(body []byte, total int64, limit int) string {
	if len(body) > limit {
		body = body[len(body)-limit:]
	}
	// A byte-bounded tail can start inside a UTF-8 rune. Keep binary output
	// explicit, but do not misclassify an otherwise valid text suffix as binary.
	if total > int64(len(body)) && !utf8.Valid(body) {
		start := 0
		for start < len(body) && start < utf8.UTFMax && !utf8.RuneStart(body[start]) {
			start++
		}
		if utf8.Valid(body[start:]) {
			body = body[start:]
		}
	}
	if !utf8.Valid(body) {
		count := min(len(body), base64.RawStdEncoding.DecodedLen(limit))
		encoded := base64.RawStdEncoding.EncodeToString(body[len(body)-count:])
		return "[binary output; base64 tail: " + encoded + "; " + strconv.FormatInt(total-int64(count), 10) + " bytes omitted]"
	}
	return string(body[tailLineStart(body):])
}

func tailLineStart(body []byte) int {
	end := len(body)
	if end > 0 && body[end-1] == '\n' {
		end--
	}
	for lines, i := 0, end-1; i >= 0; i-- {
		if body[i] == '\n' {
			lines++
			if lines == inlineOutputLines {
				return i + 1
			}
		}
	}
	return 0
}

// bashResult is specific to process execution; other tools return their own data.
type bashResult struct {
	Status      protocol.ToolCallStatus `json:"status"`
	ExitCode    int                     `json:"exit_code"`
	StdoutTail  string                  `json:"stdout_tail,omitempty"`
	StderrTail  string                  `json:"stderr_tail,omitempty"`
	StdoutDump  string                  `json:"stdout_dump,omitempty"`
	StderrDump  string                  `json:"stderr_dump,omitempty"`
	StdoutTotal int64                   `json:"stdout_total,omitempty"`
	StderrTotal int64                   `json:"stderr_total,omitempty"`
	Diagnostic  string                  `json:"diagnostic,omitempty"`
}
