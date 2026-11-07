package context

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"magi/internal/core/protocol"
)

func testCompactionConfig(headMessages int) CompactionConfig {
	return CompactionConfig{SoftTokenLimit: 6_000, HardTokenLimit: 7_500, HeadReservedMessages: headMessages, TailReservedTokens: 65_536}
}

// newTestContext seeds fixtures through the public single-message Append path.
func newTestContext(t *testing.T, messages []protocol.Message, config CompactionConfig, modelAPI contextModelFunc) *Context {
	t.Helper()
	previous := protocol.ModelPro
	if modelAPI == nil {
		protocol.ModelPro = nil
	} else {
		protocol.ModelPro = modelAPI
	}
	t.Cleanup(func() { protocol.ModelPro = previous })
	c, err := New(config, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if err := c.Append(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestContextRequiresOrderedCompactionLimits(t *testing.T) {
	valid := CompactionConfig{SoftTokenLimit: 600, HardTokenLimit: 750, HeadReservedMessages: 1, TailReservedTokens: 100}
	c, err := New(valid, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.compactionConfig != valid {
		t.Fatalf("Context changed valid compaction limits: got %+v", c.compactionConfig)
	}
	for _, invalid := range []CompactionConfig{
		{},
		{SoftTokenLimit: 600},
		{HardTokenLimit: 750},
		{SoftTokenLimit: 751, HardTokenLimit: 750},
		{SoftTokenLimit: -1, HardTokenLimit: 750},
		{SoftTokenLimit: 600, HardTokenLimit: -1},
		{SoftTokenLimit: 600, HardTokenLimit: 750, HeadReservedMessages: -1},
		{SoftTokenLimit: 600, HardTokenLimit: 750, TailReservedTokens: -1},
	} {
		if _, err := New(invalid, true); err == nil {
			t.Fatalf("Context accepted invalid compaction limits: %+v", invalid)
		}
	}
	valid.SoftTokenLimit = valid.HardTokenLimit
	if _, err := New(valid, true); err != nil {
		t.Fatal("compaction limits must allow equality", err)
	}
}

func TestNewCreatesEmptyContext(t *testing.T) {
	c, err := New(testCompactionConfig(1), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.messages) != 0 || len(c.messageTokens) != 0 || len(c.tokenWatermarks) != 0 || c.totalTokens != 0 {
		t.Fatal("New created nonempty message or token state")
	}
	snapshot, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(snapshot, true)
	if err != nil {
		t.Fatal(err)
	}
	clone := restored.Clone()
	if err := clone.Append(context.Background(), protocol.Message{Role: "system", Content: "head"}); err != nil {
		t.Fatal(err)
	}
	if len(clone.messages) != 1 || clone.totalTokens != clone.messageTokens[0] || len(restored.messages) != 0 || restored.totalTokens != 0 {
		t.Fatal("empty snapshot/clone could not independently append its first message")
	}
}

func TestContextOwnsMessagesAndCloneIsIndependent(t *testing.T) {
	reasoning := "private"
	messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "assistant", Content: "answer", ReasoningContent: &reasoning}}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	c.recordInputTokens(200)
	// Spare capacity exposes accidental sharing when either branch appends.
	c.messages = append(make([]protocol.Message, 0, 8), c.messages...)
	c.messageTokens = append(make([]int, 0, 8), c.messageTokens...)
	clone, sibling := c.Clone(), c.Clone()
	messages[0].Content, reasoning = "edited", "edited"
	exported := clone.buildRequest(protocol.Request{})
	*exported.Messages[1].ReasoningContent = "provider edit"
	for _, branch := range []struct {
		window *Context
		text   string
	}{{c, "parent"}, {clone, "child"}, {sibling, "second child"}} {
		if err := branch.window.Append(context.Background(), protocol.Message{Role: "user", Content: branch.text}); err != nil {
			t.Fatal(err)
		}
	}
	for _, branch := range []*Context{c, clone, sibling} {
		tokens := estimateMessageTokens(branch.messages[2])
		if branch.messageTokens[2] != tokens || branch.totalTokens != 200+tokens {
			t.Fatal("a branch overwrote another branch's token estimates")
		}
	}
	clone.recordInputTokens(300)
	if c.messages[0].Content != "head" || *clone.messages[1].ReasoningContent != "private" || c.messages[2].Content != "parent" || clone.messages[2].Content != "child" || sibling.messages[2].Content != "second child" || len(c.tokenWatermarks) != 1 {
		t.Fatal("mutable input, projections or clone operations changed owned state")
	}
	if &c.messages[0] == &clone.messages[0] {
		t.Fatal("append failed to isolate slice storage")
	}
}

func TestAppendOwnsMessagesWithoutInterpretingProtocolState(t *testing.T) {
	messages := []protocol.Message{{Role: "system", Content: "head"}}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	reasoning := ""
	open := protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "open", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}}}}
	if err := c.Append(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if len(c.messages) != 2 || c.messages[1].ToolCalls[0].ID != "open" {
		t.Fatal("Append changed or lost model input messages")
	}
	saved, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(saved, true); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
		t.Fatal("restore accepted the unfinished tool group", err)
	}
	if err := c.Append(context.Background(), protocol.Message{Role: "tool", ToolCallID: "open", Content: "settled"}); err != nil {
		t.Fatal(err)
	}
	saved, err = c.Save()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(saved, true)
	if err != nil || !reflect.DeepEqual(restored.messages, c.messages) {
		t.Fatal("closed history did not roundtrip", err)
	}
}

