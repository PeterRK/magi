package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

func testGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(controlledEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitTool(t *testing.T, runtime protocol.ToolExecutor, name string, args any) gitReadResult {
	t.Helper()
	body, err := json.Marshal(args)
	noError(t, err)
	result, err := runtime.Execute(context.Background(), protocol.ToolRequest{ID: "git", Name: name, Arguments: body})
	noError(t, err)
	var value gitReadResult
	noError(t, json.Unmarshal(result.Body, &value))
	if result.Status != protocol.ToolSucceeded {
		t.Fatalf("query failed: %s", result.Body)
	}
	return value
}

func writeGitFile(t *testing.T, root, path, text string) {
	t.Helper()
	noError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700))
	noError(t, os.WriteFile(filepath.Join(root, path), []byte(text), 0600))
}

func TestGitQueriesRecoverAfterWorkerScratchCleanup(t *testing.T) {
	for _, name := range []string{"git_log", "git_diff", "git_status"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testGit(t, root, "init", "-b", "main")
			writeGitFile(t, root, "a.txt", "delivered\n")
			testGit(t, root, "add", "a.txt")
			testGit(t, root, "commit", "-m", "delivery")
			runtime, err := NewRuntime(root, ExecutablePaths{})
			noError(t, err)
			noError(t, runtime.PrepareRound(context.Background()))
			_, err = runtime.Execute(context.Background(), protocol.ToolRequest{
				ID: "cleanup", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf .magi-temp"}`),
			})
			noError(t, err)
			var args any = struct{}{}
			if name == "git_diff" {
				args = gitDiffArgs{Ref: "HEAD"}
			}
			result := gitTool(t, runtime, name, args)
			if result.Content == "" || result.Diagnostic != "" {
				t.Fatalf("query lost output after scratch cleanup: %+v", result)
			}
			ignore, err := os.ReadFile(filepath.Join(root, tempDirectory, ".gitignore"))
			noError(t, err)
			if string(ignore) != "*\n" {
				t.Fatalf("restored scratch directory is not ignored: %q", ignore)
			}
		})
	}
}

func TestGitHistoryDiffAndStatus(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-b", "main")
	writeGitFile(t, root, "a.txt", "one\n")
	for _, path := range []string{".magi/secret", "nested/.magi/secret", ".magi-temp/output", ".magi-init-private/secret"} {
		writeGitFile(t, root, path, "private-secret\n")
	}
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-m", "root")
	base := testGit(t, root, "rev-parse", "HEAD")
	writeGitFile(t, root, "a.txt", "two\n")
	writeGitFile(t, root, "file[1].txt", "literal-path\n")
	testGit(t, root, "add", "--all")
	testGit(t, root, "commit", "-m", "change")
	change := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "checkout", "-b", "side")
	writeGitFile(t, root, "a.txt", "three\n")
	testGit(t, root, "commit", "-am", "side-change")
	side := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "checkout", "main")
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))

	log := gitTool(t, runtime, "git_log", gitLogArgs{})
	if strings.Count(log.Content, "root") != 1 || !strings.Contains(log.Content, side[:12]) || !strings.Contains(log.Content, "side-change") {
		t.Fatal("default log missed branches or duplicated ancestry", log)
	}
	log = gitTool(t, runtime, "git_log", gitLogArgs{Ref: "main"})
	if strings.Contains(log.Content, "side-change") || log.Ref != change[:12] {
		t.Fatal("branch-scoped log escaped its history", log)
	}
	if !gitTool(t, runtime, "git_log", gitLogArgs{Limit: 1}).Truncated {
		t.Fatal("omitted log entries were not marked")
	}
	// Changing branches does not change the commit selected by a short SHA.
	testGit(t, root, "checkout", "--detach", side)
	diff := gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: change[:12], Paths: []string{"a.txt"}})
	if !strings.Contains(diff.Content, "+two") || strings.Contains(diff.Content, "+three") || diff.Ref != change[:12] {
		t.Fatal("diff followed HEAD instead of the specified commit", diff)
	}
	diff = gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: side, Base: base})
	if !strings.Contains(diff.Content, "+three") || strings.Contains(diff.Content, "+two") || diff.Base != base[:12] {
		t.Fatal("range diff was not the net change", diff)
	}
	diff = gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: change, Paths: []string{"file[1].txt"}})
	if !strings.Contains(diff.Content, "literal-path") || strings.Contains(diff.Content, "a.txt") {
		t.Fatal("Git path was treated as a glob", diff)
	}
	diff = gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: base})
	if !strings.Contains(diff.Content, "+one") || strings.Contains(diff.Content, "private-secret") || strings.Contains(diff.Content, ".magi") {
		t.Fatal("root diff lost source or disclosed protected paths", diff)
	}

	testGit(t, root, "checkout", "main")
	writeGitFile(t, root, "a.txt", "staged\n")
	testGit(t, root, "add", "a.txt")
	writeGitFile(t, root, "a.txt", "unstaged\n")
	writeGitFile(t, root, "new.txt", "untracked\n")
	writeGitFile(t, root, ".magi/untracked-secret", "private\n")
	writeGitFile(t, root, ".magi-temp/untracked-output", "private\n")
	before, err := os.ReadFile(filepath.Join(root, ".git/index"))
	noError(t, err)
	status := gitTool(t, runtime, "git_status", struct{}{})
	if !strings.Contains(status.Content, "## main") || !strings.Contains(status.Content, "MM a.txt") || !strings.Contains(status.Content, "?? new.txt") || strings.Contains(status.Content, ".magi") {
		t.Fatal("status missed uncommitted work or exposed metadata", status)
	}
	after, err := os.ReadFile(filepath.Join(root, ".git/index"))
	noError(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only query rewrote the index")
	}
	for _, args := range []gitDiffArgs{{Ref: "--output=bad"}, {Ref: "HEAD:secret"}, {Ref: "HEAD", Paths: []string{"../outside"}}, {Ref: "HEAD", Paths: []string{"nested/.git/config"}}} {
		body, _ := json.Marshal(args)
		if _, err := runtime.Execute(context.Background(), protocol.ToolRequest{ID: "bad", Name: "git_diff", Arguments: body}); err == nil {
			t.Fatal("accepted invalid Git selector", args)
		}
	}
}

