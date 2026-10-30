package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

const lineBytes = 8 << 10

type readProvider struct{ workspace *workspace }

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func projectPath(value string) (string, error) {
	if filepath.IsAbs(value) || strings.ContainsRune(value, 0) {
		return "", protocol.InvalidCandidate("path must be a project-relative file or directory; use . for the project root")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == ".magi" || part == ".git" {
			return "", protocol.InvalidCandidate("path cannot traverse a parent or enter .magi/.git")
		}
	}
	return filepath.ToSlash(filepath.Clean(value)), nil
}
func decodeReadArgs(arguments json.RawMessage) (readArgs, error) {
	var a readArgs
	if err := json.Unmarshal(arguments, &a); err != nil {
		return a, protocol.InvalidCandidate("read requires path and optional integer offset/limit")
	}
	var err error
	a.Path, err = projectPath(a.Path)
	if err != nil {
		return a, err
	}
	if a.Offset == 0 {
		a.Offset = 1
	}
	if a.Limit == 0 {
		a.Limit = inlineOutputLines
	}
	return a, nil
}

type readResult struct {
	Path         string   `json:"path"`
	Type         string   `json:"type"`
	Content      string   `json:"content,omitempty"`
	Entries      []string `json:"entries,omitempty"`
	Next         int      `json:"next_offset,omitempty"`
	LinesClipped bool     `json:"lines_clipped,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
}

// visitLines bounds retained memory even for a giant source line. Long lines
// carry an explicit truncation flag; cancellation is checked while draining.
func visitLines(ctx context.Context, reader *bufio.Reader, maxLineBytes int, trimCR bool, visit func(int, string, bool) (bool, error)) error {
	for number := 1; ; number++ {
		var line []byte
		clipped := false
		var readErr error
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			part, err := reader.ReadSlice('\n')
			// Keep room for LF or CRLF without charging it to source text.
			remaining := maxLineBytes + 2 - len(line)
			if len(part) > remaining {
				clipped = true
			}
			line = append(line, part[:min(len(part), remaining)]...)
			if err == bufio.ErrBufferFull {
				continue
			}
			readErr = err
			break
		}
		// A failed read cannot produce a complete line, even if the visitor
		// would stop at this page boundary. EOF alone may end a valid last line.
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if len(line) == 0 && readErr == io.EOF {
			return nil
		}
		if !clipped {
			line = bytes.TrimSuffix(line, []byte("\n"))
			if trimCR {
				line = bytes.TrimSuffix(line, []byte("\r"))
			}
		}
		if len(line) > maxLineBytes {
			clipped = true
			line = line[:maxLineBytes]
		}
		if clipped && len(line) > 0 {
			end := len(line) - 1
			for end > 0 && !utf8.RuneStart(line[end]) {
				end--
			}
			if !utf8.FullRune(line[end:]) {
				line = line[:end]
			}
		}
		keep, err := visit(number, string(line), clipped)
		if err != nil || !keep {
			return err
		}
		if readErr == io.EOF {
			return nil
		}
	}
}
func (p readProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	a, err := decodeReadArgs(request.Arguments)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	file, link, err := p.workspace.open(ctx, a.Path)
	if err != nil {
		return toolFailure(err)
	}
	result := readResult{Path: a.Path}
	if file == nil {
		if !utf8.Valid(link) {
			return toolFailure(errors.New("link text is not UTF-8"))
		}
		result.Type = "symlink"
		result.Content = string(link)
		return encodeResult(result, nil)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return toolFailure(err)
	}
	if info.IsDir() {
		result, err = readDirectory(ctx, file, a)
	} else {
		result, err = readText(ctx, file, a)
	}
	if err != nil {
		return toolFailure(err)
	}
	return encodeResult(result, nil)
}

func readText(ctx context.Context, file io.Reader, a readArgs) (readResult, error) {
	return readTextWindow(ctx, file, a, true)
}

// Git shares the line window but keeps patch text without source line numbers.
func readTextWindow(ctx context.Context, file io.Reader, a readArgs, numbered bool) (readResult, error) {
	result := readResult{Path: a.Path, Type: "file"}
	reader := bufio.NewReaderSize(file, 32<<10)
	used, count := 0, 0
	var content strings.Builder
	// Leave room for the newline so even an oversized first line advances.
	maxLineBytes := inlineOutputBytes - 1
	if numbered {
		maxLineBytes = lineBytes
	}
	err := visitLines(ctx, reader, maxLineBytes, numbered, func(number int, text string, clipped bool) (bool, error) {
		if number < a.Offset {
			return true, nil
		}
		line := text + "\n"
		if numbered {
			line = fmt.Sprintf("%d: %s\n", number, text)
		}
		if used+len(line) > inlineOutputBytes {
			result.Next = number
			return false, nil
		}
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return false, errors.New("read requires UTF-8 text without NUL; use bash for binary data")
		}
		content.WriteString(line)
		used += len(line)
		count++
		result.LinesClipped = result.LinesClipped || clipped
		if count == a.Limit || used == inlineOutputBytes {
			// Only detect another line; do not drain it just to finish this page.
			_, err := reader.Peek(1)
			if err == io.EOF {
				return false, nil
			}
			if err == nil {
				result.Next = number + 1
			}
			return false, err
		}
		return true, nil
	})
	result.Content = content.String()
	return result, err
}

type directoryReader interface {
	ReadDir(int) ([]fs.DirEntry, error)
	Fd() uintptr
}

// readDirectory lists one scope; disappearing children do not discard the page.
func readDirectory(ctx context.Context, file directoryReader, a readArgs) (readResult, error) {
	result := readResult{Path: a.Path, Type: "directory", Entries: []string{}}
	used, count := 0, 0
	position := 0
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entries, readErr := file.ReadDir(64)
		for _, entry := range entries {
			if privateComponent(entry.Name()) || entry.Name() == tempDirectory {
				continue
			}
			name := entry.Name()
			if !entry.IsDir() && !entry.Type().IsRegular() && entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if entry.IsDir() {
				if strings.HasPrefix(name, ".magi-init-") {
					child, err := openPublicDirectory(file.Fd(), name)
					if skippableEntryError(err) {
						result.Truncated = true
						continue
					}
					if err != nil {
						return result, err
					}
					if child == nil {
						continue
					}
					child.Close()
				}
				name += "/"
			}
			position++
			if position < a.Offset {
				continue
			}
			if count >= a.Limit || used+len(name) > inlineOutputBytes {
				result.Next = position
				return result, nil
			}
			if !utf8.ValidString(name) {
				return result, errors.New("directory entry is not UTF-8")
			}
			result.Entries = append(result.Entries, name)
			count++
			used += len(name)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return result, readErr
		}
	}
	return result, nil
}
