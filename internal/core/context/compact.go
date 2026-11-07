package context

import (
	"cmp"
	"context"
	"fmt"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
	"slices"
	"sort"
)

// A span covers whole messages. An assistant and all its receipts are indivisible.
type messageSpan struct{ Start, End int }

type compactionKey struct{}

// IsCompacting identifies a private maintenance call for role response settlement.
// It is not part of the provider request or the saved Context.
func IsCompacting(ctx context.Context) bool {
	active, _ := ctx.Value(compactionKey{}).(bool)
	return active
}

func messageSpans(messages []protocol.Message) []messageSpan {
	var spans []messageSpan
	for start := 0; start < len(messages); {
		end := start + 1
		if messages[start].Role == "assistant" {
			for end < len(messages) && messages[end].Role == "tool" {
				end++
			}
		}
		spans = append(spans, messageSpan{start, end})
		start = end
	}
	return spans
}

// TryCompact compacts atomically when usage reaches the soft threshold.
func (c *Context) TryCompact(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.totalTokens < c.compactionConfig.SoftTokenLimit {
		return nil
	}
	return c.compact(ctx)
}

// compact fixes the retained boundaries before summarizing and publishing.
func (c *Context) compact(ctx context.Context) error {
	spans := messageSpans(c.messages)
	// An unfinished final tool group is retained outside the compaction input.
	end, sourceEnd := len(spans), len(c.messages)
	if end > 0 {
		last := spans[end-1]
		message := c.messages[last.Start]
		if message.Role == "assistant" && len(message.ToolCalls) > last.End-last.Start-1 {
			end--
			sourceEnd = last.Start
		}
	}
	head := c.compactionConfig.HeadReservedMessages
	if head > len(c.messages) {
		return protocol.IntegrityFailure("invalid fixed head message count")
	}
	first, _ := slices.BinarySearchFunc(spans[:end], head, func(s messageSpan, head int) int { return cmp.Compare(s.Start, head) })
	if first == end {
		return protocol.Fault{Code: protocol.CodeCapacityLimited, Message: "no closed history remains outside fixed head"}
	}
	if spans[first].Start != head {
		return protocol.IntegrityFailure("fixed head splits a native message group")
	}
	tail, err := c.retainedTail(spans, first, end)
	if err != nil {
		return err
	}
	c.DumpContext(ctx, "before_compaction")
	tailStart := spans[tail-1].End
	next, err := c.summarize(ctx, head, tailStart, sourceEnd)
	if err != nil {
		return err
	}
	*c = next
	return nil
}

// compactionRequest replays the original prefix and appends a private summary task.
func (c *Context) compactionRequest(head, tailStart, sourceEnd int) (protocol.Request, error) {
	instructions, err := prompt.ForCompaction()
	if err != nil {
		return protocol.Request{}, err
	}
	ceiling := min(c.totalTokens, c.compactionConfig.SoftTokenLimit)
	instruction := instructions + prompt.CompactionBoundaries(head, sourceEnd-tailStart) + prompt.FullCapacity(ceiling, ceiling/4) + "\n\n" + prompt.CompactionTask
	messages := append(slices.Clone(c.messages[:tailStart]), protocol.Message{Role: "user", Content: instruction})
	return protocol.Request{
		Messages:   messages,
		Tools:      []protocol.Tool{},
		LongOutput: true,
	}, nil
}

