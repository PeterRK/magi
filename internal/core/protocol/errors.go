package protocol

import (
	"errors"
	"fmt"
)

type ErrorCode string

func (code ErrorCode) Error() string { return string(code) }

const (
	CodeInvalidCommand    ErrorCode = "INVALID_COMMAND"
	CodeLifecycleConflict ErrorCode = "LIFECYCLE_CONFLICT"
	CodePermissionDenied  ErrorCode = "PERMISSION_DENIED"
	CodeCapacityLimited   ErrorCode = "CAPACITY_LIMITED"
	CodeIntegrityFailure  ErrorCode = "INTEGRITY_FAILURE"
	CodeOutcomeUnknown    ErrorCode = "OUTCOME_UNKNOWN"
)

type Fault struct {
	Code    ErrorCode
	Message string
}

func (e Fault) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Is matches a code target without changing matching against a specific Fault.
func (e Fault) Is(target error) bool {
	code, ok := target.(ErrorCode)
	return ok && e.Code == code
}

func IsCode(err error, code ErrorCode) bool {
	return errors.Is(err, code)
}

func InvalidCommand(message string) error {
	return Fault{Code: CodeInvalidCommand, Message: message}
}

func LifecycleConflict(message string) error {
	return Fault{Code: CodeLifecycleConflict, Message: message}
}

func IntegrityFailure(message string) error {
	return Fault{Code: CodeIntegrityFailure, Message: message}
}

var ErrModelUnavailable = errors.New("model unavailable")

var ErrContextOverflow = errors.New("model context overflow")

// CandidateFeedback explicitly identifies a model-owned validation failure whose
// public correction can cross the role boundary. Err remains the local diagnostic;
// never derive PublicMessage from unknown IDs, source text or an underlying cause.
type CandidateFeedback struct {
	Err           error
	PublicMessage string
}

func (e CandidateFeedback) Error() string { return e.Err.Error() }

func (e CandidateFeedback) Unwrap() error { return e.Err }

func WithCandidateFeedback(err error, publicMessage string) error {
	return CandidateFeedback{Err: err, PublicMessage: publicMessage}
}

// InvalidCandidate is for reviewed constant messages about model-owned fields.
func InvalidCandidate(publicMessage string) error {
	return WithCandidateFeedback(Fault{Code: CodeInvalidCommand, Message: publicMessage}, publicMessage)
}
