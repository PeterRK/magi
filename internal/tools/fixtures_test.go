package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"magi/internal/core/protocol"
)

type toolProvider interface {
	Execute(context.Context, toolExecution) (protocol.ToolResult, error)
}

func execution(root string, arguments any) toolExecution {
	body, _ := json.Marshal(arguments)
	return toolExecution{Arguments: body, ProjectRoot: root, MaxOutputBytes: 4096}
}

// noError reports setup failures at the calling test, preserving Fatal semantics.
func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Fixture for the durable marker format; tools only consumes its recognition API.
func writeTestInitializationMarker(stage string) error {
	body := "magi private initialization v3 " + filepath.Base(stage) + " 0123456789abcdef0123456789abcdef\n"
	return os.WriteFile(filepath.Join(stage, ".magi-private-init-v3"), []byte(body), 0600)
}
