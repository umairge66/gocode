package planmode

import (
	"strings"
	"testing"
)

func TestEnterPromptForGrepViaBash(t *testing.T) {
	if EnterPromptFor(false) != EnterPrompt {
		t.Fatal("without grep via Bash the prompt is unchanged")
	}
	got := EnterPromptFor(true)
	if !strings.Contains(got, "explore the codebase using Glob, `grep` (via Bash), and Read") || strings.Contains(got, "Grep, and Read") {
		t.Fatalf("prompt not adapted:\n%s", got)
	}
}
