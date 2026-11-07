package context

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

func summaryResponse(summary string) protocol.Response {
	reasoning := ""
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, Content: summary}, FinishReason: "stop", Usage: protocol.Usage{InputTokens: 100, OutputTokens: 10}}
}

func summaryInput(t *testing.T, request protocol.Request) []protocol.Message {
	t.Helper()
	instructions, err := prompt.ForCompaction()
	if err != nil {
		t.Fatal(err)
	}
	end := slices.IndexFunc(request.Messages, func(m protocol.Message) bool {
		return m.Role == "user" && strings.HasPrefix(m.Content, instructions) && strings.HasSuffix(m.Content, prompt.CompactionTask)
	})
	if end < 0 {
		t.Fatal("summary request lost its appended instructions")
	}
	return request.Messages[:end]
}

func TestCompactionRequestPreservesOriginalMessagesWithoutChangingSource(t *testing.T) {
	reasoning := "private reasoning"
	messages := []protocol.Message{
		{Role: "system", Content: `{"instructions":"head"}`},
		{Role: "user", Content: `{"ref":"P1","instruction":"first\nsecond\\nthird"}`},
		{Role: "assistant", Content: `{"note":"unchanged"}`, ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "read", Type: "function", Function: protocol.FunctionCall{Name: "read", Arguments: `{"path":"file.go","limit":9007199254740993}`}}}},
		{Role: "tool", ToolCallID: "read", Content: `{"stdout_tail":"first\nsecond\\nthird","exit_code":0}`},
		{Role: "user", Content: `{"ref":"W1","report":"{\"keep\":true}","value":9007199254740993,"nested":{"keep":true},"absent":null,"empty":""}`},
	}
	c := newTestContext(t, messages, testCompactionConfig(2), nil)
	before, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	request, err := c.compactionRequest(2, len(messages), len(messages))
	if err != nil {
		t.Fatal(err)
	}
	if !request.LongOutput || request.Effort != protocol.EffortHigh {
		t.Fatal("maintenance request must use long output and default effort")
	}
	actual := summaryInput(t, request)
	expected := append([]protocol.Message(nil), messages...)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("maintenance request changed selected native messages: %#v", actual)
	}
	after, err := c.Save()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("maintenance encoding changed the original Context", err)
	}
}

func TestAppendCompactsOnlyOldInputAndPreservesCompletePendingGroup(t *testing.T) {
	settledReasoning := "settled reasoning"
	messages := []protocol.Message{
		{Role: "system", Content: "head"},
		{Role: "user", Content: strings.Repeat("old", 700)},
		{Role: "assistant", Content: "settled answer", ReasoningContent: &settledReasoning, ToolCalls: []protocol.ToolCall{{ID: "settled", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}}}},
		{Role: "tool", ToolCallID: "settled", Content: "original source body"},
	}
	publicHistory := append([]protocol.Message(nil), messages...)
	config := CompactionConfig{HeadReservedMessages: 1, HardTokenLimit: 700, SoftTokenLimit: 600}
	calls := 0
	c := newTestContext(t, messages, config, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		material := summaryInput(t, input)
		if len(input.Messages) != len(publicHistory)+1 || !reflect.DeepEqual(material, publicHistory) {
			t.Fatal("summary input changed native history or pairing, or included protected material")
		}
		input.Messages[1].Content = "provider edit"
		if len(input.Tools) != 0 {
			t.Fatal("Full offered tools")
		}
		return summaryResponse("settled history"), nil
	})
	c.recordInputTokens(800)
	parent := c.Clone()
	reasoning := "pending reasoning"
	pending := []protocol.Message{{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "read", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}}}}, {Role: "tool", ToolCallID: "read", Content: "complete receipt"}}
	for _, message := range pending {
		if err := c.Append(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	actual := c.messages
	if calls != 1 || len(actual) != 4 || !reflect.DeepEqual(actual[2:], pending) || !reflect.DeepEqual(actual[0], messages[0]) || len(c.tokenWatermarks) != 0 {
		t.Fatal("compaction split a group, changed head or retained stale usage")
	}
	if !reflect.DeepEqual(parent.messages, messages) || len(parent.tokenWatermarks) != 1 {
		t.Fatal("compaction changed cloned parent")
	}
}

func TestAppendReceiptCompactsHistoryAndKeepsPendingGroup(t *testing.T) {
	reasoning := "pending reasoning"
	group := []protocol.Message{
		{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{
			{ID: "first", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}},
			{ID: "second", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P2"}`}},
		}},
		{Role: "tool", ToolCallID: "first", Content: "first receipt"},
		{Role: "tool", ToolCallID: "second", Content: "second receipt"},
	}
	for _, count := range []int{1, 2} {
		messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: strings.Repeat("old", 700)}}
		messages = append(messages, group[:count]...)
		calls := 0
		c := newTestContext(t, messages, CompactionConfig{SoftTokenLimit: 600, HardTokenLimit: 700, HeadReservedMessages: 1}, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
			calls++
			if !reflect.DeepEqual(summaryInput(t, input), messages[:2]) {
				t.Fatal("pending tool group entered the compaction input")
			}
			return summaryResponse("settled history"), nil
		})
		c.recordInputTokens(800)
		if err := c.Append(context.Background(), group[count]); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !reflect.DeepEqual(c.messages[2:], group[:count+1]) {
			t.Fatal("compaction changed a partially appended tool group")
		}
	}
}