func TestRestoreRejectsBrokenMessageGroupsAndBoundaries(t *testing.T) {
	reasoning := ""
	messages := []protocol.Message{
		{Role: "system", Content: "original fixed instructions"},
		{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{
			{ID: "read", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}},
		}},
		{Role: "tool", ToolCallID: "read", Content: "original result"},
		{Role: "user", Content: "continue"},
	}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	c.tokenWatermarks = []tokenWatermark{{MessageCount: 1, InputTokens: 37}, {MessageCount: 3, InputTokens: 91}}
	body, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(body, true)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := restored.Save()
	if err != nil || string(unchanged) != string(body) {
		t.Fatal("validation changed original messages or cached usage", err)
	}
	for _, tc := range []struct {
		name          string
		breakSnapshot func(*contextSnapshot)
	}{
		{"wrong receipt", func(s *contextSnapshot) { s.Messages[2].ToolCallID = "other" }},
		{"head splits group", func(s *contextSnapshot) { s.CompactionConfig.HeadReservedMessages = 2 }},
		{"head exceeds history", func(s *contextSnapshot) { s.CompactionConfig.HeadReservedMessages = 5 }},
		{"watermark splits group", func(s *contextSnapshot) { s.TokenWatermarks[1].MessageCount = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var saved contextSnapshot
			if err := json.Unmarshal(body, &saved); err != nil {
				t.Fatal(err)
			}
			tc.breakSnapshot(&saved)
			broken, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(broken, true); !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
				t.Fatal("accepted broken saved message history", err)
			}
		})
	}
}

func TestCallModelCapturesUsageWithoutAppendingOutput(t *testing.T) {
	messages := []protocol.Message{{Role: "system", Content: "head"}}
	failure := errors.New("provider failed")
	var tokens int
	var modelErr error
	c := newTestContext(t, messages, testCompactionConfig(1), func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		if input.Messages[0].Content != "head" {
			t.Fatal("provider changed owned input")
		}
		input.Messages[0].Content = "provider edit"
		return protocol.Response{Message: protocol.Message{Role: "assistant", Content: "caller decides"}, Usage: protocol.Usage{InputTokens: tokens, OutputTokens: 50}}, modelErr
	})
	// Request metadata stays outside Context; only its messages are replaced.
	request := protocol.Request{Messages: []protocol.Message{{Role: "user", Content: "ignored caller messages"}}}
	for _, tc := range []struct {
		tokens int
		err    error
	}{{100, nil}, {110, nil}, {0, nil}, {999, failure}} {
		tokens, modelErr = tc.tokens, tc.err
		response, err := c.CallModel(context.Background(), request)
		if !errors.Is(err, tc.err) || response.Message.Content != "caller decides" || len(c.messages) != 1 || c.messages[0].Content != "head" {
			t.Fatal("call changed context state or lost response/error")
		}
	}
	if !reflect.DeepEqual(c.tokenWatermarks, []tokenWatermark{{MessageCount: 1, InputTokens: 110}}) {
		t.Fatalf("duplicate, missing or failed usage corrupted tokenWatermarks: %+v", c.tokenWatermarks)
	}
}

