package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

func readWorkspace(t *testing.T, files map[string]string) (*workspace, string) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		target := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return b, root
}
func executeRead(t *testing.T, provider toolProvider, arguments string) []byte {
	t.Helper()
	r, err := provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return r.Body
}

func TestReadLinesAndDirectDirectoryScope(t *testing.T) {
	b, root := readWorkspace(t, map[string]string{"README.md": "first\n中文🙂\nthird\n", "child/file": "child bytes", ".hidden": "visible", ".magi/private": "private", ".git/config": "private"})
	if err := syscall.Mkfifo(filepath.Join(root, "child", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := readProvider{workspace: b}
	page := decodeReceipt[readResult](t, executeRead(t, provider, `{"path":"README.md","offset":2,"limit":1}`))
	if page.Path != "README.md" || page.Type != "file" || page.Content != "2: 中文🙂\n" || page.Next != 3 {
		t.Fatalf("line read=%+v", page)
	}
	entries := decodeReceipt[readResult](t, executeRead(t, provider, `{"path":"."}`)).Entries
	sort.Strings(entries)
	expected := []string{".hidden", "README.md", "child/"}
	sort.Strings(expected)
	if !reflect.DeepEqual(entries, expected) {
		t.Fatalf("directory=%v", entries)
	}
	child := decodeReceipt[readResult](t, executeRead(t, provider, `{"path":"child"}`))
	if !reflect.DeepEqual(child.Entries, []string{"file"}) {
		t.Fatal("special node disrupted directory listing", child)
	}
	if _, err := provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(`{"path":"child/pipe"}`)}); err == nil {
		t.Fatal("explicit special-file read accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("updated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if body := string(executeRead(t, provider, `{"path":"README.md"}`)); !strings.Contains(body, "updated") {
		t.Fatal(body)
	}
	if err := os.Symlink("/outside-secret", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if body := string(executeRead(t, provider, `{"path":"link"}`)); !strings.Contains(body, "symlink") || !strings.Contains(body, "/outside-secret") {
		t.Fatal(body)
	}
}

func TestReadPageDoesNotValidateFollowingLine(t *testing.T) {
	for _, invalid := range []string{"\x00binary", "\xffinvalid"} {
		b, _ := readWorkspace(t, map[string]string{"mixed.txt": "good\n" + invalid + "\n"})
		provider := readProvider{workspace: b}
		page := decodeReceipt[readResult](t, executeRead(t, provider, `{"path":"mixed.txt","limit":1}`))
		if page.Content != "1: good\n" || page.Next != 2 {
			t.Fatalf("page=%+v", page)
		}
		if _, err := provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(`{"path":"mixed.txt","offset":2,"limit":1}`)}); err == nil {
			t.Fatal("explicit binary line was accepted")
		}
	}
}

// Change a child after enumeration, independent of directory entry ordering.
type changingChildDirectory struct {
	*os.File
	change func() error
}

func (d *changingChildDirectory) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.File.ReadDir(n)
	if d.change != nil {
		change := d.change
		d.change = nil
		if err := change(); err != nil {
			return nil, err
		}
	}
	return entries, err
}

func TestReadDirectoryListsUnreadableChildren(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires directory permission enforcement")
	}
	for _, name := range []string{"locked", ".magi-init-locked"} {
		t.Run(name, func(t *testing.T) {
			b, root := readWorkspace(t, map[string]string{"visible.txt": "visible"})
			child := filepath.Join(root, name)
			noError(t, os.Mkdir(child, 0000))
			t.Cleanup(func() { _ = os.Chmod(child, 0700) })
			page := decodeReceipt[readResult](t, executeRead(t, readProvider{b}, `{"path":"."}`))
			privateCandidate := strings.HasPrefix(name, ".magi-init-")
			if page.Truncated != privateCandidate || slices.Contains(page.Entries, name+"/") == privateCandidate || !slices.Contains(page.Entries, "visible.txt") {
				t.Fatalf("page=%+v", page)
			}
		})
	}
}

