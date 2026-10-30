package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"magi/internal/core/characters"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
)

type Planner struct {
	Context *corecontext.Context
}

// Plan contains this round's frozen dispatch.
type Plan struct {
	Orders []characters.WorkOrder
	Memo   *characters.PlannerMemo
}

type Input struct {
	State   Baseline
	Handoff Handoff
}

// Handoff carries immutable accepted role outputs; Planner owns their message order.
type Handoff struct {
	Workers []characters.WorkerReport `json:"worker_reports"`
	Review  *characters.ReviewReport  `json:"review"`
	Entry   string                    `json:"workflow_entry"`
}

func (h Handoff) messages() []protocol.Message {
	messages := make([]protocol.Message, 0, len(h.Workers)+2)
	for _, report := range h.Workers {
		messages = append(messages, report.Message())
	}
	if h.Review != nil {
		messages = append(messages, h.Review.Message())
	}
	if h.Entry != "" {
		messages = append(messages, (characters.WorkflowEntry{Mode: h.Entry}).Message())
	}
	return messages
}

// Baseline identifies the workflow and planning round for execution metadata.
type Baseline struct {
	ID    protocol.WorkflowID
	Round uint64
}

type orderDraft struct {
	Instruction string `json:"instruction"`
}

type planDraft struct {
	Orders []orderDraft `json:"orders"`
	Memo   string       `json:"memo"`
}

// Plan advances the main Context directly. Engine snapshots isolate accepted
// memory; an execution failure ends the run without publishing it.
func (p *Planner) Plan(ctx context.Context, input Input) (Plan, error) {
	key := fmt.Sprintf("planner:%s:%d", input.State.ID, input.State.Round)
	tools, err := characters.ModelTools("history_read", "submit_plan")
	if err != nil {
		return Plan{}, err
	}
	request := characters.InvocationRequest{
		WorkflowID: input.State.ID, Role: protocol.RolePlanner, Purpose: "planner",
		SessionID: "planner:" + string(input.State.ID), CycleKey: key, PromptVersion: prompt.Version,
		Request: protocol.Request{Tools: tools},
	}

	ctx = characters.InvocationContext(ctx, request)
	window := p.Context
	if window == nil {
		instructions, err := prompt.ForPurpose("planner")
		if err != nil {
			return Plan{}, err
		}
		window, err = corecontext.New(characters.RoleContextConfig(protocol.ModelPro, 1), true)
		if err != nil {
			return Plan{}, err
		}
		if err := window.Append(ctx, protocol.Message{Role: "system", Content: instructions}); err != nil {
			return Plan{}, err
		}
	}
	if err := window.TryCompact(ctx); err != nil {
		return Plan{}, err
	}
	for _, message := range input.Handoff.messages() {
		if err := window.Append(ctx, message); err != nil {
			return Plan{}, err
		}
	}
	record := characters.CurrentInvocation(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		_, err := window.CallModel(ctx, request.Request)
		if err != nil {
			return Plan{}, err
		}
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		calls, batchErr := record.Batch()
		if batchErr != nil {
			if err := record.RejectBatch(ctx, window, batchErr); err != nil {
				return Plan{}, err
			}
			continue
		}
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return Plan{}, err
			}
			var plan Plan
			var result []byte
			var callErr error
			switch call.Call.Function.Name {
			case "history_read":
				result, callErr = characters.ReadHistory(ctx, call.CanonicalArguments)
			case "submit_plan":
				plan, callErr = submitPlan(ctx, input.State.ID, call)
				if callErr == nil {
					result = acknowledgment(plan)
				}
			default:
				return Plan{}, protocol.IntegrityFailure("unexpected native Planner control")
			}
			if err := record.SaveReceipt(call, result, callErr); err != nil {
				return Plan{}, err
			}
			if callErr == nil && call.Call.Function.Name == "submit_plan" {
				// Publish accepted memory only after its submission and receipt append.
				if err := record.AppendPending(ctx, window); err != nil {
					return Plan{}, err
				}
				p.Context = window
				return plan, nil
			}
		}
		if err := record.AppendPending(ctx, window); err != nil {
			return Plan{}, err
		}
	}
}

