package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// TestBuildSession_SetsSystemPromptWithMemory: buildSession now sets conv.System
// to the identity + guidance + env block, with the loaded gocode.md
// project memory appended. It also feeds the subagent MemoryFor seam.
func TestBuildSession_SetsSystemPromptWithMemory(t *testing.T) {
	env, _, _, cwd := sessionEnv(t, "")
	const memMark = "REMEMBER-THE-TABS-RULE"
	if err := os.WriteFile(filepath.Join(cwd, "gocode.md"), []byte("# Team rules\n\n"+memMark+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	sys := rt.conv.System
	if sys == "" {
		t.Fatal("conv.System is empty; the main agent still has no system prompt")
	}
	for _, want := range []string{
		"You are gocode, Gocode's official CLI", // identity
		"# Environment",                         // runtime env block
		"Primary working directory: " + cwd,     // env reflects the session cwd
		"Is a git repository:",                  // git state present
		memMark,                                 // loaded project memory
		"# Project memory",                      // memory header
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("conv.System missing %q", want)
		}
	}

	// No telemetry / account leakage sneaks in through the assembled system.
	low := strings.ToLower(sys)
	for _, forbidden := range []string{"telemetry", "oauth", "report the issue"} {
		if strings.Contains(low, forbidden) {
			t.Errorf("conv.System must not contain %q", forbidden)
		}
	}

	// Subagent MemoryFor returns the same project memory.
	if rt.agentTool == nil || rt.agentTool.MemoryFor == nil {
		t.Fatal("subagent MemoryFor was not wired")
	}
	if got := rt.agentTool.MemoryFor(subagent.AgentType{Name: "general-purpose"}); !strings.Contains(got, memMark) {
		t.Errorf("MemoryFor did not return the project memory, got %q", got)
	}
}

// TestBuildSession_NoMemoryStillHasSystem: with no gocode.md, conv.System is
// still the non-empty base prompt and MemoryFor gives a plain agent only the
// scratchpad section (no empty memory section leaks into subagents).
func TestBuildSession_NoMemoryStillHasSystem(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	s := &printSession{Model: "parent-model", Provider: providerOpenRouter}
	rt := buildSession(context.Background(), env, s, fakeCaller{reply: "ok"}, sessionConfig{})
	defer rt.Close("other")

	if !strings.Contains(rt.conv.System, "You are gocode") {
		t.Error("conv.System should carry the identity even with no project memory")
	}
	if strings.Contains(rt.conv.System, "# Project memory") {
		t.Error("memory header should be absent when no gocode.md exists")
	}
	if rt.agentTool != nil && rt.agentTool.MemoryFor != nil {
		got := rt.agentTool.MemoryFor(subagent.AgentType{Name: "general-purpose"})
		if want := scratchpadPromptSection(rt.scratchpad); got != want {
			t.Errorf("MemoryFor with no project memory = %q, want only the scratchpad section %q", got, want)
		}
		if rt.scratchpad == "" {
			t.Error("the session has no scratchpad")
		}
	}
}

// TestBuildSession_SystemReachesProviderRequest: the assembled system prompt now
// flows end to end — turnSystem -> the loop -> the provider request — so the
// request the model receives carries the identity and env block. This guards the
// central concern that the model still works with a real (non-empty) system.
func TestBuildSession_SystemReachesProviderRequest(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")

	if _, err := rt.conv.Submit(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	reqs := sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	for _, want := range []string{"You are gocode", "# Environment"} {
		if !strings.Contains(reqs[0], want) {
			t.Errorf("provider request system missing %q", want)
		}
	}
}