func TestReadDirectoryRetainsPageWhenChildChanges(t *testing.T) {
	for _, mode := range []string{"removed", "file", "symlink", "fifo", "socket"} {
		t.Run(mode, func(t *testing.T) {
			_, root := readWorkspace(t, map[string]string{"kept.txt": "kept"})
			child := filepath.Join(root, ".magi-init-changed")
			noError(t, os.Mkdir(child, 0700))
			dir, err := os.Open(root)
			noError(t, err)
			defer dir.Close()
			changing := &changingChildDirectory{File: dir, change: func() error {
				if err := os.Remove(child); err != nil {
					return err
				}
				switch mode {
				case "file":
					return os.WriteFile(child, []byte("replacement"), 0600)
				case "symlink":
					return os.Symlink("missing-target", child)
				case "fifo":
					return syscall.Mkfifo(child, 0600)
				case "socket":
					return syscall.Mknod(child, syscall.S_IFSOCK|0600, 0)
				}
				return nil
			}}
			page, err := readDirectory(context.Background(), changing, readArgs{Path: ".", Offset: 1, Limit: inlineOutputLines})
			if err != nil || !page.Truncated || !reflect.DeepEqual(page.Entries, []string{"kept.txt"}) || page.Next != 0 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
		})
	}
}

func TestReadDirectoryUsesHeldParent(t *testing.T) {
	_, root := readWorkspace(t, map[string]string{"selected/.magi-init-user/file.txt": "original", "selected/kept.txt": "kept"})
	selected := filepath.Join(root, "selected")
	dir, err := os.Open(selected)
	noError(t, err)
	defer dir.Close()
	changing := &changingChildDirectory{File: dir, change: func() error {
		if err := os.Rename(selected, filepath.Join(root, "moved")); err != nil {
			return err
		}
		return os.Mkdir(selected, 0700)
	}}
	page, err := readDirectory(context.Background(), changing, readArgs{Path: "selected", Offset: 1, Limit: inlineOutputLines})
	noError(t, err)
	if page.Truncated || page.Next != 0 || len(page.Entries) != 2 || !slices.Contains(page.Entries, ".magi-init-user/") || !slices.Contains(page.Entries, "kept.txt") {
		t.Fatalf("page did not retain the original scope: %+v", page)
	}
}

// Return the fault with the final bytes once, then EOF, as a Reader may do.
type partialReadFailure struct{ text string }

func (r *partialReadFailure) Read(p []byte) (int, error) {
	if r.text == "" {
		return 0, io.EOF
	}
	n := copy(p, r.text)
	r.text = r.text[n:]
	if r.text == "" {
		return n, syscall.EIO
	}
	return n, nil
}

func TestReadPagePreservesReadFailure(t *testing.T) {
	for _, tc := range []struct {
		text   string
		limit  int
		failed bool
	}{
		{"partial", 1, true},
		{"partial", 2, true},
		{"complete\npartial", 1, false},
		{"complete\npartial", 2, true},
	} {
		page, err := readText(context.Background(), &partialReadFailure{text: tc.text}, readArgs{Path: "source", Offset: 1, Limit: tc.limit})
		if tc.failed {
			if !errors.Is(err, syscall.EIO) {
				t.Errorf("text=%q limit=%d lost read failure: page=%+v err=%v", tc.text, tc.limit, page, err)
			}
		} else if err != nil || page.Content != "1: complete\n" || page.Next != 2 {
			t.Errorf("following-page failure affected complete page: page=%+v err=%v", page, err)
		}
	}
}

func TestReadPageStopsBeforeDrainingFollowingLine(t *testing.T) {
	for _, suffix := range []string{"", "\n", "\n" + strings.Repeat("x", 4<<20)} {
		text := "good" + suffix
		reader := &io.LimitedReader{R: strings.NewReader(text), N: int64(len(text))}
		page, err := readText(context.Background(), reader, readArgs{Path: "source", Offset: 1, Limit: 1})
		noError(t, err)
		next := 0
		if len(suffix) > 1 {
			next = 2
		}
		if page.Content != "1: good\n" || page.Next != next || int64(len(text))-reader.N > 32<<10 {
			t.Fatalf("page=%+v bytes_read=%d", page, int64(len(text))-reader.N)
		}
	}
}

func TestReadAndGrepLineLimitExcludesLineEndings(t *testing.T) {
	for _, size := range []int{lineBytes - 1, lineBytes, lineBytes + 1} {
		for _, ending := range []string{"", "\n", "\r\n"} {
			t.Run(fmt.Sprintf("%d/%q", size, ending), func(t *testing.T) {
				text := "needle" + strings.Repeat("x", size-len("needle"))
				b, _ := readWorkspace(t, map[string]string{"source": text + ending})
				page := decodeReceipt[readResult](t, executeRead(t, readProvider{b}, `{"path":"source"}`))
				if page.Content != "1: "+text[:min(size, lineBytes)]+"\n" || page.LinesClipped != (size > lineBytes) {
					t.Fatalf("incorrect read boundary: bytes=%d clipped=%v", len(page.Content), page.LinesClipped)
				}
				result := decodeReceipt[grepResult](t, executeRead(t, grepProvider{b}, `{"path":"source","pattern":"needle"}`))
				want := 1
				if size > lineBytes {
					want = 0
				}
				if result.Count != want || !result.Truncated {
					t.Fatalf("search=%+v", result)
				}
			})
		}
	}
}

