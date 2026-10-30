package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"magi/internal/core/protocol"
)

func TestWriteCreatesAndOverwritesExactText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "file.txt")
	for i, text := range []string{"first\r\nno final newline", "", "\ufefflast\n"} {
		result, err := (writeProvider{}).Execute(context.Background(), execution(root, writeArgs{Path: "nested/file.txt", Content: text}))
		noError(t, err)
		fields := receiptFields(t, result.Body)
		operation := "updated"
		if i == 0 {
			operation = "created"
		}
		if result.Status != protocol.ToolSucceeded || len(fields) != 2 || fields["path"] != "nested/file.txt" || fields["operation"] != operation {
			t.Fatalf("unexpected write receipt: %s", result.Body)
		}
		body, err := os.ReadFile(path)
		noError(t, err)
		if string(body) != text {
			t.Fatalf("write changed the supplied text: %q", body)
		}
		info, err := os.Stat(path)
		noError(t, err)
		if i > 0 && info.Mode().Perm() != 0750 {
			t.Fatalf("write changed permissions: %v", info.Mode())
		}
		noError(t, os.Chmod(path, 0750))
	}
}

func TestEditExactReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, source, old, replacement, want, diagnostic string
		all                                              bool
		count                                            int
	}{
		{name: "unique", source: "alpha beta", old: "beta", replacement: "next", want: "alpha next", count: 1},
		{name: "all", source: "x x x", old: "x", replacement: "xx", want: "xx xx xx", all: true, count: 3},
		{name: "delete", source: "a\nb\n", old: "b\n", want: "a\n", count: 1},
		{name: "missing", source: "alpha", old: "beta", replacement: "next", diagnostic: "not found"},
		{name: "ambiguous", source: "x x", old: "x", replacement: "y", diagnostic: "matches 2 times"},
		{name: "empty", source: "alpha", replacement: "next", diagnostic: "nonempty"},
		{name: "no-op", source: "alpha", old: "alpha", replacement: "alpha", diagnostic: "differ"},
		{name: "newline-no-op", source: "a\r\n", old: "a\n", replacement: "a\r\n", diagnostic: "differ"},
		{name: "bom-crlf", source: "\ufeffa\r\nb", old: "a\nb", replacement: "c\nd", want: "\ufeffc\r\nd", count: 1},
		{name: "bom-verbatim", source: "\ufeffa\r\nb", old: "\ufeffa\nb", replacement: "\ufeffc\nd", want: "\ufeffc\r\nd", count: 1},
		{name: "bom-preserved", source: "\ufeffa\nb", old: "\ufeffa", replacement: "c", want: "\ufeffc\nb", count: 1},
		{name: "bom-only-change", source: "\ufeffa", old: "\ufeffa", replacement: "a", diagnostic: "would not change"},
		{name: "bom-unique-prefix", source: "\ufeffa a", old: "\ufeffa", replacement: "b", want: "\ufeffb a", count: 1},
		{name: "bom-all-exact", source: "\ufeffa a", old: "\ufeffa", replacement: "b", want: "\ufeffb a", all: true, count: 1},
		{name: "lf", source: "a\nb\n", old: "a\r\nb", replacement: "c\r\nd", want: "c\nd\n", count: 1},
		{name: "mixed", source: "a\r\nb\n", old: "a", replacement: "c", diagnostic: "mixed LF/CRLF"},
		{name: "binary", source: "a\x00b", old: "a", replacement: "c", diagnostic: "UTF-8 without NUL"},
		{name: "invalid-utf8", source: "a\xff", old: "a", replacement: "c", diagnostic: "UTF-8 without NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file")
			noError(t, os.WriteFile(path, []byte(tc.source), 0750))
			result, err := (editProvider{}).Execute(context.Background(), execution(root, editArgs{
				Path: "file", OldString: tc.old, NewString: tc.replacement, ReplaceAll: tc.all,
			}))
			want := tc.want
			if tc.diagnostic != "" {
				want = tc.source
				if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
					t.Fatalf("expected %q, got %s %v", tc.diagnostic, result.Body, err)
				}
			} else {
				noError(t, err)
				fields := receiptFields(t, result.Body)
				if result.Status != protocol.ToolSucceeded || len(fields) != 2 || fields["path"] != "file" || fields["replacements"] != float64(tc.count) {
					t.Fatalf("unexpected edit receipt: %s", result.Body)
				}
			}
			body, err := os.ReadFile(path)
			noError(t, err)
			info, err := os.Stat(path)
			noError(t, err)
			if string(body) != want || info.Mode().Perm() != 0750 {
				t.Fatalf("edit changed unintended bytes/permissions: %q %v", body, info.Mode())
			}
		})
	}
}

