package characters

import (
	corecontext "magi/internal/core/context"
	"magi/internal/core/protocol"
)

// Error stop-loss is shared by all roles and their temporary operations.
// Worker owns its execution deadline and wrap-up separately.
const (
	maxConsecutiveErrors = 5
)

// RoleContextConfig resolves the role policy to absolute token counts before
// constructing a Context. Context receives no percentages or implicit defaults.
func RoleContextConfig(model protocol.Model, headMessages int) corecontext.CompactionConfig {
	available := model.InputCapacity()
	return corecontext.CompactionConfig{
		SoftTokenLimit:       roleTokenThreshold(available, 60),
		HardTokenLimit:       roleTokenThreshold(available, 75),
		HeadReservedMessages: headMessages,
		TailReservedTokens:   65_536,
	}
}

func roleTokenThreshold(available, percent int) int {
	n := available/100*percent + available%100*percent/100
	if available%100*percent%100 != 0 {
		n++
	}
	return n
}
