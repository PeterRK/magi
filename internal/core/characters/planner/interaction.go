package planner

import (
	"context"
	"encoding/json"
	"io"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

type showUserArgs struct {
	Message      string `json:"message"`
	WaitForReply bool   `json:"wait_for_reply"`
}

// dialogueEntry is a visible I/O event, not a copy of the fork's model history.
type dialogueEntry struct {
	ref  string
	text string
}

// interaction owns the disposable dialogue, input waiting and summary state.
type interaction struct {
	window   *corecontext.Context
	record   *characters.Invocation
	dialogue []dialogueEntry
}

// Interact runs the temporary dialogue. Engine only supplies a normal background
// completion signal; it never cancels a model call or a necessary-answer wait.
func (p *Planner) Interact(ctx context.Context, workflow protocol.WorkflowID, ended <-chan struct{}) (*characters.UserInput, error) {
	interaction, err := p.newInteraction(ctx, workflow)
	if err != nil {
		return nil, err
	}
	for {
		ask, err := interaction.show(ctx)
		if err != nil {
			return nil, err
		}
		text, closed, err := waitForReply(ctx, ended, ask)
		if err != nil {
			return nil, err
		}
		if text != "" {
			if err := interaction.recordReply(ctx, text); err != nil {
				return nil, err
			}
		}
		if closed {
			refs := interaction.replyRefs()
			if len(refs) == 0 {
				return nil, nil
			}
			input, err := interaction.summarize(ctx, refs)
			if err != nil {
				return nil, err
			}
			if err := interaction.appendDialogue(ctx, p.Context); err != nil {
				return nil, err
			}
			return input, nil
		}
	}
}

func (p *Planner) newInteraction(ctx context.Context, workflow protocol.WorkflowID) (*interaction, error) {
	tools, err := characters.ModelTools("history_read", "show_user")
	if err != nil {
		return nil, err
	}
	key := characters.NextID()
	request := characters.InvocationRequest{
		WorkflowID: workflow, Role: protocol.RolePlanner, Purpose: "interaction",
		SessionID: "interaction:" + key, CycleKey: key, PromptVersion: prompt.Version,
		Request: protocol.Request{Tools: tools},
	}
	record := characters.NewInvocation(request)
	ctx = record.Context(ctx)
	// Compact the main Context before forking; dialogue itself stays disposable.
	if err := p.Context.TryCompact(ctx); err != nil {
		return nil, err
	}
	interaction := &interaction{window: p.Context.Clone(), record: record}
	instruction, err := prompt.ForPurpose("interaction")
	if err != nil {
		return nil, err
	}
	err = interaction.window.Append(ctx, protocol.Message{Role: "system", Content: prompt.PhaseInstruction("TEMPORARY_INTERACTION", instruction)})
	return interaction, err
}

// show resolves history, displays the next message and settles its tool call.
// Its result only selects input waiting policy, independent of receipt content.
func (i *interaction) show(ctx context.Context) (bool, error) {
	ctx = i.record.Context(ctx)
	request := i.record.Owner().Request
	for {
		_, err := i.window.CallModel(ctx, request)
		if err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		calls, batchErr := i.record.Batch()
		if batchErr != nil {
			if err := i.record.RejectBatch(ctx, i.window, batchErr); err != nil {
				return false, err
			}
			continue
		}
		for _, call := range calls {
			if call.Call.Function.Name == "show_user" {
				var message showUserArgs
				if err := json.Unmarshal(call.CanonicalArguments, &message); err != nil {
					return false, err
				}
				// Notify before output can wake the external input controller.
				if message.WaitForReply && protocol.Observer != nil {
					protocol.Observer.NotifyReplyRequired()
				}
				if err := protocol.ShowUser(message.Message); err != nil {
					return false, err
				}
				if err := i.record.SaveReceipt(call, []byte(`{"status":"shown"}`), nil); err != nil {
					return false, err
				}
				if err := i.record.AppendPending(ctx, i.window); err != nil {
					return false, err
				}
				i.dialogue = append(i.dialogue, dialogueEntry{text: message.Message})
				return message.WaitForReply, nil
			}
			outcome, callErr := characters.ReadHistory(ctx, call.CanonicalArguments)
			err := i.record.SaveReceipt(call, outcome, callErr)
			if err != nil {
				return false, err
			}
		}
		if err := i.record.AppendPending(ctx, i.window); err != nil {
			return false, err
		}
	}
}

// recordReply archives the actual user response and adds its source to the dialogue.
func (i *interaction) recordReply(ctx context.Context, text string) error {
	ctx = i.record.Context(ctx)
	ref, err := characters.CacheUserReply(ctx, text)
	if err != nil {
		return err
	}
	entry := dialogueEntry{ref: ref, text: text}
	if err := i.window.Append(ctx, entry.message()); err != nil {
		return err
	}
	i.dialogue = append(i.dialogue, entry)
	return nil
}

func (i *interaction) replyRefs() []string {
	var refs []string
	for _, entry := range i.dialogue {
		if entry.ref != "" {
			refs = append(refs, entry.ref)
		}
	}
	return refs
}

func (entry dialogueEntry) message() protocol.Message {
	var body []byte
	if entry.ref == "" {
		body, _ = json.Marshal(struct {
			ShowUser string `json:"show_user"`
		}{entry.text})
	} else {
		body, _ = json.Marshal(struct {
			Ref     string `json:"ref"`
			Message string `json:"message"`
		}{entry.ref, entry.text})
	}
	return protocol.Message{Role: "user", Content: string(body)}
}

// summarize archives the accepted user input summary for Reviewer.
func (i *interaction) summarize(ctx context.Context, refs []string) (*characters.UserInput, error) {
	owner := i.record.Owner()
	owner.Purpose = "interaction_summary"
	owner.Request.Tools = []protocol.Tool{}
	owner.CycleKey = characters.NextID()
	ctx = characters.InvocationContext(ctx, owner)
	instruction, err := prompt.ForPurpose("interaction_summary")
	if err != nil {
		return nil, err
	}
	if err := i.window.Append(ctx, protocol.Message{Role: "system", Content: prompt.PhaseInstruction("TEMPORARY_USER_INPUT_SUMMARY", instruction+prompt.UserReplySources(refs))}); err != nil {
		return nil, err
	}
	record := characters.CurrentInvocation(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := i.window.CallModel(ctx, owner.Request)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, batchErr := record.Batch(); batchErr != nil {
			if err := record.RejectBatch(ctx, i.window, batchErr); err != nil {
				return nil, err
			}
			continue
		}
		ref, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, response.Message.Content)
		if err != nil {
			return nil, err
		}
		return &characters.UserInput{InputID: ref, Body: response.Message.Content}, nil
	}
}

