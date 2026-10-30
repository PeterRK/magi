package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

const searchLimit = 100
const grepPreviewBytes = 2 << 10

// grepPreview displays the first match and nearby text after the full line has
// been matched. A match wider than the budget is shown from its beginning.
func grepPreview(text string, start, end int) string {
	if len(text) <= grepPreviewBytes {
		return text
	}
	padding := max(0, grepPreviewBytes-(end-start)) / 2
	start = max(0, start-padding)
	end = min(len(text), start+grepPreviewBytes)
	start = max(0, end-grepPreviewBytes)
	for start < end && !utf8.RuneStart(text[start]) {
		start++
	}
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	preview := text[start:end]
	if start > 0 {
		preview = "[prefix omitted] " + preview
	}
	if end < len(text) {
		preview += " [suffix omitted]"
	}
	return preview
}

type globProvider struct{ workspace *workspace }
type grepProvider struct{ workspace *workspace }

type searchArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Include string `json:"include,omitempty"`
}

func globPattern(pattern string) (func(string) bool, error) {
	if strings.HasPrefix(pattern, "/") || strings.ContainsRune(pattern, 0) {
		return nil, protocol.InvalidCandidate("glob pattern must be a nonempty relative pattern")
	}
	parts := strings.Split(pattern, "/")
	for _, part := range parts {
		if part == ".." {
			return nil, protocol.InvalidCandidate("glob pattern cannot traverse a parent")
		}
		if part != "**" {
			if _, err := path.Match(part, ""); err != nil {
				return nil, protocol.InvalidCandidate("invalid glob pattern: " + err.Error())
			}
		}
	}
	return func(value string) bool {
		names := strings.Split(value, "/")
		// Dynamic programming keeps repeated ** segments from exponential backtracking.
		previous := make([]bool, len(names)+1)
		previous[0] = true
		for _, part := range parts {
			next := make([]bool, len(names)+1)
			if part == "**" {
				next[0] = previous[0]
				for j := 1; j <= len(names); j++ {
					next[j] = previous[j] || next[j-1]
				}
			} else {
				for j := 1; j <= len(names); j++ {
					matched, _ := path.Match(part, names[j-1])
					next[j] = previous[j-1] && matched
				}
			}
			previous = next
		}
		return previous[len(names)]
	}, nil
}

// globDirectories prunes paths that cannot contain a match. Before the first
// **, every directory must match the corresponding segment. After **, leave
// traversal unrestricted and let the full matcher decide.
func globDirectories(pattern, scope string) func(string) bool {
	parts := strings.Split(pattern, "/")
	return func(name string) bool {
		for i, segment := range strings.Split(relativeSearchPath(scope, name), "/") {
			if i >= len(parts) {
				return false
			}
			if parts[i] == "**" {
				return true
			}
			matched, _ := path.Match(parts[i], segment)
			if !matched || i == len(parts)-1 {
				return false
			}
		}
		return true
	}
}

func decodeSearchArgs(arguments json.RawMessage) (searchArgs, error) {
	var a searchArgs
	if err := json.Unmarshal(arguments, &a); err != nil {
		return a, protocol.InvalidCandidate("search requires pattern and optional project-relative path")
	}
	if a.Path == "" {
		a.Path = "."
	}
	var err error
	a.Path, err = projectPath(a.Path)
	if err != nil {
		return a, err
	}
	return a, nil
}
func relativeSearchPath(scope, name string) string {
	if scope == "." {
		return name
	}
	relative := strings.TrimPrefix(name, scope+"/")
	if relative == name && name == scope {
		return path.Base(name)
	}
	return relative
}
func (p globProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	a, err := decodeSearchArgs(request.Arguments)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	match, err := globPattern(a.Pattern)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	result := globResult{Files: []string{}}
	used := 0
	incomplete, err := p.workspace.walk(ctx, a.Path, globDirectories(a.Pattern, a.Path), func(name string, _ bool, _ *os.File) (bool, error) {
		if !match(relativeSearchPath(a.Path, name)) {
			return true, nil
		}
		if used+len(name) > inlineOutputBytes {
			result.Truncated = true
			return false, nil
		}
		result.Files = append(result.Files, name)
		used += len(name)
		if len(result.Files) == searchLimit {
			result.Truncated = true
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return toolFailure(err)
	}
	result.Truncated = result.Truncated || incomplete
	return encodeResult(result, nil)
}
func (p grepProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	a, err := decodeSearchArgs(request.Arguments)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	pattern, err := regexp.Compile(a.Pattern)
	if err != nil {
		return protocol.ToolResult{}, protocol.InvalidCandidate("invalid Go/RE2 regular expression: " + err.Error())
	}
	var include func(string) bool
	var descend func(string) bool
	if a.Include != "" {
		include, err = globPattern(a.Include)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if strings.Contains(a.Include, "/") {
			descend = globDirectories(a.Include, a.Path)
		}
	}
	result := grepResult{Matches: map[string]string{}}
	used, count := 0, 0
	incomplete, err := p.workspace.walk(ctx, a.Path, descend, func(name string, symlink bool, directory *os.File) (bool, error) {
		if symlink {
			return true, nil
		}
		candidate := relativeSearchPath(a.Path, name)
		if a.Include != "" && !strings.Contains(a.Include, "/") {
			candidate = path.Base(name)
		}
		if include != nil && !include(candidate) {
			return true, nil
		}
		var file *os.File
		var err error
		if directory == nil {
			file, _, err = p.workspace.open(ctx, name)
		} else {
			file, _, err = openEntryAt(directory.Fd(), path.Base(name))
		}
		if err != nil {
			return false, err
		}
		if file == nil {
			return true, nil
		}
		defer file.Close()
		keep := true
		var matches strings.Builder
		err = visitLines(ctx, bufio.NewReaderSize(file, 32<<10), lineBytes, true, func(number int, text string, clipped bool) (bool, error) {
			if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
				result.Truncated = true
				return false, nil
			}
			// A clipped line cannot establish the complete regexp match.
			if clipped {
				result.Truncated = true
				return true, nil
			}
			match := pattern.FindStringIndex(text)
			if match == nil {
				return true, nil
			}
			line := fmt.Sprintf("%d: %s\n", number, grepPreview(text, match[0], match[1]))
			// Bound source text and line numbers before JSON encoding.
			size := len(line)
			if matches.Len() == 0 {
				size += len(name)
			}
			if used+size > inlineOutputBytes {
				result.Truncated = true
				keep = false
				return false, nil
			}
			matches.WriteString(line)
			result.Truncated = result.Truncated || len(text) > grepPreviewBytes
			used += size
			count++
			if count == searchLimit {
				result.Truncated = true
				keep = false
			}
			return keep, nil
		})
		if matches.Len() > 0 {
			result.Matches[name] = matches.String()
		}
		return keep, err
	})
	if err != nil {
		return toolFailure(err)
	}
	result.Count = count
	result.Truncated = result.Truncated || incomplete
	return encodeResult(result, nil)
}

type globResult struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated,omitempty"`
}

type grepResult struct {
	Count     int               `json:"count"`
	Matches   map[string]string `json:"matches"`
	Truncated bool              `json:"truncated,omitempty"`
}