func TestGrepRetainsMatchesAroundProtectedHardlinks(t *testing.T) {
	b, root := readWorkspace(t, map[string]string{"first.txt": "needle one", ".magi/private": "needle secret", "last.txt": "needle two"})
	if err := os.Link(filepath.Join(root, ".magi/private"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	provider := grepProvider{workspace: b}
	result := decodeReceipt[grepResult](t, executeRead(t, provider, `{"path":".","pattern":"needle"}`))
	if result.Count != 2 || !result.Truncated || result.Matches["first.txt"] == "" || result.Matches["last.txt"] == "" || result.Matches["alias.txt"] != "" {
		t.Fatal("protected file discarded other matches or leaked content", result)
	}
	if _, err := provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(`{"path":"alias.txt","pattern":"needle"}`)}); err == nil {
		t.Fatal("explicit protected target accepted")
	}
}

func TestGlobAndGrepRespectScopePatternsAndPrivatePaths(t *testing.T) {
	b, root := readWorkspace(t, map[string]string{"src/a.go": "Alpha\nneedle one\nother\nneedle again\n", "src/sub/b.go": "needle two\n", "src/.hidden.go": "needle hidden\n", "src/no.txt": "needle ignored\n", "other/out.go": "needle outside\n", "src/.magi/private.go": "needle private\n", "src/.git/config": "needle git"})
	// Neither matching nor unrelated special nodes should abort a search.
	for _, name := range []string{"src/pipe", "src/sub/pipe.go"} {
		if err := syscall.Mkfifo(filepath.Join(root, name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "src", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other", "out.go"), filepath.Join(root, "src", "link.go")); err != nil {
		t.Fatal(err)
	}
	glob := globProvider{workspace: b}
	files := decodeReceipt[globResult](t, executeRead(t, glob, `{"path":"src","pattern":"**/*.go"}`)).Files
	sort.Strings(files)
	if !reflect.DeepEqual(files, []string{"src/.hidden.go", "src/a.go", "src/link.go", "src/sub/b.go"}) {
		t.Fatalf("glob=%v", files)
	}
	grep := grepProvider{workspace: b}
	result := decodeReceipt[grepResult](t, executeRead(t, grep, `{"path":"src","pattern":"^needle","include":"*.go"}`))
	want := map[string]string{"src/.hidden.go": "1: needle hidden\n", "src/a.go": "2: needle one\n4: needle again\n", "src/sub/b.go": "1: needle two\n"}
	if result.Count != 4 || !reflect.DeepEqual(result.Matches, want) {
		t.Fatalf("grouped search lost lines or escaped scope: %+v", result)
	}
	if got := decodeReceipt[grepResult](t, executeRead(t, grep, `{"path":"src/a.go","pattern":"(?i)^alpha$"}`)); got.Matches["src/a.go"] != "1: Alpha\n" {
		t.Fatal(got)
	}
}

func TestSearchPrunesUnrelatedDirectories(t *testing.T) {
	b, _ := readWorkspace(t, map[string]string{
		"src/a.go": "needle", "src/sub/b.go": "needle", "src/deep/nested/c.go": "needle",
		"other/\xff": "needle", "literal*/a.go": "needle",
	})
	for _, tc := range []struct {
		pattern string
		want    []string
	}{
		{"src/**/*.go", []string{"src/a.go", "src/deep/nested/c.go", "src/sub/b.go"}},
		{"s[rt]c/*/*.go", []string{"src/sub/b.go"}},
		{`literal\*/*.go`, []string{"literal*/a.go"}},
		{"src/*.go", []string{"src/a.go"}},
		{"missing/**/*.go", []string{}},
	} {
		args, err := json.Marshal(searchArgs{Pattern: tc.pattern})
		noError(t, err)
		got := decodeReceipt[globResult](t, executeRead(t, globProvider{workspace: b}, string(args)))
		sort.Strings(got.Files)
		if got.Truncated || !reflect.DeepEqual(got.Files, tc.want) {
			t.Fatalf("pattern %q: got %+v, want %v; unrelated unreadable names must not affect results", tc.pattern, got, tc.want)
		}
		args, err = json.Marshal(searchArgs{Pattern: "needle", Include: tc.pattern})
		noError(t, err)
		matches := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, string(args)))
		if matches.Truncated || matches.Count != len(tc.want) {
			t.Fatalf("include %q: search traversed unrelated directories or lost matches: %+v", tc.pattern, matches)
		}
		for _, name := range tc.want {
			if matches.Matches[name] != "1: needle\n" {
				t.Fatalf("include %q: missing match in %s", tc.pattern, name)
			}
		}
	}
	got := decodeReceipt[grepResult](t, executeRead(t, grepProvider{b}, `{"path":"src","pattern":"needle","include":"sub/*.go"}`))
	if got.Truncated || !reflect.DeepEqual(got.Matches, map[string]string{"src/sub/b.go": "1: needle\n"}) {
		t.Fatal("directory pruning did not respect the selected scope", got)
	}
}

