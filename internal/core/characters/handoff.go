package characters

import (
	jsonv2 "encoding/json/v2"

	"magi/internal/core/protocol"
)

type ReviewReport struct {
	ReviewID string `json:"ref"`
	Summary  string `json:"summary"`
}

type WorkOrder struct {
	OrderID     string              `json:"ref"`
	Instruction string              `json:"instruction"`
	WorkflowID  protocol.WorkflowID `json:"-"`
}

// WorkerReport carries the completed execution and its public handoff.
// OrderID identifies the original order by its P fragment.
type WorkerReport struct {
	ReportID string `json:"ref"`
	OrderID  string `json:"order_id"`
	Body     string `json:"report"`
}

type PlannerMemo struct {
	MemoID string `json:"ref"`
	Body   string `json:"memo"`
}

type UserInput struct {
	InputID string `json:"ref"`
	Body    string `json:"user_input"`
}

type WorkflowEntry struct {
	Mode string `json:"workflow_entry"`
}

func (o WorkOrder) Message() protocol.Message     { return message("user", o) }
func (r WorkerReport) Message() protocol.Message  { return message("user", r) }
func (r ReviewReport) Message() protocol.Message  { return message("user", r) }
func (m PlannerMemo) Message() protocol.Message   { return message("user", m) }
func (i UserInput) Message() protocol.Message     { return message("user", i) }
func (e WorkflowEntry) Message() protocol.Message { return message("system", e) }

// Only the concrete string message structs above use this encoder.
func message(role string, value any) protocol.Message {
	body, err := jsonv2.Marshal(value)
	if err != nil {
		panic(err)
	}
	return protocol.Message{Role: role, Content: string(body)}
}
