package tools

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func TestCurrentWorkspaceReadsAndPrivateBoundaries(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".magi", ".git", "child/.magi", "node_modules/pkg", "ordinary", ".magi-init-user", ".magi-init-crashed", "child/.magi-init-crashed"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		".magi/state": "private", ".git/config": "git-private", "child/.magi/state": "child-private",
		".gitignore": "node_modules", "node_modules/pkg/file": "dependency", "ordinary/file": "initial",
		".magi-init-user/file": "public", ".magi-init-crashed/file": "private", "child/.magi-init-crashed/file": "private",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".magi-init-crashed", "child/.magi-init-crashed"} {
		stage := filepath.Join(root, name)
		if err := writeTestInitializationMarker(stage); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".magi", filepath.Join(root, "private-alias")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "external-link")); err != nil {
		t.Fatal(err)
	}
	backend, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{}
	_, err = backend.walk(context.Background(), ".", nil, func(name string, _ bool, _ *os.File) (bool, error) { files = append(files, name); return true, nil })
	sort.Strings(files)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", ".magi-init-user/file", "external-link", "node_modules/pkg/file", "ordinary/file", "private-alias"}
	if len(files) != len(want) {
		t.Fatalf("files=%+v", files)
	}
	for i, file := range files {
		if file != want[i] {
			t.Fatalf("files=%+v", files)
		}
	}
	for _, path := range []string{".magi/state", ".git/config", "child/.magi/state", "private-alias/state", "../secret", "/etc/passwd"} {
		if body, _, err := readWorkspaceFile(context.Background(), backend, path); err == nil {
			t.Fatalf("forbidden path %q returned %q", path, body)
		}
	}
	for _, path := range []string{".magi-init-crashed", ".magi-init-crashed/file", "child/.magi-init-crashed", "child/.magi-init-crashed/file"} {
		file, _, err := backend.open(context.Background(), path)
		if file != nil {
			file.Close()
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("private initialization path %q: %v", path, err)
		}
	}
	body, mode, err := readWorkspaceFile(context.Background(), backend, "external-link")
	if err != nil || mode&fs.ModeSymlink == 0 || string(body) != outside {
		t.Fatalf("link=%q mode=%v error=%v", body, mode, err)
	}
	if err := os.Link(filepath.Join(root, ".magi/state"), filepath.Join(root, "private-hardlink")); err != nil {
		t.Fatal(err)
	}
	if body, _, err := readWorkspaceFile(context.Background(), backend, "private-hardlink"); !errors.Is(err, fs.ErrPermission) || len(body) != 0 {
		t.Fatalf("private hardlink returned %q, error=%v", body, err)
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary/file"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	body, _, err = readWorkspaceFile(context.Background(), backend, "ordinary/file")
	if err != nil || string(body) != "updated" {
		t.Fatalf("current=%q error=%v", body, err)
	}
}

func TestCurrentWorkspaceSkipsSpecialNodesButRejectsReadingThem(t *testing.T) {
	root := t.TempDir()
	backend, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readWorkspaceFile(context.Background(), backend, "pipe"); err == nil {
		t.Fatal("FIFO accepted")
	}
	if count, err := walkFiles(backend, context.Background()); err != nil || count != 0 {
		t.Fatalf("walk with FIFO: count=%d err=%v", count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := readWorkspaceFile(ctx, backend, "pipe"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation=%v", err)
	}
	if _, err := walkFiles(backend, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("listing cancellation=%v", err)
	}
}

func TestReadLinkUsesHeldParent(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "selected")
	noError(t, os.Mkdir(selected, 0700))
	target := "../" + strings.Repeat("segment/", 40) + "original"
	noError(t, os.Symlink(target, filepath.Join(selected, "link")))
	directory, err := os.Open(selected)
	noError(t, err)
	defer directory.Close()
	noError(t, os.Rename(selected, filepath.Join(root, "moved")))
	noError(t, os.Mkdir(selected, 0700))
	noError(t, os.Symlink("replacement", filepath.Join(selected, "link")))
	body, err := readLinkAt(directory.Fd(), "link")
	if err != nil || string(body) != target {
		t.Fatalf("link text was truncated or read through the replaced parent: %q, %v", body, err)
	}
}

func walkFiles(b *workspace, ctx context.Context) (int, error) {
	count := 0
	_, err := b.walk(ctx, ".", nil, func(string, bool, *os.File) (bool, error) { count++; return true, nil })
	return count, err
}

func TestWalkPreservesResultsWhenChildDirectoryChanges(t *testing.T) {
	for _, mode := range []string{"removed", "file", "symlink", "fifo", "socket"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"a", "b"} {
				noError(t, os.Mkdir(filepath.Join(root, name), 0700))
				noError(t, os.WriteFile(filepath.Join(root, name, "hit.txt"), nil, 0600))
			}
			b, err := newWorkspace(root)
			noError(t, err)
			var visited []string
			var changed string
			incomplete, err := b.walk(context.Background(), ".", nil, func(name string, _ bool, _ *os.File) (bool, error) {
				visited = append(visited, name)
				changed = "a"
				if strings.HasPrefix(name, "a/") {
					changed = "b"
				}
				// Both entries were enumerated. Change the unopened sibling,
				// independent of filesystem entry order.
				target := filepath.Join(root, changed)
				noError(t, os.RemoveAll(target))
				switch mode {
				case "file":
					noError(t, os.WriteFile(target, nil, 0600))
				case "symlink":
					noError(t, os.Symlink("missing-target", target))
				case "fifo":
					noError(t, syscall.Mkfifo(target, 0600))
				case "socket":
					noError(t, syscall.Mknod(target, syscall.S_IFSOCK|0600, 0))
				}
				return true, nil
			})
			if err != nil || !incomplete || len(visited) != 1 {
				t.Fatalf("visited=%v incomplete=%v err=%v", visited, incomplete, err)
			}
			if _, err := b.walk(context.Background(), filepath.Join(changed, "hit.txt"), nil, func(string, bool, *os.File) (bool, error) {
				t.Fatal("unavailable explicit scope was visited")
				return true, nil
			}); err == nil {
				t.Fatal("unavailable explicit scope did not fail")
			}
		})
	}
}

