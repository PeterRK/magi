package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/characters"
	plannerrole "magi/internal/core/characters/planner"
	corecontext "magi/internal/core/context"
	"magi/internal/core/prompt"
	"magi/internal/core/protocol"
	"magi/internal/storage/mock"
)

type interactionObservation func()

func (f interactionObservation) NotifyReplyRequired()                                   { f() }
func (interactionObservation) BeforeModelRequest(context.Context, string, []byte) error { return nil }
func (interactionObservation) RecordModelRequest(context.Context, *protocol.RequestDiagnostic, *protocol.Response, error) {
}
func (interactionObservation) RecordToolExecution(context.Context, *protocol.ToolDiagnostic) {}
func (interactionObservation) DumpContext(context.Context, string, []byte)                   {}

func TestInteractionAcknowledgmentPreservesReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		fixture := roleFixtureFor(t)
		window, err := corecontext.New(fixture.ContextConfig, true)
		noError(t, err)
		noError(t, window.Append(context.Background(), fixture.Prefix))
		planner := &plannerrole.Planner{Context: window}
		calls := 0
		var shown []string
		replyRequests := 0
		protocol.Observer = interactionObservation(func() { replyRequests++ })
		protocol.ShowUser = func(message string) error {
			if replyRequests != 1 {
				t.Fatal("missing or redundant reply request notification")
			}
			shown = append(shown, message)
			return nil
		}
		model := modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
			calls++
			if request.LongOutput {
				t.Fatal("interaction or summary inherited planning generation tiers")
			}
			switch calls {
			case 1:
				if request.Purpose != "interaction" || len(request.Messages) != 2 || request.Messages[0].Content != fixture.Prefix.Content || messageField(request.Messages[1].Content, "phase") != "TEMPORARY_INTERACTION" {
					t.Fatal("interaction changed its inherited context or phase", request)
				}
				return controlResponse("show_user", showUserArgs{Message: "Clarify", WaitForReply: true}), nil
			case 2:
				last := request.Messages[len(request.Messages)-1]
				if last.Role != "user" || messageField(last.Content, "message") != "clarified" || messageField(last.Content, "ref") != "U1" {
					t.Fatal("dialogue lost the actual user reply", last)
				}
				return controlResponse("show_user", showUserArgs{Message: "Understood", WaitForReply: false}), nil
			case 3:
				if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "[[U1]]") {
					t.Fatal("summary lost user source list")
				}
				users := 0
				for _, message := range request.Messages {
					if message.Role == "tool" && message.Content != `{"status":"shown"}` {
						t.Fatal("display receipt depends on waiting or input", message)
					}
					if message.Role == "user" {
						users++
					}
				}
				if users != 1 {
					t.Fatal("empty closure added user input", users)
				}
				if request.Purpose != "interaction_summary" {
					t.Fatal("interaction did not request a summary")
				}
				return modelResponse(toolCall("invalid", "history_read", `{"ref":"P1"}`)), nil
			case 4:
				last := request.Messages[len(request.Messages)-1]
				if last.Role != "system" || messageField(last.Content, "instruction") != "a complete nonempty text response without tools is required" || len(request.Tools) != 0 {
					t.Fatal("summary did not repair within its own scope", last)
				}
				reasoning := "private summary reasoning"
				return protocol.Response{Message: protocol.Message{Role: "assistant", Content: "clarified requirement", ReasoningContent: &reasoning}, FinishReason: "stop", Usage: protocol.Usage{InputTokens: 19000, OutputTokens: 2}}, nil
			default:
				t.Fatal("terminal summary triggered another call or Full")
				return protocol.Response{}, nil
			}
		})
		bindModel(model)
		before, err := window.Save()
		noError(t, err)
		input := make(chan string, 1)
		input <- "clarified"
		protocol.UserMessages = input
		ended := make(chan struct{})
		close(ended)
		block, err := planner.Interact(context.Background(), "task", ended)
		noError(t, err)
		if block == nil || block.Body != "clarified requirement" || block.InputID == "" || calls != 4 || len(shown) != 2 || replyRequests != 1 {
			t.Fatal("interaction summary or continuation changed", block, calls, shown)
		}
		after, err := window.Save()
		noError(t, err)
		if string(before) == string(after) {
			t.Fatal("completed visible dialogue was not returned to parent Context")
		}

		body, err := readHistory("U1")
		if err != nil || string(body) != "clarified" {
			t.Fatal("user original changed", string(body), err)
		}
		if _, err := readHistory("U2"); err == nil {
			t.Fatal("empty closure created user history")
		}
		bindModel(modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
			want := []string{`{"interaction":"begin"}`, `{"show_user":"Clarify"}`, `{"ref":"U1","message":"clarified"}`, `{"show_user":"Understood"}`, `{"interaction":"end"}`}
			if len(request.Messages) != len(want)+1 {
				t.Fatal("Planner received fork traffic instead of visible dialogue")
			}
			for index, content := range want {
				message := request.Messages[index+1]
				if message.Role != "user" || message.Content != content || message.ReasoningContent != nil || len(message.ToolCalls) != 0 {
					t.Fatal("dialogue projection changed or leaked fork content", message)
				}
			}
			return protocol.Response{}, protocol.ErrModelUnavailable
		}))
		_, err = planner.Plan(context.Background(), plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 2}})
		if !errors.Is(err, protocol.ErrModelUnavailable) {
			t.Fatal("next Plan did not consume the completed dialogue", err)
		}
	})
}

