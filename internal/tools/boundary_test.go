package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"magi/internal/core/protocol"
)

func TestManagedToolsRejectPrivateInitializationStage(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".magi-init-crashed")
	noError(t, os.Mkdir(stage, 0700))
	noError(t, writeTestInitializationMarker(stage))
	bashArgs := json.RawMessage(`{"command":"true","workdir":".magi-init-crashed"}`)
	if _, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), toolExecution{Arguments: bashArgs, ProjectRoot: root}); err == nil {
		t.Fatal("exec provider entered private initialization cwd")
	}
	if _, err := (writeProvider{}).Execute(context.Background(), execution(root, writeArgs{Path: ".magi-init-crashed/new.txt", Content: "x"})); err == nil {
		t.Fatal("write provider allowed private initialization target")
	}
	ordinary := filepath.Join(root, ".magi-init-user")
	noError(t, os.Mkdir(ordinary, 0700))
	if _, err := (writeProvider{}).Execute(context.Background(), execution(root, writeArgs{Path: ".magi-init-user/notes.txt", Content: "x"})); err != nil {
		t.Fatalf("ordinary same-prefix directory was excluded: %v", err)
	}
}

func TestMutationsRejectProtectedTargets(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	noError(t, os.WriteFile(filepath.Join(outside, "file"), []byte("old"), 0600))
	noError(t, os.Symlink(outside, filepath.Join(root, "linked")))
	private := filepath.Join(root, "nested", ".magi", "file")
	noError(t, os.MkdirAll(filepath.Dir(private), 0700))
	noError(t, os.WriteFile(private, []byte("old"), 0600))
	for _, target := range []string{"../file", outside + "/file", ".", ".magi/file", ".git/file", "nested/.magi/file", "nested/.git/file", "linked/file"} {
		for _, provider := range []toolProvider{writeProvider{}, editProvider{}} {
			args := map[string]any{"path": target, "content": "new", "old_string": "old", "new_string": "new"}
			if _, err := provider.Execute(context.Background(), execution(root, args)); err == nil {
				t.Fatalf("%T allowed protected target: %s", provider, target)
			}
		}
	}
	for _, path := range []string{private, filepath.Join(outside, "file")} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != "old" {
			t.Fatalf("protected file changed: body=%q err=%v", body, err)
		}
	}
}

func TestProcessPreservesShellSignalStatus(t *testing.T) {
	script := "kill -TERM $$"
	result, err := runBash(processContext(t, time.Second), defaultBashPath, script, t.TempDir(), toolExecution{MaxOutputBytes: 100})
	if err == nil || result.ExitCode != 143 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("expected process exit error, got %T", err)
	}
}

func TestExecPhysicalWorkingDirectory(t *testing.T) {
	for _, mode := range []string{"root", "inside", "outside", "private", "git", "symlink-parent-cwd", "metadata-to-inside", "outside-to-inside"} {
		t.Run(mode, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			for _, dir := range []string{"inside", ".magi", ".git"} {
				noError(t, os.Mkdir(filepath.Join(root, dir), 0700))
			}
			target := filepath.Join(root, "inside")
			switch mode {
			case "outside":
				target = outside
			case "private":
				target = filepath.Join(root, ".magi")
			case "git":
				target = filepath.Join(root, ".git")
			case "root":
				target = root
			case "symlink-parent-cwd":
				target = filepath.Join(root, "nested")
				noError(t, os.MkdirAll(filepath.Join(target, "child"), 0700))
			}
			alias := filepath.Join(root, "alias")
			aliasTarget := target
			if mode == "symlink-parent-cwd" {
				aliasTarget = filepath.Join(target, "child")
			}
			noError(t, os.Symlink(aliasTarget, alias))
			request := execution(root, map[string]any{"command": "printf selected > marker", "workdir": "alias"})
			if mode == "root" {
				request = execution(root, map[string]any{"command": "printf selected > marker"})
			}
			if mode == "symlink-parent-cwd" {
				request = execution(root, map[string]any{"command": "printf selected > marker", "workdir": "alias/.."})
			}
			if mode == "metadata-to-inside" {
				noError(t, os.Remove(filepath.Join(root, ".git")))
				noError(t, os.Symlink(target, filepath.Join(root, ".git")))
				request = execution(root, map[string]any{"command": "printf selected > marker", "workdir": ".git"})
			}
			if mode == "outside-to-inside" {
				back := filepath.Join(outside, "back")
				noError(t, os.Symlink(target, back))
				relative, err := filepath.Rel(root, back)
				noError(t, err)
				request = execution(root, map[string]any{"command": "printf selected > marker", "workdir": relative})
			}
			_, err := (bashProvider{program: defaultBashPath}).Execute(context.Background(), request)
			body, readErr := os.ReadFile(filepath.Join(target, "marker"))
			allowed := mode == "root" || mode == "inside" || mode == "symlink-parent-cwd"
			if allowed {
				if err != nil || string(body) != "selected" {
					t.Fatalf("exec=%v body=%q", err, body)
				}
			} else if err == nil || !os.IsNotExist(readErr) {
				t.Fatalf("illegal physical cwd executed: %v %q %v", err, body, readErr)
			}
		})
	}
}

func TestParentWorkspaceReadSkipsChildPrivateInitialization(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(child, ".magi-init-live")
	noError(t, os.Mkdir(stage, 0700))
	noError(t, writeTestInitializationMarker(stage))
	noError(t, os.WriteFile(filepath.Join(stage, "state.db"), []byte("private project state"), 0600))
	userDir := filepath.Join(child, ".magi-init-user")
	if err := os.Mkdir(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "notes.txt"), []byte("ordinary user content"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspace(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (readProvider{workspace}).Execute(ctx, toolExecution{Arguments: json.RawMessage(`{"path":"child/.magi-init-live/state.db"}`)}); err == nil {
		t.Fatal("child private initialization entered parent reads")
	}
	result, err := (readProvider{workspace}).Execute(ctx, toolExecution{Arguments: json.RawMessage(`{"path":"child/.magi-init-user/notes.txt"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if receiptFields(t, result.Body)["content"] != "1: ordinary user content\n" {
		t.Fatalf("ordinary same-prefix user file was excluded: %s", result.Body)
	}

	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("parent read changed the child's initialization: %v", err)
	}
}

func TestToolArgumentFeedbackDoesNotMaskIsolationOrIOFailures(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name     string
		provider toolProvider
		args     string
		root     string
		repair   bool
		hint     string
	}{
		{"edit-empty", editProvider{}, `{"path":"file","old_string":"","new_string":"new"}`, root, true, "old_string must be nonempty"},
		{"exec-isolation", bashProvider{program: defaultBashPath}, `{"command":"true","workdir":"../private"}`, root, true, "escapes ProjectRoot"},
		{"write-isolation", writeProvider{}, `{"path":".magi/private","content":"x"}`, root, true, "path cannot traverse a parent or enter .magi/.git"},
		{"filesystem", bashProvider{program: defaultBashPath}, `{"command":"true"}`, root + "/missing", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(tc.args), ProjectRoot: tc.root})
			if err == nil {
				t.Fatal("invalid tool arguments accepted")
			}
			var feedback protocol.CandidateFeedback
			marked := errors.As(err, &feedback)
			if marked != tc.repair {
				t.Fatalf("repair marker=%v want %v; error=%v", marked, tc.repair, err)
			}
			if marked && !strings.Contains(feedback.PublicMessage, tc.hint) {
				t.Fatal("missing useful correction")
			}
		})
	}
}