func TestEditMissingFileDoesNotCreateDirectories(t *testing.T) {
	root := t.TempDir()
	_, err := (editProvider{}).Execute(context.Background(), execution(root, editArgs{Path: "missing/file", OldString: "a", NewString: "b"}))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatal("missing edit source accepted", err)
	}
	entries, err := os.ReadDir(root)
	noError(t, err)
	if len(entries) != 0 {
		t.Fatal("failed edit created directories", entries)
	}
}

func TestMutationsRejectLinksAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	noError(t, os.WriteFile(original, []byte("old"), 0600))
	noError(t, os.Symlink(original, filepath.Join(root, "symlink")))
	noError(t, os.Link(original, filepath.Join(root, "hardlink")))
	noError(t, syscall.Mkfifo(filepath.Join(root, "fifo"), 0600))
	for _, path := range []string{"symlink", "hardlink", "fifo"} {
		for _, provider := range []toolProvider{writeProvider{}, editProvider{}} {
			_, err := provider.Execute(context.Background(), execution(root, map[string]any{
				"path": path, "content": "new", "old_string": "old", "new_string": "new",
			}))
			if err == nil {
				t.Fatalf("%T accepted %s", provider, path)
			}
		}
	}
	body, err := os.ReadFile(original)
	noError(t, err)
	if string(body) != "old" {
		t.Fatal("linked source changed", string(body))
	}
}

func TestTextMutationSizeLimits(t *testing.T) {
	root := t.TempDir()
	_, err := (writeProvider{}).Execute(context.Background(), execution(root, writeArgs{Path: "large", Content: strings.Repeat("x", maxTextFileBytes+1)}))
	if err == nil {
		t.Fatal("oversized write accepted")
	}
	file, err := os.Create(filepath.Join(root, "large"))
	noError(t, err)
	noError(t, file.Truncate(maxTextFileBytes+1))
	noError(t, file.Close())
	_, err = (editProvider{}).Execute(context.Background(), execution(root, editArgs{Path: "large", OldString: "x", NewString: "y"}))
	if err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatal("oversized source accepted", err)
	}
	_, _, err = replaceText(strings.Repeat("x", maxTextFileBytes), editArgs{OldString: "x", NewString: "xx", ReplaceAll: true})
	if err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatal("oversized replacement accepted", err)
	}
}

func TestPublicationRefusesStaleSourceAndCleansTemporaryFiles(t *testing.T) {
	for _, reason := range []string{"modified", "created", "cancelled", "invalid-name"} {
		t.Run(reason, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutation, err := openFileMutation(ctx, root)
			noError(t, err)
			defer mutation.root.Close()
			var before *fileSnapshot
			if reason != "created" {
				noError(t, os.WriteFile(path, []byte("old"), 0600))
				before, err = mutation.readAt(mutation.root, "file")
				noError(t, err)
			}
			want := "old"
			name := "file"
			switch reason {
			case "modified", "created":
				want = "concurrent"
				noError(t, os.WriteFile(path, []byte(want), 0600))
			case "cancelled":
				cancel()
			case "invalid-name":
				name = strings.Repeat("x", 256)
			}
			err = mutation.publish(mutation.root, name, []byte("replacement"), before)
			if err == nil || (reason == "cancelled" && !errors.Is(err, context.Canceled)) {
				t.Fatal("publication succeeded unexpectedly", err)
			}
			body, err := os.ReadFile(path)
			noError(t, err)
			entries, err := os.ReadDir(root)
			noError(t, err)
			if string(body) != want || len(entries) != 1 || entries[0].Name() != "file" {
				t.Fatalf("failed publication changed source or leaked temporary files: %q %v", body, entries)
			}
		})
	}
}