func TestTailBudgetIncludesPendingToolGroup(t *testing.T) {
	messages := []protocol.Message{
		{Role: "user", Content: "older history"},
		{Role: "user", Content: "recent history"},
		{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "read", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}}}},
	}
	c := newTestContext(t, messages, CompactionConfig{SoftTokenLimit: 1000, HardTokenLimit: 1500}, nil)
	spans := messageSpans(c.messages)
	for _, measured := range []bool{false, true} {
		if measured {
			c.tokenWatermarks = []tokenWatermark{{MessageCount: 1, InputTokens: c.messageTokens[0]}}
		}
		for _, shortage := range []int{0, 1} {
			c.compactionConfig.TailReservedTokens = c.messageTokens[1] + c.messageTokens[2] - shortage
			tail, err := c.retainedTail(spans, 0, 2)
			if err != nil || tail != 1+shortage {
				t.Fatal("pending group was not counted in the tail budget", measured, shortage, tail, err)
			}
		}
	}
}

func TestTryCompactUsesSoftLimitAndFailuresAreAtomic(t *testing.T) {
	messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: strings.Repeat("old", 300)}}
	config := CompactionConfig{HeadReservedMessages: 1, HardTokenLimit: 400, SoftTokenLimit: 300}
	failure := errors.New("compaction failed")
	calls := 0
	c := newTestContext(t, messages, config, func(context.Context, protocol.Request) (protocol.Response, error) {
		calls++
		return protocol.Response{}, failure
	})
	c.recordInputTokens(299)
	if err := c.TryCompact(context.Background()); err != nil || calls != 0 {
		t.Fatal("compacted below soft line", err)
	}
	c.recordInputTokens(300)
	before := protocol.CloneMessages(c.messages)
	if err := c.TryCompact(context.Background()); !errors.Is(err, failure) || !reflect.DeepEqual(c.messages, before) {
		t.Fatal("failed proactive compaction changed state", err)
	}
	c.recordInputTokens(400)
	if err := c.Append(context.Background(), protocol.Message{Role: "user", Content: "pending"}); !errors.Is(err, failure) || !reflect.DeepEqual(c.messages, before) {
		t.Fatal("failed forced compaction published pending material", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Append(canceled, protocol.Message{Role: "user", Content: "pending"}); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(c.messages, before) {
		t.Fatal("canceled append changed input", err)
	}
}

type checkpointObservation func(context.Context, string, []byte)

func (checkpointObservation) NotifyReplyRequired() {}

func (checkpointObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (checkpointObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (checkpointObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
func (f checkpointObservation) DumpContext(ctx context.Context, event string, snapshot []byte) {
	f(ctx, event, snapshot)
}

func TestCompactionWithoutFixedHeadPreservesItsFirstSourceMessage(t *testing.T) {
	messages := []protocol.Message{{Role: "user", Content: strings.Repeat("old history", 300)}}
	c := newTestContext(t, messages, CompactionConfig{SoftTokenLimit: 500, HardTokenLimit: 700}, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		if !reflect.DeepEqual(summaryInput(t, input), messages) {
			t.Fatal("compaction instructions replaced the first history message")
		}
		return summaryResponse("settled history"), nil
	})
	previous := protocol.Observer
	t.Cleanup(func() { protocol.Observer = previous })
	checkpoints := 0
	protocol.Observer = checkpointObservation(func(_ context.Context, event string, snapshot []byte) {
		checkpoints++
		var saved contextSnapshot
		if err := json.Unmarshal(snapshot, &saved); err != nil || event != "before_compaction" || !reflect.DeepEqual(saved.Messages, messages) {
			t.Fatal("checkpoint did not preserve the original Context before compression", err)
		}
	})
	if err := c.TryCompact(context.Background()); err != nil || checkpoints != 1 {
		t.Fatal("missing compaction checkpoint", err, checkpoints)
	}
}

func TestSummaryCapacityRetryPreservesOriginalHistoryAndText(t *testing.T) {
	const text = "Outstanding obligation; original acceptance source [[P12]].\nPrior finding [[W3]]: preserve \"exact\" wording. 汉🙂"
	messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: strings.Repeat("x", 3000)}}
	config := CompactionConfig{HeadReservedMessages: 1, SoftTokenLimit: 600, HardTokenLimit: 700}
	calls := 0
	c := newTestContext(t, messages, config, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		if !reflect.DeepEqual(summaryInput(t, input), messages) {
			t.Fatal("repairs discarded original history")
		}
		if calls == 1 {
			return summaryResponse(strings.Repeat("\"", 4000)), nil
		}
		if len(input.Messages) != 4 || input.Messages[3].Role != "system" || !strings.Contains(input.Messages[3].Content, "must remain below 600") || protocol.ValidateMessages(input.Messages) != nil {
			t.Fatal("capacity retry retained the oversized summary or lost its diagnostic")
		}
		return summaryResponse(text), nil
	})
	if err := c.TryCompact(context.Background()); err != nil || calls != 2 {
		t.Fatal("repair did not publish a usable checkpoint", err)
	}
	message := c.messages[1]
	if message.Role != "user" || message.Content != text {
		t.Fatalf("compressed summary lost references or exact text: %s", message.Content)
	}
	calls = 0
	c = newTestContext(t, messages, config, func(context.Context, protocol.Request) (protocol.Response, error) {
		calls++
		if calls == 4 {
			return summaryResponse("finally small enough"), nil
		}
		return summaryResponse(strings.Repeat("x", 4000)), nil
	})
	if err := c.TryCompact(context.Background()); err != nil || calls != 4 || !strings.Contains(c.messages[1].Content, "finally small enough") {
		t.Fatal("Full did not allow its third capacity retry", calls, err)
	}
}