func TestWalkPreservesResultsWhenChildFileChanges(t *testing.T) {
	for _, mode := range []string{"directory", "fifo"} {
		t.Run(mode, func(t *testing.T) {
			b, root := readWorkspace(t, map[string]string{"a": "needle", "b": "needle"})
			var visited []string
			incomplete, err := b.walk(context.Background(), ".", nil, func(name string, _ bool, directory *os.File) (bool, error) {
				file, _, err := openEntryAt(directory.Fd(), filepath.Base(name))
				if err != nil {
					return false, err
				}
				defer file.Close()
				if _, err := io.ReadAll(file); err != nil {
					return false, err
				}
				visited = append(visited, name)
				changed := "a"
				if name == "a" {
					changed = "b"
				}
				// Replace the other enumerated file before the visitor opens it.
				target := filepath.Join(root, changed)
				noError(t, os.Remove(target))
				if mode == "directory" {
					noError(t, os.Mkdir(target, 0700))
				} else {
					noError(t, syscall.Mkfifo(target, 0600))
				}
				return true, nil
			})
			if err != nil || !incomplete || len(visited) != 1 {
				t.Fatalf("visited=%v incomplete=%v err=%v", visited, incomplete, err)
			}
		})
	}
}

func TestWalkReadsThroughHeldParent(t *testing.T) {
	b, root := readWorkspace(t, map[string]string{"selected/a": "original", "selected/b": "original"})
	count := 0
	incomplete, err := b.walk(context.Background(), "selected", nil, func(name string, _ bool, directory *os.File) (bool, error) {
		file, _, err := openEntryAt(directory.Fd(), filepath.Base(name))
		if err != nil {
			return false, err
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			return false, err
		}
		if string(body) != "original" {
			t.Fatalf("visitor reopened the replaced parent: %q", body)
		}
		count++
		if count == 1 {
			noError(t, os.Rename(filepath.Join(root, "selected"), filepath.Join(root, "moved")))
			noError(t, os.Symlink(".magi", filepath.Join(root, "selected")))
		}
		return true, nil
	})
	if err != nil || incomplete || count != 2 {
		t.Fatalf("count=%d incomplete=%v err=%v", count, incomplete, err)
	}
}

func TestScopedWalkStopsEarlyAndNeverEntersOtherDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "selected"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "unrelated"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "unrelated", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(root, "selected", name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	_, err = b.walk(context.Background(), "selected", nil, func(name string, _ bool, _ *os.File) (bool, error) {
		count++
		if !strings.HasPrefix(name, "selected/") {
			t.Fatal(name)
		}
		return false, nil
	})
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

// Read the complete current file for boundary assertions.
func readWorkspaceFile(ctx context.Context, b *workspace, path string) ([]byte, fs.FileMode, error) {
	file, link, err := b.open(ctx, path)
	if err != nil {
		return nil, 0, err
	}
	if file == nil {
		return link, fs.ModeSymlink, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("workspace_file must be a regular file")
	}
	body, err := io.ReadAll(file)
	if err == nil {
		err = ctx.Err()
	}
	return body, info.Mode(), err
}