func TestPlannerReturnsOrderPRefsInSubmittedSequence(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	ctx := context.Background()
	_, err := characters.CacheHistoryOutput(ctx, protocol.RolePlanner, "Earlier demand.")
	noError(t, err)
	calls := 0
	bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("Planner acquired an unexpected total deadline")
		}
		calls++
		instructions, err := prompt.ForPurpose("planner")
		noError(t, err)
		if request.Role != protocol.RolePlanner || request.Purpose != "planner" || len(request.Messages) == 0 || request.Messages[0].Role != "system" || request.Messages[0].Content != instructions {
			t.Fatal("planning request lost its role instructions", request)
		}
		if request.LongOutput {
			t.Fatal("planning must use the ordinary output tier")
		}
		if calls == 1 {
			return modelResponse(toolCall("empty-memo", "submit_plan", `{"orders":[{"instruction":"First work."}],"memo":" "}`)), nil
		}
		if calls != 2 || !strings.Contains(request.Messages[len(request.Messages)-1].Content, "nonempty memo") {
			t.Fatal("dispatch without memo was not rejected")
		}
		if _, err := readHistory("P2"); err == nil {
			t.Fatal("rejected dispatch allocated an order ref")
		}
		return modelResponse(toolCall("plan", "submit_plan", `{"orders":[{"instruction":"First work."},{"instruction":"Second work."}],"memo":"Dispatch intent: fulfill the assigned outcomes."}`)), nil
	}))
	planner := &plannerrole.Planner{}
	plan, err := planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
	noError(t, err)
	if len(plan.Orders) != 2 {
		t.Fatal("Planner changed the submitted orders", plan)
	}
	if plan.Memo == nil || plan.Memo.MemoID != "P4" {
		t.Fatal("Planner did not return the numbered memo", plan.Memo)
	}
	memo, err := readHistory(plan.Memo.MemoID)
	noError(t, err)
	if string(memo) != plan.Memo.Body {
		t.Fatal("memo archive differs from submitted body")
	}
	bindModel(modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if request.LongOutput {
			t.Fatal("subsequent planning must use the ordinary output tier")
		}
		if got := request.Messages[len(request.Messages)-1].Content; got != `{"ref":"P4","status":"committed","order_ids":["P2","P3"]}` {
			t.Fatalf("next plan lost order refs: %s", got)
		}
		return protocol.Response{}, protocol.ErrModelUnavailable
	}))
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 2}})
	if !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("next plan did not reach the model", err)
	}
	for i, order := range plan.Orders {
		want := []string{"P2", "P3"}[i]
		if order.OrderID != want || order.Instruction != []string{"First work.", "Second work."}[i] {
			t.Fatalf("P association or order fields lost: %+v", order)
		}
		body, err := readHistory(want)
		if err != nil || string(body) != order.Instruction {
			t.Fatalf("order archive differs from its instruction: body=%q err=%v", body, err)
		}
	}
}

