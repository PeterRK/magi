package tools

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

type writeProvider struct{}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func textFilePath(path string) (string, error) {
	clean, err := projectPath(path)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return "", protocol.InvalidCandidate("path must name a project-relative file")
	}
	return clean, nil
}

func validFileText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func (writeProvider) Execute(ctx context.Context, request toolExecution) (protocol.ToolResult, error) {
	var args writeArgs
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return protocol.ToolResult{}, protocol.InvalidCandidate("write requires path and content strings")
	}
	path, err := textFilePath(args.Path)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if len(args.Content) > maxTextFileBytes || !validFileText(args.Content) {
		return protocol.ToolResult{}, protocol.InvalidCandidate("content must be UTF-8 without NUL, at most 16 MiB")
	}
	mutation, err := openFileMutation(ctx, request.ProjectRoot)
	if err != nil {
		return toolFailure(err)
	}
	defer mutation.root.Close()
	directory, name, err := mutation.parent(path, true)
	if err != nil {
		return toolFailure(err)
	}
	defer directory.Close()
	before, err := mutation.readAt(directory, name)
	if err != nil {
		return toolFailure(err)
	}
	if err := mutation.publish(directory, name, []byte(args.Content), before); err != nil {
		return toolFailure(err)
	}
	operation := "created"
	if before != nil {
		operation = "updated"
	}
	return encodeResult(struct {
		Path      string `json:"path"`
		Operation string `json:"operation"`
	}{path, operation}, nil)
}