// summarize retries oversized checkpoints from frozen input without publishing them.
func (c *Context) summarize(ctx context.Context, head, tailStart, sourceEnd int) (Context, error) {
	invoke := c.modelAPI()
	if invoke == nil {
		return Context{}, protocol.InvalidCommand("context model operation is unavailable")
	}
	request, err := c.compactionRequest(head, tailStart, sourceEnd)
	if err != nil {
		return Context{}, err
	}
	ceiling := min(c.totalTokens, c.compactionConfig.SoftTokenLimit)
	ctx = context.WithValue(ctx, compactionKey{}, true)
	// Capacity retries restart from the frozen source, never accumulating rejected
	// summaries or previous feedback.
	const maxCapacityRetries = 3
	source := slices.Clone(request.Messages)
	capacityRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return Context{}, err
		}
		response, err := invoke.Invoke(ctx, protocol.CloneRequest(request))
		if err != nil {
			return Context{}, err
		}
		summary := summaryMessage(response.Message.Content)
		candidate, err := c.compacted(summary, head, tailStart)
		if err == nil {
			return candidate, nil
		}
		if !protocol.IsCode(err, protocol.CodeCapacityLimited) {
			return Context{}, err
		}
		if capacityRetries == maxCapacityRetries {
			return Context{}, fmt.Errorf("context compaction capacity retry limit exhausted: %w", err)
		}
		capacityRetries++
		feedback := protocol.Message{Role: "system", Content: prompt.FullCapacityRepair(candidate.totalTokens, ceiling)}
		request.Messages = append(slices.Clone(source), feedback)
	}
}

func summaryMessage(summary string) protocol.Message {
	return protocol.Message{Role: "user", Content: summary}
}

func (c *Context) compacted(summary protocol.Message, head, tailStart int) (Context, error) {
	tokens := estimateMessageTokens(summary)
	next := *c
	next.messages = append([]protocol.Message(nil), c.messages[:head]...)
	next.messageTokens = append([]int(nil), c.messageTokens[:head]...)
	next.tokenWatermarks = nil
	next.messages = append(next.messages, summary)
	next.messageTokens = append(next.messageTokens, tokens)
	next.messages = append(next.messages, c.messages[tailStart:]...)
	next.messageTokens = append(next.messageTokens, c.messageTokens[tailStart:]...)
	if err := next.resetTotalTokens(); err != nil {
		return Context{}, err
	}
	before := c.totalTokens
	after := next.totalTokens
	if after >= c.compactionConfig.SoftTokenLimit || after >= before {
		return next, protocol.Fault{Code: protocol.CodeCapacityLimited, Message: fmt.Sprintf("full compaction must shrink the input and leave room below its proactive threshold (input=%d threshold=%d source=%d)", after, c.compactionConfig.SoftTokenLimit, before)}
	}
	return next, nil
}

// retainedTail uses real watermarks before estimates, preserving whole groups.
// The pending group consumes tail budget; one old group remains for summarizing.
func (c *Context) retainedTail(spans []messageSpan, first, end int) (int, error) {
	limit := c.compactionConfig.TailReservedTokens
	total := c.totalTokens
	tail, kept := end, 0
	for _, tokens := range c.messageTokens[spans[end-1].End:] {
		var err error
		kept, err = addTokens(kept, tokens)
		if err != nil {
			return 0, err
		}
	}
	lo := sort.Search(len(c.tokenWatermarks), func(i int) bool { return c.tokenWatermarks[i].MessageCount >= spans[first].End })
	hi := sort.Search(len(c.tokenWatermarks), func(i int) bool { return c.tokenWatermarks[i].MessageCount > spans[end-1].End })
	offset, _ := slices.BinarySearchFunc(c.tokenWatermarks[lo:hi], total-limit, func(w tokenWatermark, minimum int) int { return cmp.Compare(w.InputTokens, minimum) })
	i := lo + offset
	if i < hi {
		w := c.tokenWatermarks[i]
		boundary, _ := slices.BinarySearchFunc(spans[:end], w.MessageCount, func(s messageSpan, count int) int { return cmp.Compare(s.End, count) })
		if spans[boundary].End != w.MessageCount {
			return 0, protocol.IntegrityFailure("native token watermark splits a message group")
		}
		tail, kept = boundary+1, total-w.InputTokens
	}
	for tail > first+1 {
		span := spans[tail-1]
		tokens := 0
		for _, estimate := range c.messageTokens[span.Start:span.End] {
			var err error
			tokens, err = addTokens(tokens, estimate)
			if err != nil {
				return 0, err
			}
		}
		if tokens > limit-kept {
			break
		}
		kept += tokens
		tail--
	}
	return tail, nil
}