func TestPlannerCompactsBeforePlanAndInteraction(t *testing.T) {
	isolateBindings(t)
	for _, point := range []string{"plan", "interaction"} {
		t.Run(point, func(t *testing.T) {
			roleFixtureFor(t)
			ctx := context.Background()
			window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 100, HardTokenLimit: 1000, HeadReservedMessages: 1}, true)
			noError(t, err)
			noError(t, window.Append(ctx, protocol.Message{Role: "system", Content: "fixed prefix"}))
			if point == "plan" {
				noError(t, window.Append(ctx, protocol.Message{Role: "user", Content: strings.Repeat("old source detail ", 100)}))
			}
			calls := 0
			model := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
				calls++
				if point == "interaction" && calls == 1 {
					if corecontext.IsCompacting(ctx) {
						t.Fatal("post-plan fixture compacted before planning")
					}
					response := modelResponse(toolCall("plan", "submit_plan", `{"orders":[],"memo":""}`))
					response.Usage.InputTokens = 500 // Above soft, below hard.
					return response, nil
				}
				if !corecontext.IsCompacting(ctx) || request.Role != protocol.RolePlanner || request.WorkflowID != "task" {
					t.Fatalf("%s skipped proactive compaction or lost audit metadata: %+v", point, request)
				}
				return protocol.Response{}, protocol.ErrModelUnavailable
			})
			bindModel(model)
			planner := &plannerrole.Planner{Context: window}
			_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
			wantCalls := 1
			if point == "interaction" {
				noError(t, err)
				_, err = planner.Interact(ctx, "task", nil)
				wantCalls = 2
			}
			if !errors.Is(err, protocol.ErrModelUnavailable) || calls != wantCalls {
				t.Fatalf("%s bypassed compaction failure: calls=%d err=%v", point, calls, err)
			}
		})
	}
}

func TestInteractionInheritsCompactedMainMemory(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	ctx := context.Background()
	window, err := corecontext.New(corecontext.CompactionConfig{SoftTokenLimit: 100, HardTokenLimit: 1000, HeadReservedMessages: 1}, true)
	noError(t, err)
	noError(t, window.Append(ctx, protocol.Message{Role: "system", Content: "inherited fixed prefix"}))
	planner := &plannerrole.Planner{Context: window}
	calls := 0
	var inherited []protocol.Message
	var shown string
	protocol.ShowUser = func(message string) error { shown = message; return nil }
	bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		calls++
		switch calls {
		case 1:
			if request.Messages[0].Content != "inherited fixed prefix" {
				t.Fatal("planning replaced the inherited system head")
			}
			response := modelResponse(toolCall("plan", "submit_plan", `{"orders":[],"memo":""}`))
			response.Usage.InputTokens = 500 // Soft threshold only; compact at interaction entry.
			return response, nil
		case 2:
			if !corecontext.IsCompacting(ctx) || request.Purpose != "interaction" {
				t.Fatal("interaction did not first compact the main Context")
			}
			reasoning := ""
			return protocol.Response{Message: protocol.Message{Role: "assistant", Content: "Known planning state.", ReasoningContent: &reasoning}, FinishReason: "stop"}, nil
		case 3:
			if len(request.Messages) != 3 || request.Messages[0].Content != "inherited fixed prefix" || messageField(request.Messages[1].Content, "context_summary") != "Known planning state." || messageField(request.Messages[2].Content, "phase") != "TEMPORARY_INTERACTION" {
				t.Fatal("dialogue did not inherit the compacted main memory", request.Messages)
			}
			inherited = request.Messages[:2]
			return controlResponse("show_user", showUserArgs{Message: "Done"}), nil
		case 4:
			if !reflect.DeepEqual(request.Messages, inherited) {
				t.Fatal("planning lost the main compaction or retained a notification-only interaction")
			}
			return protocol.Response{}, protocol.ErrModelUnavailable
		default:
			t.Fatal("unexpected model call")
			return protocol.Response{}, nil
		}
	}))
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
	noError(t, err)
	ended := make(chan struct{})
	close(ended)
	input, err := planner.Interact(ctx, "task", ended)
	noError(t, err)
	if input != nil || shown != "Done" {
		t.Fatal("notification-only interaction changed its display or produced user input", shown, input)
	}
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 2}})
	if !errors.Is(err, protocol.ErrModelUnavailable) || calls != 4 || planner.Context != window {
		t.Fatal("interaction did not keep the main Context's compacted state", calls, err)
	}
}