func TestFullCapacityRepairCancellationPreservesSavedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := newTestContext(t, []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: strings.Repeat("x", 3000)}}, CompactionConfig{HeadReservedMessages: 1, SoftTokenLimit: 600, HardTokenLimit: 1000}, func(context.Context, protocol.Request) (protocol.Response, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return summaryResponse(strings.Repeat("x", 4000)), nil
	})
	c.recordInputTokens(800)
	before, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TryCompact(ctx); !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatal("Full failed to respect the caller's cancellation", calls, err)
	}
	after, err := c.Save()
	if err != nil || string(before) != string(after) {
		t.Fatal("failed repairs changed original messages or usage", err)
	}
}

func TestFullCapacityRetriesDiscardRejectedSummariesAndStopAtomically(t *testing.T) {
	calls := 0
	var frozen protocol.Request
	lastFeedback := ""
	c := newTestContext(t, []protocol.Message{{Role: "system", Content: "original head"}, {Role: "user", Content: strings.Repeat("x", 3000)}}, CompactionConfig{HeadReservedMessages: 1, SoftTokenLimit: 600, HardTokenLimit: 1000}, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		calls++
		if calls > 4 {
			t.Fatal("capacity retry quota was reset")
		}
		if calls == 1 {
			frozen = protocol.CloneRequest(input)
		}
		if !reflect.DeepEqual(input.Messages[:len(frozen.Messages)], frozen.Messages) || len(input.Tools) != 0 {
			t.Fatal("capacity retry changed the frozen input")
		}
		if calls > 1 {
			if len(input.Messages) != len(frozen.Messages)+1 || input.Messages[len(frozen.Messages)].Role != "system" || input.Messages[len(frozen.Messages)].Content == lastFeedback || !strings.Contains(input.Messages[len(frozen.Messages)].Content, "must remain below 600") {
				t.Fatal("retry retained rejected output or stale capacity feedback")
			}
			lastFeedback = input.Messages[len(frozen.Messages)].Content
		}
		return summaryResponse(strings.Repeat("large candidate ", 300+calls*10)), nil
	})
	c.recordInputTokens(800)
	before, err := c.Save()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.TryCompact(context.Background()); !protocol.IsCode(err, protocol.CodeCapacityLimited) || calls != 4 {
		t.Fatal("capacity quota must allow four oversized candidates", calls, err)
	}
	after, err := c.Save()
	if err != nil || string(before) != string(after) {
		t.Fatal("capacity exhaustion changed original context or usage", err)
	}
}

