package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"magi/internal/core/protocol"
	"magi/internal/storage"
)

type bashArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
	Timeout *int64 `json:"timeout,omitempty"`
}

const defaultBashTimeout = 120 * time.Second

type bashProvider struct{ program string }

func (p bashProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	args, err := decodeBashArgs(request.Arguments)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	cwd := request.ProjectRoot
	if args.Workdir != "" {
		// Keep symlink/.. semantics until bashWorkdir resolves the physical cwd.
		cwd = request.ProjectRoot + string(filepath.Separator) + args.Workdir
	}
	cwd, err = bashWorkdir(request.ProjectRoot, cwd)
	if err != nil {
		var feedback protocol.CandidateFeedback
		if errors.As(err, &feedback) {
			return protocol.ToolResult{}, err
		}
		return bashOutcome(processResult{ExitCode: -1}, request.inlineBytes, err)
	}
	timeout := defaultBashTimeout
	if args.Timeout != nil {
		timeout = time.Duration(*args.Timeout) * time.Second
	}
	// A command deadline is local to this call; it must not cancel the Worker.
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := runBash(commandCtx, p.program, args.Command, cwd, request)
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("bash timed out after %s: %w", timeout, err)
	}
	return bashOutcome(raw, request.inlineBytes, err)
}

func decodeBashArgs(arguments json.RawMessage) (bashArgs, error) {
	var args bashArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return bashArgs{}, protocol.InvalidCandidate("bash requires command and optional workdir strings")
	}
	if strings.ContainsRune(args.Command, '\x00') || strings.ContainsRune(args.Workdir, '\x00') {
		return bashArgs{}, protocol.InvalidCandidate("command and workdir must not contain NUL")
	}
	if filepath.IsAbs(args.Workdir) {
		return bashArgs{}, protocol.InvalidCandidate("workdir must be relative to ProjectRoot, not an absolute path; omit it or use . for the project root")
	}
	if args.Timeout != nil && (*args.Timeout <= 0 || *args.Timeout > int64((1<<63-1)/time.Second)) {
		return bashArgs{}, protocol.InvalidCandidate("timeout must be a positive integer number of seconds within the supported duration range")
	}
	return args, nil
}

func bashWorkdir(root, cwd string) (string, error) {
	// Preserve the declared-path boundary as well as checking its physical target.
	for _, physical := range []bool{false, true} {
		if physical {
			var err error
			cwd, err = filepath.EvalSymlinks(cwd)
			if err != nil {
				return "", err
			}
		}
		rel, err := filepath.Rel(root, cwd)
		if err != nil {
			return "", err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", protocol.InvalidCandidate("workdir escapes ProjectRoot")
		}
		current := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == ".magi" || part == ".git" {
				return "", protocol.InvalidCandidate("workdir enters managed metadata")
			}
			current = filepath.Join(current, part)
			if strings.HasPrefix(part, ".magi-init-") {
				private, err := storage.IsPrivateInitializationDir(current)
				if err != nil {
					return "", err
				}
				if private {
					return "", protocol.InvalidCandidate("workdir enters private project initialization")
				}
			}
		}
	}
	return cwd, nil
}
