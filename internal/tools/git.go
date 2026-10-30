package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"magi/internal/core/protocol"
)

type gitLogArgs struct {
	Ref   string `json:"ref,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type gitDiffArgs struct {
	Ref   string   `json:"ref"`
	Base  string   `json:"base,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// Git's own output is already structured for reading; do not re-encode patch lines.
type gitReadResult struct {
	Ref        string `json:"ref,omitempty"`
	Base       string `json:"base,omitempty"`
	Content    string `json:"content"`
	Dump       string `json:"dump,omitempty"`
	Next       int    `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

type gitProvider struct{ name, program string }

func (p gitProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	var log gitLogArgs
	var diff gitDiffArgs
	var err error
	switch p.name {
	case "git_log":
		err = json.Unmarshal(request.Arguments, &log)
		if log.Limit == 0 {
			log.Limit = 10
		}
		if err != nil || log.Limit < 1 || log.Limit > 50 {
			return protocol.ToolResult{}, protocol.InvalidCandidate("git_log requires an optional ref and limit from 1 to 50 (0 defaults to 10)")
		}
		if log.Ref != "" {
			err = checkGitRef(log.Ref)
		}
	case "git_diff":
		err = json.Unmarshal(request.Arguments, &diff)
		if err == nil {
			err = checkGitRef(diff.Ref)
		}
		if err == nil && diff.Base != "" {
			err = checkGitRef(diff.Base)
		}
		for i, path := range diff.Paths {
			if err != nil {
				break
			}
			diff.Paths[i], err = projectPath(path)
		}
	case "git_status":
		err = json.Unmarshal(request.Arguments, &struct{}{})
	}
	if err != nil {
		return protocol.ToolResult{}, protocol.InvalidCandidate(err.Error())
	}

	// Do not discover a repository above the bound workspace.
	metadata, err := os.Lstat(filepath.Join(request.ProjectRoot, ".git"))
	if err != nil {
		return toolFailure(err)
	}
	if metadata.Mode()&os.ModeSymlink != 0 || (!metadata.IsDir() && !metadata.Mode().IsRegular()) {
		return toolFailure(errors.New("workspace .git must be a directory or Git worktree file, not a link"))
	}
	g := gitQuery{root: request.ProjectRoot, program: p.program}
	top, _, err := g.run(ctx, "rev-parse", "--show-toplevel")
	if err == nil {
		var physical string
		physical, err = filepath.EvalSymlinks(strings.TrimSpace(top))
		if err == nil && physical != request.ProjectRoot {
			err = errors.New("Git repository root differs from ProjectRoot")
		}
	}
	if err != nil {
		return toolFailure(err)
	}
	var result gitReadResult
	switch p.name {
	case "git_log":
		result, err = g.log(ctx, request, log)
	case "git_diff":
		result, err = g.diff(ctx, request, diff)
	case "git_status":
		err = g.disableFilters(ctx)
		if err == nil {
			err = g.output(ctx, request, &result, append([]string{"status", "--short", "--branch", "--untracked-files=all", "--ignore-submodules=dirty", "--"}, gitPaths(nil)...)...)
		}
	}
	if err != nil {
		return toolFailure(err)
	}
	return encodeResult(result, nil)
}

func checkGitRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\x00\r\n\t :") || strings.Contains(ref, "..") {
		return errors.New("ref must name one commit or branch, not options, a range or a file selector")
	}
	return nil
}

type gitQuery struct {
	root    string
	program string
	config  []string
}

// Fixed commands use no shell, user/global Git configuration or inherited GIT_* overrides.
// Optional locks and lazy fetch are disabled so queries remain local and read-only.
func (g gitQuery) run(ctx context.Context, args ...string) (string, bool, error) {
	out := gitBuffer{limit: inlineOutputBytes}
	err := g.runTo(ctx, &out, args...)
	return out.String(), out.truncated, err
}

// Public queries save output first, then use the same line window as read.
// Only output that cannot be fully inlined retains its temporary file.
func (g gitQuery) output(ctx context.Context, request toolExecution, result *gitReadResult, args ...string) error {
	capture := newOutputFile(request, "stdout")
	err := g.runTo(ctx, capture, args...)
	path, omitted, diagnostic := capture.finish()
	if err != nil {
		removeOutput(request.ProjectRoot, path)
		return err
	}
	if path == "" {
		return fmt.Errorf("git output unavailable: %s", diagnostic)
	}
	dir, err := outputDir(request.ProjectRoot)
	if err != nil {
		return err
	}
	file, _, err := openEntryAt(dir.Fd(), strings.TrimPrefix(path, outputDirectory))
	dir.Close()
	if err != nil {
		return err
	}
	defer file.Close()
	window, err := readTextWindow(ctx, file, readArgs{Path: path, Offset: 1, Limit: inlineOutputLines}, false)
	if err != nil {
		return err
	}
	result.Content = window.Content
	result.Next = window.Next
	result.Truncated = omitted > 0 || window.LinesClipped
	if result.Next == 0 && !result.Truncated {
		removeOutput(request.ProjectRoot, path)
	} else {
		result.Dump = path
	}
	if omitted > 0 {
		diagnostic = strings.TrimSpace(diagnostic + fmt.Sprintf("\noutput retention incomplete: retained %d of %d bytes", capture.saved, capture.total))
	}
	result.Diagnostic = diagnostic
	return nil
}

func (g gitQuery) runTo(ctx context.Context, stdout io.Writer, args ...string) error {
	fixed := []string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "color.ui=false", "-c", "protocol.allow=never"}
	// Bind mounts may have a different owner; global safe.directory settings are
	// deliberately ignored. Trust only this explicitly bound workspace.
	fixed = append(fixed, "-c", "safe.directory="+g.root)
	fixed = append(fixed, g.config...)
	cmd := exec.CommandContext(ctx, g.program, append(fixed, args...)...)
	cmd.Dir = g.root
	cmd.Env = append(controlledEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.WaitDelay = time.Second
	diagnostic := gitBuffer{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, &diagnostic
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if err != nil {
		err = fmt.Errorf("git query failed: %s: %w", strings.TrimSpace(diagnostic.String()), err)
	}
	return err
}

// Keep draining while retaining a bounded prefix, even for very large patches.
type gitBuffer struct {
	body      bytes.Buffer
	limit     int
	truncated bool
}

func (b *gitBuffer) Write(body []byte) (int, error) {
	n := min(len(body), max(0, b.limit-b.body.Len()))
	b.truncated = b.truncated || n < len(body)
	_, _ = b.body.Write(body[:n])
	return len(body), nil
}

func (b *gitBuffer) String() string { return b.body.String() }

func (g gitQuery) resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := g.run(ctx, "rev-parse", "--short=12", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(value), err
}

func (g gitQuery) log(ctx context.Context, request toolExecution, args gitLogArgs) (gitReadResult, error) {
	result := gitReadResult{}
	command := []string{"log", "--max-count=" + strconv.Itoa(args.Limit+1), "--date-order", "--abbrev=12", "--format=%h%d %s", "--decorate=short", "--no-color", "--no-show-signature", "--no-notes"}
	if args.Ref != "" {
		ref, err := g.resolve(ctx, args.Ref)
		if err != nil {
			return result, err
		}
		result.Ref = ref
		command = append(command, ref)
	} else {
		command = append(command, "--branches", "--remotes")
		// Include a detached HEAD; an unborn repository simply has no commits.
		if head, err := g.resolve(ctx, "HEAD"); err == nil {
			command = append(command, head)
		} else if ctx.Err() != nil {
			return result, ctx.Err()
		}
	}
	command = append(command, "--")
	err := g.output(ctx, request, &result, command...)
	lines := strings.Split(strings.TrimSuffix(result.Content, "\n"), "\n")
	if len(lines) > args.Limit {
		lines = lines[:args.Limit]
		result.Truncated = true
	}
	result.Content = strings.Join(lines, "\n")
	return result, err
}

func (g gitQuery) diff(ctx context.Context, request toolExecution, args gitDiffArgs) (gitReadResult, error) {
	result := gitReadResult{}
	var err error
	result.Ref, err = g.resolve(ctx, args.Ref)
	if err != nil {
		return result, err
	}
	command := []string{"show", "--format=", "--no-show-signature", "--no-notes", "--diff-merges=first-parent", "--root"}
	if args.Base != "" {
		result.Base, err = g.resolve(ctx, args.Base)
		if err != nil {
			return result, err
		}
		command = []string{"diff"}
	}
	command = append(command, "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--ignore-submodules=dirty", "--submodule=short", "--src-prefix=a/", "--dst-prefix=b/")
	if result.Base != "" {
		command = append(command, result.Base)
	}
	command = append(command, result.Ref, "--")
	command = append(command, gitPaths(args.Paths)...)
	err = g.output(ctx, request, &result, command...)
	return result, err
}

func gitPaths(paths []string) []string {
	result := make([]string, 0, len(paths)+9)
	// User paths are literal; only our fixed exclusions use pathspec magic.
	for _, path := range paths {
		result = append(result, ":(top,literal)"+path)
	}
	if len(result) == 0 {
		result = append(result, ":(top,glob)**")
	}
	for _, name := range []string{".magi", ".git", ".magi-temp", ".magi-init-*"} {
		result = append(result, ":(top,exclude,glob)**/"+name, ":(top,exclude,glob)**/"+name+"/**")
	}
	return result
}

// Status may hash working files through configured clean filters. Disable those
// commands as well as fsmonitor; suppressing optional locks alone is insufficient.
func (g *gitQuery) disableFilters(ctx context.Context) error {
	body, clipped, err := g.run(ctx, "config", "--includes", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil // No configured filters.
	}
	if err != nil {
		return err
	}
	if clipped {
		return errors.New("Git filter configuration exceeds query capacity")
	}
	for _, key := range strings.Split(body, "\x00") {
		if key == "" {
			continue
		}
		value := ""
		if strings.HasSuffix(key, ".required") {
			value = "false"
		}
		g.config = append(g.config, "-c", key+"="+value)
	}
	return nil
}
