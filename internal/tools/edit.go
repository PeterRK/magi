package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"magi/internal/core/protocol"
)

type editProvider struct{}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func (editProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	var args editArgs
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return protocol.ToolResult{}, protocol.InvalidCandidate("edit requires path, old_string and new_string strings, with optional replace_all boolean")
	}
	path, err := textFilePath(args.Path)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	// Match line endings independently of the caller's JSON representation.
	args.OldString = strings.ReplaceAll(args.OldString, "\r\n", "\n")
	args.NewString = strings.ReplaceAll(args.NewString, "\r\n", "\n")
	if args.OldString == "" || args.OldString == args.NewString {
		return protocol.ToolResult{}, protocol.InvalidCandidate("old_string must be nonempty and differ from new_string")
	}
	for _, text := range []string{args.OldString, args.NewString} {
		if len(text) > maxTextFileBytes || !validFileText(text) {
			return protocol.ToolResult{}, protocol.InvalidCandidate("edit strings must be UTF-8 without NUL, at most 16 MiB each")
		}
	}
	mutation, err := openFileMutation(ctx, request.ProjectRoot)
	if err != nil {
		return toolFailure(err)
	}
	defer mutation.root.Close()
	directory, name, err := mutation.parent(path, false)
	if err != nil {
		return toolFailure(err)
	}
	if directory == nil {
		return toolFailure(errors.New("edit target does not exist; use write to create a file"))
	}
	defer directory.Close()
	before, err := mutation.readAt(directory, name)
	if err != nil {
		return toolFailure(err)
	}
	if before == nil {
		return toolFailure(errors.New("edit target does not exist; use write to create a file"))
	}
	after, count, err := replaceText(string(before.body), args)
	if err != nil {
		return toolFailure(err)
	}
	if err := mutation.publish(directory, name, []byte(after), before); err != nil {
		return toolFailure(err)
	}
	return encodeResult(struct {
		Path         string `json:"path"`
		Replacements int    `json:"replacements"`
	}{path, count}, nil)
}

func replaceText(source string, args editArgs) (string, int, error) {
	if !validFileText(source) {
		return "", 0, errors.New("edit target must be UTF-8 without NUL")
	}
	original := source
	crlf := strings.Contains(source, "\r\n")
	if crlf && strings.Count(source, "\n") != strings.Count(source, "\r\n") {
		return "", 0, errors.New("edit target has mixed LF/CRLF line endings; use write or bash for an intentional full rewrite")
	}
	source = strings.ReplaceAll(source, "\r\n", "\n")
	count := strings.Count(source, args.OldString)
	if count == 0 {
		return "", 0, errors.New("old_string not found; reread the relevant file text before retrying")
	}
	if count > 1 && !args.ReplaceAll {
		return "", 0, fmt.Errorf("old_string matches %d times; include more surrounding text for a unique match, or set replace_all true to replace every occurrence", count)
	}
	// Match the original BOM as part of the text, including when old_string
	// was copied verbatim. Restore it only if the prefix replacement removes it.
	bom := ""
	if strings.HasPrefix(source, "\ufeff") && strings.HasPrefix(source, args.OldString) && !strings.HasPrefix(args.NewString, "\ufeff") {
		bom = "\ufeff"
	}
	if len(bom)+len(source)+count*(len(args.NewString)-len(args.OldString)) > maxTextFileBytes {
		return "", 0, errors.New("edit result exceeds 16 MiB")
	}
	after := strings.ReplaceAll(source, args.OldString, args.NewString)
	if crlf {
		if len(bom)+len(after)+strings.Count(after, "\n") > maxTextFileBytes {
			return "", 0, errors.New("edit result exceeds 16 MiB")
		}
		after = strings.ReplaceAll(after, "\n", "\r\n")
	}
	after = bom + after
	if after == original {
		return "", 0, errors.New("edit would not change file content")
	}
	return after, count, nil
}