func TestGitOversizedLineRetainsDump(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", inlineOutputBytes+100), strings.Repeat("界", 20000)} {
		root := t.TempDir()
		testGit(t, root, "init", "-b", "main")
		testGit(t, root, "commit", "--allow-empty", "-m", text+"END-MARKER")
		runtime, err := NewRuntime(root, ExecutablePaths{})
		noError(t, err)
		noError(t, runtime.PrepareRound(context.Background()))
		log := gitTool(t, runtime, "git_log", gitLogArgs{Limit: 1})
		if log.Content == "" || len(log.Content) > inlineOutputBytes || !utf8.ValidString(log.Content) || !log.Truncated || log.Dump == "" || log.Next != 0 {
			t.Fatal("oversized line was lost, unmarked or left at the same offset", log)
		}
		full, err := os.ReadFile(filepath.Join(root, log.Dump))
		noError(t, err)
		if !bytes.Contains(full, []byte(text+"END-MARKER")) {
			t.Fatal("dump lost the clipped line's remainder")
		}
		page, err := readTextWindow(context.Background(), strings.NewReader(text+"\nnext\n"), readArgs{Offset: 1, Limit: 1}, false)
		noError(t, err)
		if page.Content == "" || !page.LinesClipped || page.Next != 2 {
			t.Fatal("clipped line did not advance to the next line", page)
		}
	}
}