func TestWatermarksUseMeasuredPrefixAndCachedSuffix(t *testing.T) {
	messages := []protocol.Message{{Role: "system", Content: "head"}}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	c.recordInputTokens(100)
	if err := c.Append(context.Background(), protocol.Message{Role: "user", Content: "more"}); err != nil {
		t.Fatal(err)
	}
	if c.totalTokens != 100+c.messageTokens[1] {
		t.Fatalf("estimate=%d", c.totalTokens)
	}
	c.recordInputTokens(200)
	c.recordInputTokens(180)
	if !reflect.DeepEqual(c.tokenWatermarks, []tokenWatermark{{MessageCount: 1, InputTokens: 100}, {MessageCount: 2, InputTokens: 180}}) {
		t.Fatalf("deduplication: %+v", c.tokenWatermarks)
	}
	clone := c.Clone()
	clone.recordInputTokens(80)
	if len(clone.tokenWatermarks) != 1 || !reflect.DeepEqual(c.tokenWatermarks, []tokenWatermark{{MessageCount: 1, InputTokens: 100}, {MessageCount: 2, InputTokens: 180}}) {
		t.Fatal("provider correction corrupted ordering or parent")
	}
}

func TestContextsRetainTierAndUseCurrentGlobalModel(t *testing.T) {
	previousPro, previousFlash := protocol.ModelPro, protocol.ModelFlash
	defer func() { protocol.ModelPro, protocol.ModelFlash = previousPro, previousFlash }()
	for _, pro := range []bool{true, false} {
		protocol.ModelPro, protocol.ModelFlash = nil, nil
		c, err := New(testCompactionConfig(0), pro)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Append(context.Background(), protocol.Message{Role: "user", Content: "input"}); err != nil {
			t.Fatal(err)
		}
		saved, err := c.Save()
		if err != nil {
			t.Fatal(err)
		}
		clone := c.Clone()
		restored, err := Restore(saved, pro)
		if err != nil {
			t.Fatal(err)
		}
		api := func(tier string) contextModelFunc {
			return func(context.Context, protocol.Request) (protocol.Response, error) {
				return protocol.Response{Message: protocol.Message{Content: tier}, Usage: protocol.Usage{InputTokens: 100}}, nil
			}
		}
		// Binding after construction/restoration must reach both contexts.
		protocol.ModelPro, protocol.ModelFlash = api("pro"), api("flash")
		want := "flash"
		if pro {
			want = "pro"
		}
		for _, window := range []*Context{clone, restored} {
			response, err := window.CallModel(context.Background(), protocol.Request{})
			if err != nil || response.Message.Content != want || window.pro != pro {
				t.Fatalf("tier lost: pro=%v response=%+v err=%v", pro, response, err)
			}
		}
		if len(c.tokenWatermarks) != 0 || clone.totalTokens != 100 {
			t.Fatal("model call changed parent usage")
		}
		if pro {
			protocol.ModelPro = nil
		} else {
			protocol.ModelFlash = nil
		}
		if _, err := clone.CallModel(context.Background(), protocol.Request{}); err == nil {
			t.Fatal("missing tier fell back to an old API or the other tier")
		}
		if pro {
			protocol.ModelPro = api("rebound")
		} else {
			protocol.ModelFlash = api("rebound")
		}
		response, err := restored.CallModel(context.Background(), protocol.Request{})
		if err != nil || response.Message.Content != "rebound" {
			t.Fatal("Context retained a captured API", response, err)
		}
	}
}