func TestInteractionUsageCountsAsPlanner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateBindings(t)
		fixture := roleFixtureFor(t)
		ctx := context.Background()
		window, err := corecontext.New(fixture.ContextConfig, true)
		noError(t, err)
		noError(t, window.Append(ctx, fixture.Prefix))
		response := controlResponse("show_user", showUserArgs{Message: "Working", WaitForReply: false})
		response.Usage = protocol.Usage{InputTokens: 100, OutputTokens: 20}
		hit, miss, reasoning := 30, 70, 12
		response.TokenDetails = &protocol.TokenDetails{CacheHitTokens: &hit, CacheMissTokens: &miss, ReasoningTokens: &reasoning}
		model := modelFunc(func(context.Context, characters.InvocationRequest) (protocol.Response, error) { return response, nil })
		bindModel(model)
		disk := protocol.Storage.(*mock.Storage)
		protocol.UserMessages = nil
		ended := make(chan struct{})
		close(ended)
		_, err = (&plannerrole.Planner{Context: window}).Interact(ctx, "task", ended)
		noError(t, err)
		usage := disk.Usage()
		if len(usage) != 1 || usage["PLANNER"] != (protocol.TokenUsage{Hit: 30, Miss: 70, Output: 20}) {
			t.Fatalf("interaction usage did not belong to Planner: %+v", usage)
		}
	})
}

func TestPlannerSchemaHintKeepsRejectionReceipt(t *testing.T) {
	isolateBindings(t)
	fixture := roleFixtureFor(t)
	ctx := context.Background()
	window, err := corecontext.New(fixture.ContextConfig, true)
	noError(t, err)
	noError(t, window.Append(ctx, fixture.Prefix))
	planner := &plannerrole.Planner{Context: window}
	calls := 0
	model := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		calls++
		if calls == 1 {
			return modelResponse(toolCall("candidate", "submit_plan", `{}`)), nil
		}
		last := request.Messages[len(request.Messages)-1]
		if calls != 2 || last.Role != "tool" || last.ToolCallID != "candidate" || !strings.Contains(last.Content, "$.orders: missing required property") {
			t.Fatal("schema hint was not delivered in the real paired tool receipt")
		}
		return modelResponse(toolCall("candidate", "submit_plan", `{"orders":[],"memo":""}`)), nil
	})
	bindModel(model)
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
	noError(t, err)
	if calls != 2 {
		t.Fatalf("schema repair calls=%d", calls)
	}
	bindModel(modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		if got := request.Messages[len(request.Messages)-1].Content; got != `{"status":"committed"}` {
			t.Fatalf("empty dispatch receipt: %s", got)
		}
		return protocol.Response{}, protocol.ErrModelUnavailable
	}))
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 2}})
	if !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("next plan did not reach the model", err)
	}
}

