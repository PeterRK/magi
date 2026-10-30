package debug

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"magi/internal/core/protocol"
)

// Observer is independently bound by CLI. Each notification owns its file write.
type Observer struct{}

func warnRecordOmitted() {
	fmt.Fprintln(os.Stderr, "magi: debug observation failed; record omitted")
}

// DumpContext captures the actual Context, not a reconstructed API request.
func (Observer) DumpContext(ctx context.Context, event string, snapshot []byte) {
	dir, err := sessionDirectory(ctx)
	if err != nil {
		warnRecordOmitted()
		return
	}
	if dir == "" {
		return
	}

	raw, err := json.Marshal(struct {
		protocol.DiagnosticIdentity
		Event    string          `json:"event"`
		Captured time.Time       `json:"captured_at"`
		Snapshot json.RawMessage `json:"snapshot"`
	}{protocol.DiagnosticIdentityFromContext(ctx), event, time.Now().UTC(), snapshot})
	if err == nil {
		err = writeCapture(dir, "contexts", rand.Text()+".json", raw)
	}
	if err != nil {
		warnRecordOmitted()
	}
}

// NotifyReplyRequired publishes a new reply request before displaying the question.
func (Observer) NotifyReplyRequired() {
	dir, err := rootDirectory()
	if err != nil {
		warnRecordOmitted()
		return
	}
	if dir == "" {
		return
	}
	raw, _ := json.Marshal(struct {
		ID string `json:"id"`
	}{rand.Text()})
	if err := writeProbeFile(dir, "reply-request.json", raw); err != nil {
		warnRecordOmitted()
	}
}

// RecordToolExecution saves metadata only, without a model probe or pause lifecycle.
// Each call owns a file, so parallel tools require no shared writer or lock.
func (Observer) RecordToolExecution(ctx context.Context, data *protocol.ToolDiagnostic) {
	dir, err := sessionDirectory(ctx)
	if err != nil {
		warnRecordOmitted()
		return
	}
	if dir == "" {
		return
	}
	raw, err := json.MarshalIndent(struct {
		protocol.DiagnosticIdentity
		protocol.ToolDiagnostic
	}{protocol.DiagnosticIdentityFromContext(ctx), *data}, "", "  ")
	if err == nil {
		err = writeCapture(dir, "tools", rand.Text()+".json", raw)
	}
	if err != nil {
		warnRecordOmitted()
	}
}
