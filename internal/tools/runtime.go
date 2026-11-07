package tools

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"magi/internal/core/protocol"
)

type toolExecution struct {
	Arguments      json.RawMessage
	ProjectRoot    string // Physical absolute workspace root, resolved by NewRuntime.
	MaxOutputBytes int
	inlineBytes    int
	callID         string
}

const (
	defaultBashPath       = "/bin/bash"
	defaultGitPath        = "/usr/bin/git"
	inlineOutputBytes     = 50 << 10
	inlineOutputLines     = 2000
	defaultMaxOutputBytes = 16 << 20
)

// ExecutablePaths are startup configuration, copied into the workspace runtime.
// Empty fields select the fixed system defaults; PATH is never searched.
type ExecutablePaths struct {
	Bash string
	Git  string
}

type runtime struct {
	workspace *workspace
	programs  ExecutablePaths
}

// NewRuntime creates the fixed built-in tool set for one workspace.
func NewRuntime(root string, programs ExecutablePaths) (protocol.ToolExecutor, error) {
	if programs.Bash == "" {
		programs.Bash = defaultBashPath
	}
	if programs.Git == "" {
		programs.Git = defaultGitPath
	}
	for _, path := range []string{programs.Bash, programs.Git} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
			return nil, fmt.Errorf("tool executable must be an absolute path: %q", path)
		}
	}
	backend, err := newWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &runtime{workspace: backend, programs: programs}, nil
}

// Execute dispatches schema-validated arguments; each provider decodes them and
// checks its execution constraints independently of the calling role.
func (r *runtime) Execute(ctx context.Context, request protocol.ToolRequest) (protocol.ToolResult, error) {
	if request.ID == "" {
		return protocol.ToolResult{}, protocol.InvalidCommand("tool call ID is required")
	}
	var execute func(context.Context, toolExecution) (protocol.ToolResult, error)
	switch request.Name {
	case "read":
		execute = (readProvider{r.workspace}).Execute
	case "glob":
		execute = (globProvider{r.workspace}).Execute
	case "grep":
		execute = (grepProvider{r.workspace}).Execute
	case "git_log", "git_diff", "git_status":
		execute = (gitProvider{name: request.Name, program: r.programs.Git}).Execute
	case "bash":
		execute = (bashProvider{program: r.programs.Bash}).Execute
	case "write":
		execute = (writeProvider{}).Execute
	case "edit":
		execute = (editProvider{}).Execute
	default:
		return protocol.ToolResult{}, protocol.InvalidCommand("unknown tool")
	}

	execution := toolExecution{Arguments: request.Arguments, ProjectRoot: r.workspace.root, MaxOutputBytes: defaultMaxOutputBytes, inlineBytes: inlineOutputBytes, callID: request.ID}
	if err := ctx.Err(); err != nil {
		return toolFailure(err)
	}
	observer := protocol.Observer
	if observer == nil {
		return execute(ctx, execution)
	}
	started := time.Now()
	result, err := execute(ctx, execution)
	ended := time.Now()
	status := result.Status
	if status == "" {
		status = executionStatus(err)
	}
	observer.RecordToolExecution(ctx, &protocol.ToolDiagnostic{
		CallID: request.ID, Tool: request.Name,
		Started: started.UTC(), Ended: ended.UTC(),
		DurationMS: float64(ended.Sub(started)) / float64(time.Millisecond), Status: status,
	})
	return result, err
}

func executionStatus(err error) protocol.ToolCallStatus {
	switch {
	case err == nil:
		return protocol.ToolSucceeded
	case errors.Is(err, context.Canceled):
		return protocol.ToolInterrupted
	case errors.Is(err, context.DeadlineExceeded):
		return protocol.ToolTimedOut
	default:
		return protocol.ToolFailed
	}
}

// encodeResult only encodes the fixed-field result structs defined in this package.
// Preserve each tool's own payload without adding a common envelope.
func encodeResult(value any, runErr error) (protocol.ToolResult, error) {
	body, _ := jsonv2.Marshal(value)
	return protocol.ToolResult{Status: executionStatus(runErr), Body: body}, runErr
}

func toolFailure(err error) (protocol.ToolResult, error) {
	return encodeResult(struct {
		Status     protocol.ToolCallStatus `json:"status"`
		Diagnostic string                  `json:"diagnostic"`
	}{executionStatus(err), err.Error()}, err)
}
