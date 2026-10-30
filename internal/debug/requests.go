package debug

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"magi/internal/core/protocol"
)

func fullCapture() bool { return os.Getenv("MAGI_DEBUG_API_MODE") == "full" }

type requestMetadata struct {
	protocol.DiagnosticIdentity
	protocol.RequestDiagnostic
	Model            string                 `json:"model"`
	Thinking         string                 `json:"thinking"`
	ReasoningEffort  string                 `json:"reasoning_effort,omitempty"`
	MaxTokens        int                    `json:"max_tokens"`
	Status           string                 `json:"status"`
	Error            string                 `json:"error_category,omitempty"`
	TransportFailure string                 `json:"transport_failure_reason,omitempty"`
	Usage            *protocol.Usage        `json:"usage,omitempty"`
	TokenDetails     *protocol.TokenDetails `json:"token_details,omitempty"`
	Finish           string                 `json:"finish_category,omitempty"`
}

func saveMetadata(dir string, meta *requestMetadata) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return errCapture
	}
	return writeProbeFile(dir, meta.CallID+".json", raw)
}

func modelMetadata(ctx context.Context, diagnostic *protocol.RequestDiagnostic) requestMetadata {
	meta := requestMetadata{DiagnosticIdentity: protocol.DiagnosticIdentityFromContext(ctx), RequestDiagnostic: *diagnostic}
	var settings struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		MaxTokens       int    `json:"max_tokens"`
		Thinking        struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	_ = json.Unmarshal(diagnostic.RequestBody, &settings)
	meta.Model, meta.Thinking = settings.Model, settings.Thinking.Type
	meta.ReasoningEffort, meta.MaxTokens = settings.ReasoningEffort, settings.MaxTokens
	return meta
}

// BeforeModelRequest saves the frozen provider body. Only explicit pause control
// may return an error; ordinary diagnostic failures do not stop the request.
func (Observer) BeforeModelRequest(ctx context.Context, callID string, body []byte) error {
	if !fullCapture() || os.Getenv("MAGI_DEBUG_API_DIR") == "" {
		return nil
	}
	pause := os.Getenv("MAGI_DEBUG_API_PAUSE")
	if pause != "" && pause != "before" {
		return errors.New("MAGI_DEBUG_API_PAUSE must be empty or before")
	}
	dir, err := sessionDirectory(ctx)
	if err != nil {
		if pause != "" {
			return err
		}
		warnRecordOmitted()
		return nil
	}
	meta := modelMetadata(ctx, &protocol.RequestDiagnostic{CallID: callID, Started: time.Now().UTC(), RequestBody: body})
	meta.Status = "requesting"
	if pause != "" {
		meta.Status = "waiting_before"
	}
	err = writeCapture(dir, "requests", callID+".json", body)
	if err == nil {
		err = saveMetadata(dir, &meta)
	}
	if err != nil {
		if pause != "" {
			return errCapture
		}
		warnRecordOmitted()
		return nil
	}
	if pause != "" {
		if err := waitRelease(ctx, filepath.Join(dir, callID+".continue")); err != nil {
			return err
		}
		meta.Status, meta.Started = "requesting", time.Now().UTC()
		if err := saveMetadata(dir, &meta); err != nil {
			warnRecordOmitted()
		}
	}
	return ctx.Err()
}

func (Observer) RecordModelRequest(ctx context.Context, diagnostic *protocol.RequestDiagnostic, result *protocol.Response, callErr error) {
	failed := callErr != nil || result.FinishReason == "length"
	if !fullCapture() && !failed {
		return
	}
	dir, err := sessionDirectory(ctx)
	if err != nil {
		warnRecordOmitted()
		return
	}
	if dir == "" {
		return
	}
	// The model lends the frozen wire body at completion, so lightweight capture
	// needs neither successful-request files nor a map of in-flight requests.
	if failed {
		if err := writeCapture(dir, "requests", diagnostic.CallID+".json", diagnostic.RequestBody); err != nil {
			warnRecordOmitted()
		}
	}
	meta := modelMetadata(ctx, diagnostic)
	meta.TransportFailure = transportCategory(diagnostic.TransportError)
	meta.Status, meta.Error = "completed", errorCategory(callErr)
	if failed {
		meta.Status = "failed"
	}
	// Partial streams and HTTP error bodies are not usable parsed responses.
	if result.FinishReason != "" && protocol.ValidateResponse(*result) == nil {
		raw, err := json.Marshal(result)
		if err == nil {
			err = writeCapture(dir, "responses", diagnostic.CallID+".json", raw)
		}
		if err != nil {
			warnRecordOmitted()
		}
		usage := result.Usage
		meta.Usage, meta.TokenDetails, meta.Finish = &usage, result.TokenDetails, result.FinishReason
	}
	if err := saveMetadata(dir, &meta); err != nil {
		warnRecordOmitted()
	}
}

func waitRelease(ctx context.Context, path string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return errCapture
			}
			if os.Remove(path) != nil {
				return errCapture
			}
			return ctx.Err()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errCapture
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func errorCategory(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, errCapture):
		return "probe"
	case errors.Is(err, protocol.ErrContextOverflow):
		return "context_overflow"
	case errors.Is(err, protocol.ErrModelOutput):
		return "model_output"
	case errors.Is(err, protocol.ErrModelUnavailable):
		return "model_unavailable"
	default:
		return "other"
	}
}

// Transport causes are classified here, never copied as error text.
func transportCategory(err error) string {
	if err == nil {
		return ""
	}
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var network net.Error
	var operation *net.OpError
	switch {
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &certificate), errors.As(err, &record):
		return "tls"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.As(err, &operation):
		switch operation.Op {
		case "dial", "read", "write", "proxyconnect":
			return operation.Op
		default:
			return "other"
		}
	default:
		return "other"
	}
}
