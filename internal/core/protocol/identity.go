// Package protocol declares the contracts and binding stubs shared by core and
// external support. It does not depend on their implementations.
package protocol

import (
	"strconv"
	"strings"
)

// WorkflowID identifies the persistent workflow across demands and process runs.
type WorkflowID string

type Role string

const (
	RolePlanner  Role = "PLANNER"
	RoleReviewer Role = "REVIEWER"
	RoleWorker   Role = "WORKER"
)

// ParseHistoryRef splits a canonical source label and its positive 63-bit number.
func ParseHistoryRef(ref string) (byte, uint64, error) {
	if len(ref) > 1 && strings.IndexByte("PWRU", ref[0]) >= 0 {
		n, err := strconv.ParseUint(ref[1:], 10, 63)
		if err == nil && n > 0 && strconv.FormatUint(n, 10) == ref[1:] {
			return ref[0], n, nil
		}
	}
	return 0, 0, InvalidCommand("invalid numbered history reference")
}

func ValidHistoryRef(ref string) bool {
	_, _, err := ParseHistoryRef(ref)
	return err == nil
}