func TestPlannerHistoryRefusalAppendsReceiptAndContinues(t *testing.T) {
	isolateBindings(t)
	fixture := roleFixtureFor(t)
	ctx := context.Background()
	window, err := corecontext.New(fixture.ContextConfig, true)
	noError(t, err)
	noError(t, window.Append(ctx, fixture.Prefix))
	planner := &plannerrole.Planner{Context: window}
	calls := 0
	model := modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		calls++
		if calls == 1 {
			return modelResponse(toolCall("missing-history", "history_read", `{"ref":"P1"}`)), nil
		}
		last := request.Messages[len(request.Messages)-1]
		if calls != 2 || len(request.Messages) != 3 || request.Messages[0].Content != fixture.Prefix.Content || request.Messages[1].ToolCalls[0].ID != "missing-history" || last.Role != "tool" || last.ToolCallID != "missing-history" {
			t.Fatal("history refusal rewound context or lost the paired call")
		}
		if messageField(last.Content, "error") != "source_unavailable" || messageField(last.Content, "field") != "ref" || messageField(last.Content, "message") == "" {
			t.Fatalf("missing actionable history refusal: %s", last.Content)
		}

		return modelResponse(toolCall("continue", "submit_plan", `{"orders":[],"memo":""}`)), nil
	})
	bindModel(model)
	_, err = planner.Plan(ctx, plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
	noError(t, err)
	if calls != 2 {
		t.Fatalf("history error stopped or restarted the task: calls=%d", calls)
	}
}

// Role fixtures propagate supplied failures without provider policy or retry waits.

type modelFunc func(context.Context, characters.InvocationRequest) (protocol.Response, error)

func (f modelFunc) Invoke(ctx context.Context, input protocol.Request) (protocol.Response, error) {
	r := characters.CurrentInvocation(ctx).Owner()
	r.Request = input
	return f(ctx, r)
}

func modelResponse(calls ...protocol.ToolCall) protocol.Response {
	reasoning := "private chain\r\n原文"
	return protocol.Response{Message: protocol.Message{Role: "assistant", Content: "assistant text", ReasoningContent: &reasoning, ToolCalls: calls}, Usage: protocol.Usage{InputTokens: 5, OutputTokens: 2}, FinishReason: "tool_calls"}
}

func toolCall(id, name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: args}}
}

func (modelFunc) InputCapacity() int { return 23552 }

type roleFixture struct {
	Prefix        protocol.Message
	ContextConfig corecontext.CompactionConfig
}

func roleFixtureFor(t *testing.T) roleFixture {
	t.Helper()
	protocol.Storage = &mock.Storage{}
	protocol.ShowUser = func(string) error { return nil }
	characters.ResetHistory()
	provider := modelFunc(nil)
	return roleFixture{Prefix: protocol.Message{Role: "system", Content: "test private compaction"}, ContextConfig: characters.RoleContextConfig(provider, 1)}
}

// controlResponse constructs a native control response for a typed fixture.
func controlResponse(name string, value any) protocol.Response {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	reasoning := "native offline fixture"
	return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []protocol.ToolCall{{ID: "fixture-" + name, Type: "function", Function: protocol.FunctionCall{Name: name, Arguments: string(body)}}}}, FinishReason: "tool_calls"}
}

// noError reports setup failures at the calling test, preserving Fatal semantics.
func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Global protocol bindings model the single active workspace. These tests must
// stay serial, and any goroutines must finish before test cleanup restores them.
func isolateBindings(t *testing.T) {
	t.Helper()
	pro, flash := protocol.ModelPro, protocol.ModelFlash
	storage, tools := protocol.Storage, protocol.Tools
	userMessages, showUser, observer := protocol.UserMessages, protocol.ShowUser, protocol.Observer
	characters.ResetHistory()
	t.Cleanup(func() {
		protocol.ModelPro, protocol.ModelFlash = pro, flash
		protocol.Storage, protocol.Tools = storage, tools
		protocol.UserMessages, protocol.ShowUser, protocol.Observer = userMessages, showUser, observer
		characters.ResetHistory()
	})
}

func bindModel(model protocol.Model) {
	protocol.ModelPro = characters.ContextModel(model)
	protocol.ModelFlash = nil
}

// Non-JSON instructions and source text have no engine fields.
func messageField(content, name string) string {
	var fields map[string]any
	if json.Unmarshal([]byte(content), &fields) != nil {
		return ""
	}
	value, _ := fields[name].(string)
	return value
}

