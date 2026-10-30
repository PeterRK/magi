// Package context manages model messages, token watermarks and full compaction.
package context

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"unicode/utf8"

	"magi/internal/core/protocol"
)

// Changing these rules invalidates cached message estimates in saved contexts.
const (
	contextSnapshotVersion      = 4
	estimatedCharactersPerToken = 4
	messageFramingTokens        = 4
)

// tokenWatermark records measured input usage through MessageCount messages.
type tokenWatermark struct {
	MessageCount int `json:"message_count"`
	InputTokens  int `json:"input_tokens"`
}

// estimateMessageTokens includes JSON structure, reasoning and message framing.
// The concrete text envelope always encodes; division leaves room for framing.
func estimateMessageTokens(message protocol.Message) int {
	body, _ := json.Marshal(message)
	size := utf8.RuneCount(body)
	tokens := size/estimatedCharactersPerToken + min(1, size%estimatedCharactersPerToken)
	return tokens + messageFramingTokens
}

// CompactionConfig uses absolute token thresholds: 0 < soft <= hard.
// Soft triggers TryCompact; hard triggers compaction before the next Append.
type CompactionConfig struct {
	SoftTokenLimit       int `json:"soft_token_limit"`
	HardTokenLimit       int `json:"hard_token_limit"`
	HeadReservedMessages int `json:"head_reserved_messages"` // Fixed prefix length; it must end at a complete group.
	TailReservedTokens   int `json:"tail_reserved_tokens"`   // Recent contiguous suffix budget; the head is excluded.
}

// Context owns immutable messages and mutable token accounting. It requires
// New or Restore and is not safe for concurrent mutation. Compaction replaces
// messages atomically and clears measurements for the old sequence.
type Context struct {
	compactionConfig CompactionConfig
	messages         []protocol.Message
	messageTokens    []int // Cached estimates, in the same order as messages.
	// totalTokens = last real watermark + estimates after it; without a watermark,
	// it is the sum of messageTokens. Tool tokens enter only through measured usage.
	totalTokens     int
	tokenWatermarks []tokenWatermark
	pro             bool
}

func validateConfig(config CompactionConfig) error {
	if config.HeadReservedMessages < 0 || config.TailReservedTokens < 0 || config.SoftTokenLimit <= 0 || config.HardTokenLimit <= 0 || config.SoftTokenLimit > config.HardTokenLimit {
		return protocol.InvalidCommand("invalid context compaction configuration")
	}
	return nil
}

// New creates an empty Context. Pro selects the shared Pro API; false selects Flash.
func New(config CompactionConfig, pro bool) (*Context, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Context{compactionConfig: config, pro: pro}, nil
}

// Append owns one message, compacting existing input first if it meets the hard
// threshold. The pending message stays outside compaction; errors leave state unchanged.
func (c *Context) Append(ctx context.Context, message protocol.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tokens := estimateMessageTokens(message)
	next := *c
	if c.totalTokens >= c.compactionConfig.HardTokenLimit {
		if err := next.compact(ctx); err != nil {
			return err
		}
	}
	total, err := addTokens(next.totalTokens, tokens)
	if err != nil {
		return err
	}
	owned := protocol.CloneMessage(message)
	next.messages = append(next.messages, owned)
	next.messageTokens = append(next.messageTokens, tokens)
	next.totalTokens = total
	*c = next
	return nil
}

// Clone inherits the policy and model tier. Clip makes child appends allocate
// while sharing immutable messages and estimates; mutable watermarks are copied.
func (c *Context) Clone() *Context {
	next := *c
	next.messages = slices.Clip(c.messages)
	next.messageTokens = slices.Clip(c.messageTokens)
	next.tokenWatermarks = slices.Clone(c.tokenWatermarks)
	return &next
}

// buildRequest detaches model-visible containers only at the invocation boundary.
func (c *Context) buildRequest(request protocol.Request) protocol.Request {
	request.Messages = nil
	request = protocol.CloneRequest(request)
	request.Messages = protocol.CloneMessages(c.messages)
	return request
}

// recordInputTokens deduplicates boundaries and keeps usage monotone for tail search.
func (c *Context) recordInputTokens(tokens int) {
	if tokens <= 0 || len(c.messages) == 0 {
		return
	}
	points := c.tokenWatermarks
	end := len(points)
	if end > 0 && points[end-1].MessageCount == len(c.messages) {
		points = points[:end-1]
	}
	// A corrected count supersedes contradictory larger counts at older boundaries.
	keep := sort.Search(len(points), func(i int) bool { return points[i].InputTokens > tokens })
	c.tokenWatermarks = append(points[:keep], tokenWatermark{MessageCount: len(c.messages), InputTokens: tokens})
	c.totalTokens = tokens
}

func addTokens(a, b int) (int, error) {
	if b > int(^uint(0)>>1)-a {
		return 0, fmt.Errorf("native token measurement overflows")
	}
	return a + b, nil
}

// resetTotalTokens rebuilds usage from the last watermark and cached estimates.
func (c *Context) resetTotalTokens() error {
	start, total := 0, 0
	if len(c.tokenWatermarks) > 0 {
		last := c.tokenWatermarks[len(c.tokenWatermarks)-1]
		start, total = last.MessageCount, last.InputTokens
	}
	for _, tokens := range c.messageTokens[start:] {
		var err error
		total, err = addTokens(total, tokens)
		if err != nil {
			return err
		}
	}
	c.totalTokens = total
	return nil
}