// appendDialogue transfers only observed dialogue, never fork instructions, tool
// traffic, private reasoning or its derived user_input. Markers are data, not modes.
func (i *interaction) appendDialogue(ctx context.Context, target *corecontext.Context) error {
	ctx = i.record.Context(ctx)
	if err := target.Append(ctx, protocol.Message{Role: "user", Content: `{"interaction":"begin"}`}); err != nil {
		return err
	}
	for _, entry := range i.dialogue {
		if err := target.Append(ctx, entry.message()); err != nil {
			return err
		}
	}
	return target.Append(ctx, protocol.Message{Role: "user", Content: `{"interaction":"end"}`})
}

// Planner selects whether normal completion may end the message wait.
// Input capture continues independently; complete queued messages take priority.
func waitForReply(ctx context.Context, ended <-chan struct{}, ask bool) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if ask {
		ended = nil
	}
	receive := func(text string, ok bool) (string, bool, error) {
		if !ok {
			return "", false, io.EOF
		}
		return text, false, nil
	}
	select {
	case text, ok := <-protocol.UserMessages:
		return receive(text, ok)
	default:
	}
	select {
	case text, ok := <-protocol.UserMessages:
		return receive(text, ok)
	case <-ctx.Done():
		return "", false, ctx.Err()
	case <-ended:
		select {
		case text, ok := <-protocol.UserMessages:
			return receive(text, ok)
		default:
			return "", true, nil
		}
	}
}