func TestGitQueriesDisableProgramsAndBoundOutput(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-b", "main")
	writeGitFile(t, root, "large.txt", strings.Repeat("original\n", 15000))
	writeGitFile(t, root, "wide.txt", strings.Repeat(strings.Repeat("界", 20)+" original\r\n", 1000))
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-m", "large root")
	writeGitFile(t, root, ".gitattributes", "*.txt filter=trap diff=trap\n")
	writeGitFile(t, root, "large.txt", "changed\n")
	for _, key := range []string{"core.fsmonitor", "diff.external", "diff.trap.textconv", "filter.trap.clean", "filter.trap.process"} {
		testGit(t, root, "config", key, "touch program-ran; cat")
	}
	testGit(t, root, "config", "filter.trap.required", "true")
	runtime, err := NewRuntime(root, ExecutablePaths{})
	noError(t, err)
	t.Setenv("PATH", t.TempDir())
	noError(t, runtime.PrepareRound(context.Background()))
	gitTool(t, runtime, "git_status", struct{}{})
	diff := gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: "HEAD"})
	if diff.Dump == "" || diff.Truncated || diff.Next != inlineOutputLines+1 || strings.Count(diff.Content, "\n") != inlineOutputLines || len(diff.Content) > inlineOutputBytes || !strings.Contains(diff.Content, "original") {
		t.Fatal("large Git output was not bounded", len(diff.Content), diff.Truncated)
	}
	full, err := os.ReadFile(filepath.Join(root, diff.Dump))
	noError(t, err)
	if !strings.HasPrefix(string(full), diff.Content) || strings.Count(string(full), "\n+original") != 15000 {
		t.Fatal("dump did not retain the complete patch")
	}
	body, err := json.Marshal(readArgs{Path: diff.Dump, Offset: diff.Next, Limit: 2})
	noError(t, err)
	receipt, err := runtime.Execute(context.Background(), protocol.ToolRequest{ID: "read-git", Name: "read", Arguments: body})
	noError(t, err)
	read := string(receipt.Body)
	if !strings.Contains(read, "2001: +original") {
		t.Fatal("saved Git output cannot be read")
	}
	noError(t, runtime.PrepareRound(context.Background()))
	if _, err := os.Stat(filepath.Join(root, diff.Dump)); !os.IsNotExist(err) {
		t.Fatal("Git output survived round cleanup", err)
	}
	wide := gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: "HEAD", Paths: []string{"wide.txt"}})
	if wide.Dump == "" || wide.Next != strings.Count(wide.Content, "\n")+1 || len(wide.Content) <= 32<<10 || len(wide.Content) > inlineOutputBytes || !strings.HasSuffix(wide.Content, "\r\n") {
		t.Fatal("byte-bounded Git output lost complete source lines", len(wide.Content), wide.Next)
	}
	full, err = os.ReadFile(filepath.Join(root, wide.Dump))
	noError(t, err)
	if !strings.HasPrefix(string(full), wide.Content) {
		t.Fatal("Git preview changed source text")
	}
	noError(t, runtime.PrepareRound(context.Background()))
	bounded := execution(root, gitDiffArgs{Ref: "HEAD"})
	bounded.MaxOutputBytes, bounded.inlineBytes, bounded.callID = 64<<10, inlineOutputBytes, "bounded-git"
	limited, err := (gitProvider{name: "git_diff", program: defaultGitPath}).Execute(context.Background(), bounded)
	noError(t, err)
	diff = decodeReceipt[gitReadResult](t, limited.Body)
	info, err := os.Stat(filepath.Join(root, diff.Dump))
	noError(t, err)
	if !diff.Truncated || info.Size() != 64<<10 || !strings.Contains(diff.Diagnostic, "retention incomplete") {
		t.Fatal("incomplete dump was not disclosed", diff.Diagnostic)
	}
	noError(t, os.RemoveAll(filepath.Join(root, tempDirectory)))
	// A missing directory is recoverable; a non-directory remains a real
	// capture failure and must not be replaced or hide lost output.
	noError(t, os.WriteFile(filepath.Join(root, tempDirectory), []byte("not a directory"), 0600))
	failed, failure := runtime.Execute(context.Background(), protocol.ToolRequest{ID: "missing-output", Name: "git_diff", Arguments: json.RawMessage(`{"ref":"HEAD"}`)})
	if failure == nil || failed.Status != protocol.ToolFailed || !strings.Contains(string(failed.Body), "unavailable") {
		t.Fatal("dump failure was not reported", string(failed.Body), failure)
	}
	if _, err := os.Stat(filepath.Join(root, "program-ran")); !os.IsNotExist(err) {
		t.Fatal("query executed an external program", err)
	}
	// A project without .git must not query its parent's repository.
	child := filepath.Join(root, "child")
	noError(t, os.Mkdir(child, 0700))
	childRuntime, err := NewRuntime(child, ExecutablePaths{})
	noError(t, err)
	result, err := childRuntime.Execute(context.Background(), protocol.ToolRequest{ID: "outside", Name: "git_log", Arguments: json.RawMessage(`{}`)})
	if err == nil || result.Status != protocol.ToolFailed {
		t.Fatal("Git discovered a repository outside ProjectRoot")
	}
	// The supplied ctx must interrupt an external query, including process startup.
	bin := t.TempDir()
	noError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700))
	runtime, err = NewRuntime(root, ExecutablePaths{Git: filepath.Join(bin, "git")})
	noError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	before := time.Now()
	result, err = runtime.Execute(ctx, protocol.ToolRequest{ID: "cancel", Name: "git_status", Arguments: json.RawMessage(`{}`)})
	if err == nil || result.Status != protocol.ToolTimedOut || time.Since(before) > 2*time.Second {
		t.Fatal("Git query ignored deadline", err, result.Status)
	}
}

func TestGitQueriesTrustBoundWorkspace(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "-b", "main")
	writeGitFile(t, root, "a.txt", "content\n")
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "-m", "root")
	writeGitFile(t, root, "new.txt", "untracked\n")

	git, err := exec.LookPath("git")
	noError(t, err)
	git, err = filepath.Abs(git)
	noError(t, err)
	program := filepath.Join(t.TempDir(), "git")
	// Git's ownership-test switch reproduces a bind mount owned by another user
	// without requiring chown or elevated privileges.
	script := "#!/bin/sh\nGIT_TEST_ASSUME_DIFFERENT_OWNER=1 exec '" + strings.ReplaceAll(git, "'", "'\\''") + "' \"$@\"\n"
	noError(t, os.WriteFile(program, []byte(script), 0700))
	cmd := exec.Command(program, "rev-parse", "--show-toplevel")
	cmd.Dir = root
	cmd.Env = append(controlledEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "dubious ownership") {
		t.Fatalf("fixture did not reproduce Git ownership rejection: %v: %s", err, out)
	}

	runtime, err := NewRuntime(root, ExecutablePaths{Git: program})
	noError(t, err)
	noError(t, runtime.PrepareRound(context.Background()))
	if !strings.Contains(gitTool(t, runtime, "git_log", gitLogArgs{}).Content, "root") {
		t.Fatal("missing commit")
	}
	if !strings.Contains(gitTool(t, runtime, "git_diff", gitDiffArgs{Ref: "HEAD"}).Content, "+content") {
		t.Fatal("missing committed change")
	}
	if !strings.Contains(gitTool(t, runtime, "git_status", struct{}{}).Content, "?? new.txt") {
		t.Fatal("missing untracked file")
	}
}
