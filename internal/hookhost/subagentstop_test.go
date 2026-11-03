package hookhost

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// SubagentStop hooks match on agent_type and get the subagent's id, type,
// transcript path and last message.
func TestSubagentStopPayloadAndMatcher(t *testing.T) {
	fr := &fakeRunner{script: func(RunRequest) scripted { return scripted{exit: 2, stderr: "keep going"} }}
	h := New(Options{Settings: []SettingsLayer{layer(settingspolicy.SourceUser, hooksMap("SubagentStop", "Explore", "check.sh"))}, Runner: fr})

	if r := h.Stop(context.Background(), StopInput{Subagent: true, AgentID: "a1", AgentType: "Plan"}); r != nil || len(fr.reqs) != 0 {
		t.Fatalf("an Explore matcher ran for Plan: %+v", r)
	}
	r := h.Stop(context.Background(), StopInput{Subagent: true, AgentID: "a1", AgentType: "Explore", AgentTranscriptPath: "/t/a1.jsonl", LastAssistantMessage: "report"})
	if !r.Blocked() || r.Reason != "keep going" {
		t.Fatalf("result = %+v", r)
	}
	p := fr.reqs[0].Payload
	if p.HookEventName != hooks.EventSubagentStop || p.AgentID != "a1" || p.AgentType != "Explore" || p.AgentTranscriptPath != "/t/a1.jsonl" ||
		p.LastAssistantMessage != "report" || p.StopHookActive == nil || *p.StopHookActive {
		t.Fatalf("payload = %+v", p)
	}
}

// Consecutive stop blocks are counted per agent: another agent's (or the main
// agent's) stop does not reset a subagent's count.
func TestStopBlockCapPerAgent(t *testing.T) {
	fr := &fakeRunner{script: func(req RunRequest) scripted {
		if req.Payload.HookEventName == hooks.EventStop {
			return scripted{}
		}
		return scripted{exit: 2, stderr: "never"}
	}}
	h := New(Options{
		Settings: []SettingsLayer{
			layer(settingspolicy.SourceUser, hooksMap("SubagentStop", "", "sub.sh")),
			layer(settingspolicy.SourceProject, hooksMap("Stop", "", "main.sh")),
		},
		Runner:       fr,
		StopBlockCap: 2,
	})
	ctx := context.Background()
	for i := range 2 {
		if r := h.Stop(ctx, StopInput{Subagent: true, AgentID: "a", StopHookActive: i > 0}); !r.Blocked() {
			t.Fatalf("block %d overridden early", i)
		}
		h.Stop(ctx, StopInput{}) // the main agent stops in between
		if r := h.Stop(ctx, StopInput{Subagent: true, AgentID: "b"}); !r.Blocked() {
			t.Fatal("agent b's first block overridden")
		}
	}
	if r := h.Stop(ctx, StopInput{Subagent: true, AgentID: "a", StopHookActive: true}); r.Blocked() || r.SystemMessage == "" {
		t.Fatalf("third block of agent a = %+v, want it overridden", r)
	}
}