func TestTailUsesEarliestFittingRealWatermark(t *testing.T) {
	old := protocol.Message{Role: "user", Content: strings.Repeat("x", 800)}
	messages := []protocol.Message{{Role: "system", Content: "head"}, old, old, old, {Role: "user", Content: "recent"}}
	config := CompactionConfig{HeadReservedMessages: 1, TailReservedTokens: 100, SoftTokenLimit: 1000, HardTokenLimit: 1500}
	c := newTestContext(t, messages, config, func(_ context.Context, input protocol.Request) (protocol.Response, error) {
		if !reflect.DeepEqual(summaryInput(t, input), messages[:3]) {
			t.Fatal("retained recent messages entered the summary input")
		}
		return summaryResponse("summary"), nil
	})
	c.tokenWatermarks = []tokenWatermark{{MessageCount: 2, InputTokens: 800}, {MessageCount: 3, InputTokens: 1030}, {MessageCount: 4, InputTokens: 1050}}
	if err := c.resetTotalTokens(); err != nil {
		t.Fatal(err)
	}
	if err := c.TryCompact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.messages[2:], messages[3:]) || len(c.tokenWatermarks) != 0 {
		t.Fatal("tail did not retain the suffix after real watermark 1030")
	}
}

func TestTailIsContinuousAndPreservesCompleteToolGroups(t *testing.T) {
	reasoning := "recent reasoning"
	pair := []protocol.Message{{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "read", Type: "function", Function: protocol.FunctionCall{Name: "history_read", Arguments: `{"ref":"P1"}`}}}}, {Role: "tool", ToolCallID: "read", Content: "recent receipt"}}
	large := protocol.Message{Role: "user", Content: strings.Repeat("x", 4*65_536)}
	for _, tc := range []struct {
		name       string
		tail, want []protocol.Message
	}{
		{"paired tail", pair, pair},
		{"oversized final message", []protocol.Message{large}, nil},
		{"oversized group blocks older history", append([]protocol.Message{large}, pair...), pair},
		{"interior system stays in suffix", append([]protocol.Message{{Role: "system", Content: "stage instruction"}}, pair...), append([]protocol.Message{{Role: "system", Content: "stage instruction"}}, pair...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: "fixed full order"}, large}
			messages = append(messages, tc.tail...)
			cfg := testCompactionConfig(2)
			cfg.SoftTokenLimit = 100
			cfg.HardTokenLimit = 1 << 20 // Keep fixture initialization below the compaction line.
			c := newTestContext(t, messages, cfg, nil)
			spans := messageSpans(c.messages)
			boundary, err := c.retainedTail(spans, 2, len(spans))
			if err != nil {
				t.Fatal(err)
			}
			got := c.messages[spans[boundary-1].End:]
			if len(got) != len(tc.want) || len(got) > 0 && !reflect.DeepEqual(got, tc.want) {
				t.Fatal("tail selection changed continuity, system position or complete group")
			}
			request, err := c.compactionRequest(2, spans[boundary-1].End, len(messages))
			if err != nil {
				t.Fatal(err)
			}
			input := summaryInput(t, request)
			if !reflect.DeepEqual(input, messages[:spans[boundary-1].End]) {
				t.Fatal("summary input included protected material or changed history")
			}
		})
	}
}

func TestTailAcceptsExactLimitAndCountsReasoning(t *testing.T) {
	message := protocol.Message{Role: "user"}
	base := estimateMessageTokens(message)
	message.Content = strings.Repeat("x", 4*(65_536-base))
	for _, extra := range []string{"", "xxxx"} {
		last := message
		last.Content += extra
		messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: "old"}, last}
		c := newTestContext(t, messages, testCompactionConfig(1), nil)
		spans := messageSpans(c.messages)
		tail, err := c.retainedTail(spans, 1, len(spans))
		if err != nil || (tail == 2) != (extra == "") {
			t.Fatal("exact tail-token boundary", tail, err)
		}
	}
	reasoning := strings.Repeat("r", 4*65_536)
	messages := []protocol.Message{{Role: "system", Content: "head"}, {Role: "user", Content: "old"}, {Role: "assistant", ReasoningContent: &reasoning, Content: "short visible answer"}}
	c := newTestContext(t, messages, testCompactionConfig(1), nil)
	spans := messageSpans(c.messages)
	tail, err := c.retainedTail(spans, 1, len(spans))
	if err != nil || tail != len(spans) {
		t.Fatal("reasoning excluded from tail budget", err)
	}
}

func assistantMessage(calls ...protocol.ToolCall) protocol.Message {
	reasoning := "私人推理\r\n原文"
	return protocol.Message{Role: "assistant", ReasoningContent: &reasoning, Content: "正文", ToolCalls: calls}
}