func TestSearchSkipsInvalidUTF8Paths(t *testing.T) {
	b, _ := readWorkspace(t, map[string]string{
		"ok.txt": "needle", "\xff.txt": "needle", "\xfe.txt": "needle", "\xff/child.txt": "needle",
	})
	files := decodeReceipt[globResult](t, executeRead(t, globProvider{workspace: b}, `{"pattern":"**/*"}`))
	if !files.Truncated || !reflect.DeepEqual(files.Files, []string{"ok.txt"}) {
		t.Fatalf("glob returned unaddressable paths: %+v", files)
	}
	matches := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, `{"pattern":"needle"}`))
	if !matches.Truncated || matches.Count != 1 || !reflect.DeepEqual(matches.Matches, map[string]string{"ok.txt": "1: needle\n"}) {
		t.Fatalf("grep corrupted path keys: %+v", matches)
	}
}

func TestReadAndSearchBoundsAndCancellation(t *testing.T) {
	b, _ := readWorkspace(t, map[string]string{"big": "start\n" + strings.Repeat("中", 5000) + "\nend\n"})
	read := readProvider{workspace: b}
	text := string(executeRead(t, read, `{"path":"big"}`))
	if !decodeReceipt[readResult](t, []byte(text)).LinesClipped || !utf8.ValidString(text) || !strings.Contains(text, "3: end") {
		t.Fatalf("large UTF-8 line=%s", text)
	}
	page := string(executeRead(t, read, `{"path":"big","limit":2}`))
	got := decodeReceipt[readResult](t, []byte(page))
	if got.Next != 3 || !got.LinesClipped {
		t.Fatalf("paged read lost continuation or clipping notice: %s", page)
	}
	grep := grepProvider{workspace: b}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, p := range []toolProvider{read, grep, globProvider{workspace: b}} {
		_, err := p.Execute(ctx, toolExecution{Arguments: json.RawMessage(`{"path":"big","pattern":"match"}`)})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	}
}

func TestGlobItemLimit(t *testing.T) {
	for _, count := range []int{searchLimit - 1, searchLimit, searchLimit + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			files := make(map[string]string, count)
			for i := range count {
				files[fmt.Sprintf("file-%03d.txt", i)] = ""
			}
			b, _ := readWorkspace(t, files)
			result := decodeReceipt[globResult](t, executeRead(t, globProvider{workspace: b}, `{"pattern":"*.txt"}`))
			if len(result.Files) != min(count, searchLimit) || result.Truncated != (count >= searchLimit) {
				t.Fatalf("count=%d result=%+v", count, result)
			}
		})
	}
}

func TestGrepMarksNonTextRemainder(t *testing.T) {
	for _, invalid := range []string{"\x00binary", "\xffinvalid"} {
		for _, prefix := range []string{"", "needle before\n"} {
			b, _ := readWorkspace(t, map[string]string{
				"source": prefix + invalid + "\nneedle after\n",
				"other":  "needle elsewhere\n",
			})
			result := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, `{"pattern":"needle"}`))
			want := map[string]string{"other": "1: needle elsewhere\n"}
			if prefix != "" {
				want["source"] = "1: needle before\n"
			}
			if !result.Truncated || result.Count != len(want) || !reflect.DeepEqual(result.Matches, want) {
				t.Errorf("invalid=%q prefix=%q result=%+v", invalid, prefix, result)
			}
		}
	}
}