func TestSnapshotPreservesCachedTokensAndMeasuredSuffix(t *testing.T) {
	messages := []protocol.Message{{Role: "system", Content: "head"}}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	c.recordInputTokens(100)
	if err := c.Append(context.Background(), protocol.Message{Role: "user", Content: "suffix"}); err != nil {
		t.Fatal(err)
	}
	body, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.pro || restored.compactionConfig != c.compactionConfig || !reflect.DeepEqual(restored.messageTokens, c.messageTokens) || !reflect.DeepEqual(restored.tokenWatermarks, c.tokenWatermarks) || restored.totalTokens != 100+c.messageTokens[1] {
		t.Fatal("restore lost cached state or model selection")
	}
	roundtrip, err := restored.Save()
	if err != nil || string(body) != string(roundtrip) {
		t.Fatal("snapshot did not roundtrip", err)
	}
	var saved contextSnapshot
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	saved.CharactersPerToken++
	incompatible, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(incompatible, true); err == nil {
		t.Fatal("restore accepted incompatible estimate constants")
	}
	saved.CharactersPerToken = estimatedCharactersPerToken
	saved.CompactionConfig.SoftTokenLimit = 0
	invalidConfig, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(invalidConfig, true); err == nil {
		t.Fatal("restore accepted missing compaction limit")
	}
	saved.CompactionConfig = c.compactionConfig
	saved.Version = 3
	incompatibleVersion, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(incompatibleVersion, true); err == nil {
		t.Fatal("restore accepted an incompatible snapshot version")
	}
}

func TestMessageEstimatesCountCharacters(t *testing.T) {
	latin := estimateMessageTokens(protocol.Message{Role: "user", Content: "abcd"})
	chinese := estimateMessageTokens(protocol.Message{Role: "user", Content: "中文测试"})
	if latin != chinese {
		t.Fatal("estimation counts bytes rather than Unicode characters")
	}
}

func TestCallModelPropagatesCapacityErrorsWithoutChangingState(t *testing.T) {
	request := protocol.Request{Messages: []protocol.Message{{Role: "user", Content: strings.Repeat("x", 50000)}}}
	failure := protocol.ErrContextOverflow
	calls := 0
	c := newTestContext(t, nil, testCompactionConfig(0), func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		if !reflect.DeepEqual(input, request) {
			t.Fatal("Context changed request metadata or model input")
		}
		return protocol.Response{Usage: protocol.Usage{InputTokens: 999}}, failure
	})
	if err := c.Append(context.Background(), request.Messages[0]); err != nil {
		t.Fatal(err)
	}
	before := c.totalTokens
	if _, err := c.CallModel(context.Background(), request); !errors.Is(err, protocol.ErrContextOverflow) || calls != 1 || c.totalTokens != before || len(c.tokenWatermarks) != 0 || !reflect.DeepEqual(c.messages, request.Messages) {
		t.Fatal("model capacity error was hidden or changed Context state", err)
	}
}

type contextModelFunc func(context.Context, protocol.Request) (protocol.Response, error)

func (f contextModelFunc) Invoke(ctx context.Context, r protocol.Request) (protocol.Response, error) {
	return f(ctx, r)
}
func (contextModelFunc) InputCapacity() int { return 23552 }

func TestRestoreV4ReestimatesEscapedMessagesAndRetainsMeasurements(t *testing.T) {
	c := newTestContext(t, []protocol.Message{{Role: "system", Content: "<head>"}}, testCompactionConfig(1), nil)
	c.recordInputTokens(100)
	if err := c.Append(context.Background(), protocol.Message{Role: "user", Content: "<T> A&B"}); err != nil {
		t.Fatal(err)
	}
	body, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	var saved contextSnapshot
	if err = json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Version = 4
	for i, message := range saved.Messages {
		legacy, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		size := len([]rune(string(legacy)))
		saved.MessageTokens[i] = (size+3)/4 + messageFramingTokens
	}
	body, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.messageTokens, c.messageTokens) || !reflect.DeepEqual(restored.tokenWatermarks, c.tokenWatermarks) || restored.totalTokens != c.totalTokens {
		t.Fatal("legacy estimates or measured usage were not migrated correctly")
	}
}