// A normal background-end notification is only relevant at optional-input
// waiting points. Neither the model call nor an answer the role needs is cut off.
func TestInteractionCompletionPreservesModelAndRequiredInput(t *testing.T) {
	for _, ask := range []bool{false, true} {
		t.Run(fmt.Sprint(ask), func(t *testing.T) {
			isolateBindings(t)
			synctest.Test(t, func(t *testing.T) {
				fixture := roleFixtureFor(t)
				window, err := corecontext.New(fixture.ContextConfig, true)
				noError(t, err)
				noError(t, window.Append(context.Background(), fixture.Prefix))
				ended := make(chan struct{})
				close(ended)
				displays := 0
				input := make(chan string)
				protocol.UserMessages = input
				if ask {
					go func() {
						time.Sleep(20 * time.Second)
						input <- "A necessary answer"
					}()
				}
				bindModel(modelFunc(func(ctx context.Context, request characters.InvocationRequest) (protocol.Response, error) {
					// Background completion predates this model call; the call still finishes.
					time.Sleep(time.Second)
					if err := ctx.Err(); err != nil {
						t.Fatal("background completion canceled the model", err)
					}
					if request.Purpose == "interaction_summary" {
						reasoning := ""
						return protocol.Response{Message: protocol.Message{Role: "assistant", ReasoningContent: &reasoning, Content: "The user answered. [[U1]]"}, FinishReason: "stop"}, nil
					}
					displays++
					return controlResponse("show_user", showUserArgs{Message: "Continue", WaitForReply: ask && displays == 1}), nil
				}))
				summary, err := (&plannerrole.Planner{Context: window}).Interact(context.Background(), "task", ended)
				noError(t, err)
				if ask && (summary == nil || displays != 2) {
					t.Fatal("background end bypassed a required answer", summary, displays)
				}
				if !ask && (summary != nil || displays != 1) {
					t.Fatal("optional wait failed to close without inventing input", summary, displays)
				}
			})
		})
	}
}

// Detailed dispatch limits and similarity cases are checked directly in planner_test.go.
// One role-level case verifies rejection, repair and allocation as one transaction.
func TestPlannerRejectsInvalidDispatchBeforeArchiving(t *testing.T) {
	isolateBindings(t)
	roleFixtureFor(t)
	calls := 0
	bindModel(modelFunc(func(_ context.Context, request characters.InvocationRequest) (protocol.Response, error) {
		calls++
		if calls == 1 {
			return modelResponse(toolCall("duplicate", "submit_plan", `{"orders":[{"instruction":"Implement cleanup"},{"instruction":"Implement cleanup"}],"memo":"Dispatch intent."}`)), nil
		}
		if calls != 2 || !strings.Contains(messageField(request.Messages[len(request.Messages)-1].Content, "message"), "are duplicates") {
			t.Fatal("invalid dispatch did not receive repair feedback")
		}
		if _, err := readHistory("P1"); err == nil {
			t.Fatal("rejected dispatch allocated history")
		}
		return modelResponse(toolCall("repaired", "submit_plan", `{"orders":[{"instruction":"Implement cleanup"}],"memo":"Focused dispatch."}`)), nil
	}))
	plan, err := (&plannerrole.Planner{}).Plan(context.Background(), plannerrole.Input{State: plannerrole.Baseline{ID: "task", Round: 1}})
	noError(t, err)
	if calls != 2 || len(plan.Orders) != 1 || plan.Orders[0].OrderID != "P1" || plan.Memo == nil || plan.Memo.MemoID != "P2" {
		t.Fatal("rejected dispatch consumed references or was partially accepted", plan)
	}
}

// Read through the role-facing boundary; cache representation stays private.
func readHistory(ref string) ([]byte, error) {
	return characters.ReadHistory(context.Background(), []byte(`{"ref":"`+ref+`"}`))
}

// showUserArgs models the public show_user tool parameters.
type showUserArgs struct {
	Message      string `json:"message"`
	WaitForReply bool   `json:"wait_for_reply"`
}