func TestGrepLimitsSpanFiles(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          int
	}{
		{"matches", strings.Repeat("match\n", 60), searchLimit},
		{"exact_matches", strings.Repeat("match\n", searchLimit/2), searchLimit},
		{"bytes", strings.Repeat("match"+strings.Repeat("x", 1995)+"\n", 16), 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := readWorkspace(t, map[string]string{"a": tc.content, "b": tc.content})
			result := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, `{"pattern":"match"}`))
			text := result.Matches["a"] + result.Matches["b"]
			if result.Matches["a"] == "" || result.Matches["b"] == "" || strings.Count(text, "\n") != tc.want || result.Count != tc.want || len(text) > inlineOutputBytes || !result.Truncated {
				t.Fatalf("file grouping changed global bounds: %+v", result)
			}
		})
	}
}

func TestGrepLongLinePreviewsPreserveMatchesAndReadback(t *testing.T) {
	for _, tc := range []struct {
		name, text, pattern, visible string
		prefix, suffix               bool
	}{
		{"start", "needle" + strings.Repeat("x", 3000), "^needle", "needle", false, true},
		{"end", strings.Repeat("x", 3000) + "needle", "needle$", "needle", true, false},
		{"unicode", strings.Repeat("界", 800) + "needle" + strings.Repeat("🙂", 800), "needle", "needle", true, true},
		{"wide match", "head" + strings.Repeat("x", 3000) + "tail", "x+", "xxxx", true, true},
		{"empty match", strings.Repeat("界", 1000), "$", "界", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := readWorkspace(t, map[string]string{"source": tc.text + "\n"})
			args, err := json.Marshal(searchArgs{Path: "source", Pattern: tc.pattern})
			noError(t, err)
			result := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, string(args)))
			line := result.Matches["source"]
			if result.Count != 1 || !result.Truncated || !strings.HasPrefix(line, "1: ") || !strings.HasSuffix(line, "\n") || !utf8.ValidString(line) || !strings.Contains(line, tc.visible) {
				t.Fatalf("preview lost match or location: %+v", result)
			}
			preview := strings.TrimSuffix(strings.TrimPrefix(line, "1: "), "\n")
			if strings.HasPrefix(preview, "[prefix omitted] ") != tc.prefix || strings.HasSuffix(preview, " [suffix omitted]") != tc.suffix {
				t.Fatalf("incorrect omission markers: %s", preview)
			}
			preview = strings.TrimSuffix(strings.TrimPrefix(preview, "[prefix omitted] "), " [suffix omitted]")
			if len(preview) > grepPreviewBytes || !strings.Contains(tc.text, preview) {
				t.Fatal("preview exceeded budget or changed source text")
			}
			page := decodeReceipt[readResult](t, executeRead(t, readProvider{workspace: b}, `{"path":"source","offset":1,"limit":1}`))
			if page.LinesClipped || page.Content != "1: "+tc.text+"\n" {
				t.Fatal("full matched line cannot be read back")
			}
		})
	}
	// Matching a cropped prefix would incorrectly accept the first line.
	b, _ := readWorkspace(t, map[string]string{"source": "needle" + strings.Repeat("x", 3000) + "\nneedle\n"})
	result := decodeReceipt[grepResult](t, executeRead(t, grepProvider{workspace: b}, `{"path":"source","pattern":"^needle$"}`))
	if result.Count != 1 || result.Matches["source"] != "2: needle\n" || result.Truncated {
		t.Fatalf("preview changed matching semantics: %+v", result)
	}
}

func TestReadSearchArgumentErrorsAreRepairable(t *testing.T) {
	for _, tc := range []struct {
		provider toolProvider
		args     string
	}{
		{readProvider{}, `{"path":"file","offset":100000000000000000000}`},
		{readProvider{}, `{"path":".git/../file"}`},
		{globProvider{}, `{"path":"../outside","pattern":"**"}`},
		{globProvider{}, `{"pattern":"["}`},
		{grepProvider{}, `{"pattern":"(?=unsupported)"}`},
		{grepProvider{}, `{"pattern":"ok","include":"["}`},
		{grepProvider{}, `{"path":".magi/state","pattern":"ok"}`},
	} {
		result, err := tc.provider.Execute(context.Background(), toolExecution{Arguments: json.RawMessage(tc.args)})
		var feedback protocol.CandidateFeedback
		if result.Status != "" || !errors.As(err, &feedback) || feedback.PublicMessage == "" {
			t.Fatalf("missing repair: %s => %v", tc.args, err)
		}
	}
}
