package characters

import (
	"context"
	"encoding/json"
	"sync"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

type historyReadArgs struct {
	Ref string `json:"ref"`
}

// The process shares current-round formal outputs. Older files are read directly.
var historyFragments = map[string]string{}
var historyMu sync.RWMutex

func ResetHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()
	historyFragments = map[string]string{}
}

// CacheHistoryOutput assigns a fresh immutable reference. The producing role
// registers each accepted output once; execution errors end the run.
func CacheHistoryOutput(ctx context.Context, role protocol.Role, body string) (string, error) {
	return cacheHistory(ctx, string(role), body)
}

// CacheUserReply archives exactly one received reply, without dialogue context.
// User is a history source, not an execution or token-accounting role.
func CacheUserReply(ctx context.Context, text string) (string, error) {
	return cacheHistory(ctx, "USER", text)
}

func cacheHistory(ctx context.Context, source, body string) (string, error) {
	historyMu.Lock()
	defer historyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref, err := protocol.Storage.Allocate(source)
	if err != nil {
		return "", err
	}
	historyFragments[ref] = body
	return ref, nil
}
func resolveHistory(ctx context.Context, ref string) ([]byte, error) {
	if !protocol.ValidHistoryRef(ref) {
		return nil, protocol.ToolCallError{Code: protocol.CodePermissionDenied, Detail: "ACCESS_DENIED", Message: "history reference is unavailable"}
	}
	if body, found := cachedHistory(ref); found {
		return []byte(body), nil
	}
	body, err := protocol.Storage.ReadFragment(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

// Reserve all allocated numbers before writing originals. Failed publication
// may leave durable orphans; their numbers are never reused. Flush once per
// round: the engine clears the cache after publication or abandons a failed run.
func FlushHistory(ctx context.Context, disk protocol.StorageBackend) error {
	historyMu.Lock()
	defer historyMu.Unlock()
	if err := disk.ReserveNumbers(ctx); err != nil {
		return err
	}
	for ref, body := range historyFragments {
		if err := disk.WriteFragment(ctx, ref, []byte(body)); err != nil {
			return err
		}
	}
	return nil
}

func cachedHistory(ref string) (string, bool) {
	historyMu.RLock()
	defer historyMu.RUnlock()
	body, found := historyFragments[ref]
	return body, found
}

// ReadHistory receives schema-validated, canonical history_read arguments.
func ReadHistory(ctx context.Context, arguments []byte) ([]byte, error) {
	var args historyReadArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := resolveHistory(ctx, args.Ref)
	if err != nil {
		if protocol.IsCode(err, protocol.CodeInvalidCommand) {
			return nil, protocol.ToolCallError{Code: protocol.CodeInvalidCommand, Detail: "SOURCE_UNAVAILABLE", Field: "ref", Message: err.Error()}
		}
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, protocol.IntegrityFailure("history source is not UTF-8 text")
	}
	return body, nil
}