func submitPlan(ctx context.Context, workflow protocol.WorkflowID, call protocol.ValidatedToolCall) (Plan, error) {
	var draft planDraft
	if err := json.Unmarshal(call.CanonicalArguments, &draft); err != nil {
		return Plan{}, err
	}
	if err := validateOrders(draft.Orders); err != nil {
		return Plan{}, err
	}
	if len(draft.Orders) != 0 && strings.TrimSpace(draft.Memo) == "" {
		return Plan{}, protocol.InvalidCandidate("dispatched orders require a nonempty memo explaining dispatch intent")
	}
	plan := Plan{Orders: make([]characters.WorkOrder, len(draft.Orders))}
	for i, item := range draft.Orders {
		ref, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, item.Instruction)
		if err != nil {
			return Plan{}, err
		}
		plan.Orders[i] = characters.WorkOrder{
			WorkflowID: workflow, OrderID: ref,
			Instruction: item.Instruction,
		}
	}
	if strings.TrimSpace(draft.Memo) != "" {
		ref, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, draft.Memo)
		if err != nil {
			return Plan{}, err
		}
		plan.Memo = &characters.PlannerMemo{MemoID: ref, Body: draft.Memo}
	}
	return plan, nil
}

const (
	maxOrdersPerDispatch = 8
	orderNGramSize       = 4
)

// Validate size and overlap before allocating identities or publishing orders.
// Past model responses have hallucinated duplicate orders within one dispatch,
// causing concurrent Workers to repeat work and interfere with each other.
// Check this dispatch, including investigation orders, before publishing any orders;
// reject the whole draft so Planner can resolve overlap and resubmit it.
func validateOrders(orders []orderDraft) error {
	if len(orders) > maxOrdersPerDispatch {
		return protocol.InvalidCandidate(fmt.Sprintf("dispatch permits at most %d orders; reduce the number of orders", maxOrdersPerDispatch))
	}
	if len(orders) < 2 {
		return nil
	}
	instructions := make([]string, len(orders))
	for i, order := range orders {
		text := strings.ReplaceAll(order.Instruction, "\r\n", "\n")
		instructions[i] = strings.TrimSpace(strings.ReplaceAll(text, "\r", "\n"))
		for j := range i {
			if instructions[i] == instructions[j] {
				return protocol.InvalidCandidate(fmt.Sprintf("orders[%d].instruction and orders[%d].instruction are duplicates; merge overlapping work or give each order distinct outcomes, then resubmit the complete plan", j, i))
			}
		}
	}

	const similarityThreshold = 0.8
	sets := make([]map[[orderNGramSize]rune]struct{}, len(orders))
	for i, instruction := range instructions {
		sets[i] = orderNGrams(instruction)
		for j := range i {
			a, b := sets[j], sets[i]
			if len(a) == 0 || len(b) == 0 {
				continue // Short instructions use exact comparison only.
			}
			if len(a) > len(b) {
				a, b = b, a
			}
			intersection := 0
			for key := range a {
				if _, ok := b[key]; ok {
					intersection++
				}
			}
			similarity := float64(intersection) / float64(len(a)+len(b)-intersection)
			if similarity >= similarityThreshold {
				return protocol.InvalidCandidate(fmt.Sprintf("orders[%d].instruction and orders[%d].instruction are near-duplicates (%d-character Jaccard %.4f >= %.2f); merge overlapping work or give each order distinct outcomes, then resubmit the complete plan", j, i, orderNGramSize, similarity, similarityThreshold))
			}
		}
	}
	return nil
}

func orderNGrams(text string) map[[orderNGramSize]rune]struct{} {
	set := make(map[[orderNGramSize]rune]struct{})
	var key [orderNGramSize]rune
	count := 0
	for _, char := range text {
		copy(key[:], key[1:])
		key[len(key)-1] = char
		count++
		if count >= len(key) {
			set[key] = struct{}{}
		}
	}
	return set
}

// acknowledgment projects the validated role result into its model-visible receipt.
func acknowledgment(plan Plan) []byte {
	refs := make([]string, 0, len(plan.Orders))
	for _, order := range plan.Orders {
		refs = append(refs, order.OrderID)
	}
	ref := ""
	if plan.Memo != nil {
		ref = plan.Memo.MemoID
	}
	body, _ := json.Marshal(struct {
		Ref      string   `json:"ref,omitempty"`
		Status   string   `json:"status"`
		OrderIDs []string `json:"order_ids,omitempty"`
	}{ref, "committed", refs})
	return body
}
