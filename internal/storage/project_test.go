package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"magi/internal/storage"
)

func TestStartupRejectsStateDirectoryLinkBeforeCreatingLock(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	project, err := storage.InitProject(context.Background(), outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ".magi"), filepath.Join(root, ".magi")); err != nil {
		t.Fatal(err)
	}
	if unlock, err := storage.Lock(root); err == nil {
		unlock()
		t.Fatal("lock followed a linked state directory")
	}
	if _, err := os.Lstat(filepath.Join(outside, ".magi", "instance.lock")); !os.IsNotExist(err) {
		t.Fatalf("rejected startup created an external lock file: %v", err)
	}
}

func TestProjectOpenPreservesExistingInitPrefixDirectories(t *testing.T) {
	root := t.TempDir()
	abandoned := filepath.Join(root, ".magi-init-abandoned")
	userDir := filepath.Join(root, ".magi-init-user-data")
	if err := os.Mkdir(abandoned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, ".magi-initializing"), []byte("magi project initialization staging v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "partial-state"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, ".magi-private-init-v3"), []byte("magi private initialization v3 .magi-init-other "+strings.Repeat("0", 32)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "important.txt"), []byte("keep this file"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(abandoned, "partial-state")); err != nil {
		t.Fatalf("similarly named pre-existing directory was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(userDir, "important.txt")); err != nil {
		t.Fatalf("user directory was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".magi", ".magi-initializing")); !os.IsNotExist(err) {
		t.Fatalf("published state retained initialization marker: %v", err)
	}
	reopened, err := storage.OpenProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectOpenQuarantinesOwnedCrashStageBeforeWorkspaceCapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stage := makeCrashStage(t, root, ".magi-init-crashed")
	store, err := storage.InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	assertCrashStagePrivate(t, ctx, root, stage, store)
}

func makeCrashStage(t *testing.T, root, name string) string {
	t.Helper()
	stage := filepath.Join(root, name)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	marker := "magi private initialization v3 " + name + " " + strings.Repeat("a", 32) + "\n"
	if err := os.WriteFile(filepath.Join(stage, ".magi-private-init-v3"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "state.db"), []byte("private project state"), 0600); err != nil {
		t.Fatal(err)
	}
	return stage
}

func assertCrashStagePrivate(t *testing.T, ctx context.Context, root, stage string, store *storage.Project) {
	t.Helper()
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("crash stage remains in workspace: %v", err)
	}
	privateCopy := filepath.Join(root, ".magi", "abandoned-initializations", filepath.Base(stage), "state.db")
	if body, err := os.ReadFile(privateCopy); err != nil || string(body) != "private project state" {
		t.Fatalf("quarantined stage lost original bytes: %q: %v", body, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.OpenProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectOpenQuarantinesCopiedCrashStage(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	stage := makeCrashStage(t, source, ".magi-init-crashed")
	copyRoot := t.TempDir()
	copyStage := filepath.Join(copyRoot, filepath.Base(stage))
	if err := os.Mkdir(copyStage, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".magi-private-init-v3", "state.db"} {
		body, err := os.ReadFile(filepath.Join(stage, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copyStage, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.InitProject(ctx, copyRoot)
	if err != nil {
		t.Fatal(err)
	}
	assertCrashStagePrivate(t, ctx, copyRoot, copyStage, store)
}

func TestProjectInitializationAndStartupDoNotRequireGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	store, err := storage.InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".magi", "revisions.git")); !os.IsNotExist(err) {
		t.Fatalf("private Git directory exists: %v", err)
	}
	store, err = storage.OpenProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// Neither opening nor repeating init may silently repair an incomplete project.
func TestProjectInitAndOpenBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("missing", func(t *testing.T) {
		root := t.TempDir()
		if opened, err := storage.OpenProject(ctx, root); err == nil {
			opened.Close()
			t.Fatal("Open initialized a missing project")
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("Open changed root: %v %v", entries, err)
		}
		initialized, err := storage.InitProject(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		identity := initialized.Metadata()
		if err := initialized.Close(); err != nil {
			t.Fatal(err)
		}
		for _, open := range []func(context.Context, string) (*storage.Project, error){storage.InitProject, storage.OpenProject} {
			reopened, err := open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if reopened.Metadata() != identity {
				t.Fatal("existing identity was replaced")
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, missing := range []string{"project.json", "state.db", "safe-points", "history"} {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			initialized, err := storage.InitProject(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := initialized.Close(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, ".magi", missing)
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func(context.Context, string) (*storage.Project, error){storage.OpenProject, storage.InitProject} {
				if opened, err := open(ctx, root); err == nil {
					opened.Close()
					t.Fatal("incomplete project was accepted")
				}
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatalf("missing state was repaired: %v", err)
				}
			}
		})
	}
	t.Run("cancelled init", func(t *testing.T) {
		root := t.TempDir()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if opened, err := storage.InitProject(cancelled, root); !errors.Is(err, context.Canceled) {
			if opened != nil {
				opened.Close()
			}
			t.Fatalf("cancelled init: %v", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("cancelled init left state: %v %v", entries, err)
		}
	})
}

func TestProjectRejectsRequiredPathLinksBeforeOpen(t *testing.T) {
	for _, path := range []string{"project.json", "state.db", "safe-points", "history"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			s, err := storage.InitProject(ctx, root)
			noError(t, err)
			s.Close()
			target := filepath.Join(root, ".magi", path)
			err = os.MkdirAll(filepath.Dir(target), 0700)
			noError(t, err)
			err = os.RemoveAll(target)
			noError(t, err)
			external := filepath.Join(t.TempDir(), "private")
			err = os.WriteFile(external, []byte("unchanged"), 0600)
			noError(t, err)
			if path == "project.json" || path == "state.db" {
				err = os.Link(external, target)
			} else {
				err = os.Symlink(filepath.Dir(external), target)
			}
			noError(t, err)
			opened, err := storage.OpenProject(ctx, root)
			if err == nil {
				opened.Close()
				t.Fatal("linked internal path accepted")
			}
			body, err := os.ReadFile(external)
			if err != nil || string(body) != "unchanged" {
				t.Fatal("external file changed", err)
			}
		})
	}
}

func TestProjectOpenDoesNotScanArchives(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	project, err := storage.InitProject(ctx, root)
	noError(t, err)
	ref, err := project.Disk().Allocate("PLANNER")
	noError(t, err)
	noError(t, project.Disk().WriteFragment(ctx, ref, []byte("original")))
	noError(t, project.Close())
	stateDir := filepath.Join(root, ".magi")
	for _, path := range []string{"history/unused", "safe-points/expired", "private-note"} {
		noError(t, os.Symlink("missing", filepath.Join(stateDir, path)))
	}
	// History reads use the current file bytes without checking for replacement.
	path := filepath.Join(stateDir, "history", ref)
	noError(t, os.Remove(path))
	replacement := filepath.Join(t.TempDir(), "replacement")
	noError(t, os.WriteFile(replacement, []byte("current body"), 0600))
	noError(t, os.Link(replacement, path))
	project, err = storage.OpenProject(ctx, root)
	noError(t, err)
	defer project.Close()
	body, err := project.Disk().ReadFragment(ctx, ref)
	if err != nil || string(body) != "current body" {
		t.Fatalf("history read=%q err=%v", body, err)
	}
}

func TestProjectQuarantineRejectsLinkedDestination(t *testing.T) {
	ctx, root, outside := context.Background(), t.TempDir(), t.TempDir()
	project, err := storage.InitProject(ctx, root)
	noError(t, err)
	noError(t, project.Close())
	stage := makeCrashStage(t, root, ".magi-init-crashed")
	noError(t, os.Symlink(outside, filepath.Join(root, ".magi", "abandoned-initializations")))
	if opened, err := storage.OpenProject(ctx, root); err == nil {
		opened.Close()
		t.Fatal("quarantine followed a linked destination")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("rejected quarantine moved its source", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("quarantine changed outside directory: %v %v", entries, err)
	}
}

func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestProjectRejectsReplacedStateDirectoryWhileOpen(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	project, err := storage.InitProject(ctx, root)
	noError(t, err)
	defer project.Close()
	noError(t, project.ValidateStateDirectory())
	stateDir := filepath.Join(root, ".magi")
	noError(t, os.Rename(stateDir, filepath.Join(root, "moved")))
	noError(t, os.Mkdir(stateDir, 0700))
	if err := project.ValidateStateDirectory(); err == nil {
		t.Fatal("replaced state directory was accepted")
	}
}
