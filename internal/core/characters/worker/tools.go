package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"magi/internal/core/characters"
	"magi/internal/core/protocol"
)

func modelTools() ([]protocol.Tool, error) {
	return characters.ModelTools("history_read", "finish_job", "write", "edit", "bash", "glob", "grep", "read")
}

func readOnlyTool(name string) bool {
	switch name {
	case "history_read", "read", "glob", "grep":
		return true
	}
	return false
}

// executeBatch settles an ordinary tool batch, overlapping consecutive reads.
// Only this goroutine saves receipts; each read group joins before the next write.
// Cancellation is returned after every unstarted call receives its refusal.
func executeBatch(ctx context.Context, calls []protocol.ValidatedToolCall) error {
	record := characters.CurrentInvocation(ctx)
	for start := 0; start < len(calls); {
		end := start + 1
		parallel := readOnlyTool(calls[start].Call.Function.Name)
		if parallel {
			for end < len(calls) && readOnlyTool(calls[end].Call.Function.Name) {
				end++
			}
		}
		results := make([]struct {
			body []byte
			err  error
		}, end-start)
		run := func(i int) {
			results[i-start].body, results[i-start].err = executeTool(ctx, calls[i])
		}
		if parallel {
			var group sync.WaitGroup
			for i := start; i < end; i++ {
				group.Go(func() { run(i) })
			}
			group.Wait()
		} else {
			run(start)
		}
		var failures []error
		// Preserve every known result in this group, even after cancellation or
		// a sibling's settlement failure. Fatal failures stop subsequent groups.
		for i, result := range results {
			call := calls[start+i]
			err := record.SaveReceipt(call, result.body, result.err)
			if err != nil {
				failures = append(failures, err)
			}
		}
		if err := errors.Join(failures...); err != nil {
			return err
		}
		start = end
	}
	return ctx.Err()
}

func executeTool(ctx context.Context, call protocol.ValidatedToolCall) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return interruptedToolResult(err), nil
	}
	if call.Call.Function.Name == "history_read" {
		body, err := characters.ReadHistory(ctx, call.CanonicalArguments)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return interruptedToolResult(err), nil
		}
		return body, err
	}
	request := protocol.ToolRequest{ID: characters.NextID(), Name: call.Call.Function.Name, Arguments: call.CanonicalArguments}
	result, err := protocol.Tools.Execute(characters.ToolContext(ctx, call.Call.ID), request)
	return characters.ToolOutput(result, err)
}

// Unstarted calls have no external effects. Their receipts complete the model
// batch so a Worker can wrap up after its execution timeout.
func interruptedToolResult(cause error) []byte {
	status := protocol.ToolInterrupted
	if errors.Is(cause, context.DeadlineExceeded) {
		status = protocol.ToolTimedOut
	}
	body, _ := json.Marshal(struct {
		Status     protocol.ToolCallStatus `json:"status"`
		Diagnostic string                  `json:"diagnostic"`
	}{status, cause.Error()})
	return body
}
