package prompt

import (
	"strings"
	"testing"
)

func TestEmbeddedPromptPackIdentifiesEveryPurpose(t *testing.T) {
	common, err := promptFiles.ReadFile("prompts/common.md")
	if err != nil || strings.TrimSpace(string(common)) == "" {
		t.Fatal("common instructions unavailable", err)
	}
	for _, purpose := range []string{"planner", "review", "worker"} {
		prompt, err := ForPurpose(purpose)
		if err != nil {
			t.Fatal(err)
		}
		specific, err := rolePrompt(purpose)
		if err != nil || strings.TrimSpace(specific) == "" {
			t.Fatal("role instructions unavailable", err)
		}
		if !strings.HasPrefix(prompt, string(common)) || !strings.Contains(prompt, specific) {
			t.Fatalf("%s role prompt does not include its common and specific instructions", purpose)
		}
	}
}

func TestCompactionPromptIsIndependentOfRolePurpose(t *testing.T) {
	instructions, err := ForCompaction()
	if err != nil || strings.TrimSpace(instructions) == "" {
		t.Fatal("compaction instructions unavailable", err)
	}
	if _, err := ForPurpose("unknown"); err == nil {
		t.Fatal("unknown prompt purpose accepted")
	}
}