func (c *Context) modelAPI() protocol.Model {
	if c.pro {
		return protocol.ModelPro
	}
	return protocol.ModelFlash
}

// CallModel replaces request.Messages with detached Context messages and captures
// successful input usage. Other parameters belong to the caller; compatible
// models are assumed. Output is returned unchanged and is not appended.
func (c *Context) CallModel(ctx context.Context, request protocol.Request) (protocol.Response, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	invoke := c.modelAPI()
	if invoke == nil {
		return protocol.Response{}, protocol.InvalidCommand("context model operation is unavailable")
	}
	response, err := invoke.Invoke(ctx, c.buildRequest(request))
	if err == nil {
		c.recordInputTokens(response.Usage.InputTokens)
	}
	return response, err
}

// contextSnapshot includes estimate constants so cached token counts remain interpretable.
type contextSnapshot struct {
	Version              int                `json:"version"`
	CharactersPerToken   int                `json:"characters_per_token"`
	MessageFramingTokens int                `json:"message_framing_tokens"`
	CompactionConfig     CompactionConfig   `json:"config"`
	Messages             []protocol.Message `json:"messages"`
	MessageTokens        []int              `json:"message_tokens"`
	TokenWatermarks      []tokenWatermark   `json:"watermarks"`
}

// Save serializes messages, token caches and policy; the owner supplies the model tier on restore.
func (c *Context) Save() ([]byte, error) {
	if c == nil || len(c.messages) != len(c.messageTokens) {
		return nil, protocol.InvalidCommand("uninitialized context")
	}
	saved := contextSnapshot{Version: contextSnapshotVersion, CharactersPerToken: estimatedCharactersPerToken,
		MessageFramingTokens: messageFramingTokens,
		CompactionConfig:     c.compactionConfig, TokenWatermarks: c.tokenWatermarks, Messages: c.messages, MessageTokens: c.messageTokens}
	return json.Marshal(saved)
}

// Restore checks format and cached usage without calling a model. The owning
// role supplies the model tier; runtime selection is not part of the snapshot.
func Restore(snapshot []byte, pro bool) (*Context, error) {
	var saved contextSnapshot
	if err := protocol.UnmarshalJSON(snapshot, &saved); err != nil {
		return nil, protocol.IntegrityFailure("invalid context snapshot: " + err.Error())
	}
	if saved.Version != contextSnapshotVersion || saved.CharactersPerToken != estimatedCharactersPerToken || saved.MessageFramingTokens != messageFramingTokens {
		return nil, protocol.IntegrityFailure("incompatible context snapshot format or token estimate constants")
	}
	if err := validateConfig(saved.CompactionConfig); err != nil {
		return nil, protocol.IntegrityFailure("invalid saved context settings: " + err.Error())
	}
	if len(saved.MessageTokens) != len(saved.Messages) {
		return nil, protocol.IntegrityFailure("invalid saved estimate positions")
	}
	for _, tokens := range saved.MessageTokens {
		if tokens <= 0 {
			return nil, protocol.IntegrityFailure("invalid saved message estimate")
		}
	}
	// Empty containers may round-trip before their owner installs a fixed head.
	// Nonempty saved histories must be complete; Append itself stays permissive.
	boundaries := map[int]bool{0: true}
	if len(saved.Messages) > 0 {
		if err := protocol.ValidateMessages(saved.Messages); err != nil {
			return nil, protocol.IntegrityFailure("invalid saved message chain: " + err.Error())
		}
		for _, span := range messageSpans(saved.Messages) {
			boundaries[span.End] = true
		}
		if !boundaries[saved.CompactionConfig.HeadReservedMessages] {
			return nil, protocol.IntegrityFailure("saved fixed head splits or exceeds the message chain")
		}
	}
	previous := tokenWatermark{}
	for _, point := range saved.TokenWatermarks {
		if point.MessageCount <= previous.MessageCount || !boundaries[point.MessageCount] || point.InputTokens <= 0 || point.InputTokens < previous.InputTokens {
			return nil, protocol.IntegrityFailure("invalid saved context watermarks")
		}
		previous = point
	}
	c := &Context{compactionConfig: saved.CompactionConfig, pro: pro,
		messages: saved.Messages, messageTokens: saved.MessageTokens, tokenWatermarks: saved.TokenWatermarks}
	if err := c.resetTotalTokens(); err != nil {
		return nil, protocol.IntegrityFailure(err.Error())
	}
	return c, nil
}

// ValidateMemory checks the owning role's fixed head and required history after
// Restore. It compares structure only, never replaces saved instructions.
func (c *Context) ValidateMemory(head int) error {
	if c == nil || head <= 0 || c.compactionConfig.HeadReservedMessages != head || len(c.messages) <= head || c.messages[0].Role != "system" || c.messages[0].Content == "" {
		return protocol.IntegrityFailure("saved role memory lacks its fixed system head or history")
	}
	return nil
}

// DumpContext exports a diagnostic copy only when observation is enabled.
// Diagnostic failures never change the execution or persistence boundary.
func (c *Context) DumpContext(ctx context.Context, event string) {
	observer := protocol.Observer
	if observer == nil {
		return
	}
	snapshot, err := c.Save()
	if err == nil {
		observer.DumpContext(ctx, event, snapshot)
	}
}
