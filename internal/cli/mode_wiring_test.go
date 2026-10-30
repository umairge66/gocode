package cli

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// TestShiftTabSeamAppliesToDispatcher mirrors the interactive binding: the UI's
// OnCycleMode reports the new mode and the host applies it to the real
// dispatcher. We assert that SetMode changes CurrentMode AND that the mode
// reaches the decider (the decider reads CurrentMode), so tool gating follows
// the shift+tab indicator: plan blocks a Write, acceptEdits allows an Edit
// WITHOUT asking.
func TestShiftTabSeamAppliesToDispatcher(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeDefault)
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{})
	defer rt.Close("other")

	d := rt.dispatcher
	if d.CurrentMode() != toolreg.ModeDefault {
		t.Fatalf("start mode = %q, want default", d.CurrentMode())
	}

	// The host side of OnCycleMode: apply the reported mode to the dispatcher.
	apply := func(m toolreg.Mode) { d.SetMode(m) }

	// plan → a Write (not read-only) is HELD for approval (ask) by the decider,
	// so the interactive prompt can offer to approve it / exit plan mode.
	apply(toolreg.ModePlan)
	if d.CurrentMode() != toolreg.ModePlan {
		t.Fatalf("after apply plan, mode = %q", d.CurrentMode())
	}
	dec := d.Decider.Decide(toolreg.Call{Tool: protocol.ToolWrite}, d.CurrentMode(), d.SnapshotRules(), nil)
	if dec.Behavior != toolreg.BehaviorAsk {
		t.Fatalf("plan mode Write behavior = %q, want ask", dec.Behavior)
	}

	// acceptEdits → an Edit is allowed outright (no ask prompt).
	apply(toolreg.ModeAcceptEdits)
	if d.CurrentMode() != toolreg.ModeAcceptEdits {
		t.Fatalf("after apply acceptEdits, mode = %q", d.CurrentMode())
	}
	dec = d.Decider.Decide(toolreg.Call{Tool: protocol.ToolEdit}, d.CurrentMode(), d.SnapshotRules(), nil)
	if dec.Behavior != toolreg.BehaviorAllow {
		t.Fatalf("acceptEdits Edit behavior = %q, want allow", dec.Behavior)
	}
}

// TestChildDispatcherCarriesPrompterAndMode: a subagent's child dispatcher,
// built from a parent that carries the interactive prompter + asker, also
// carries them (so a subagent "ask" surfaces the main permission dialog) and
// inherits the parent's permission mode (so plan→read-only, auto→auto-run,
// acceptEdits→auto-accept apply to the subagent via the shared decider).
func TestChildDispatcherCarriesPrompterAndMode(t *testing.T) {
	env, _, _, _ := sessionEnv(t, "")
	sp := &scriptedProvider{respond: func(int, string) string { return textReply("ok") }}
	s := openAISession(sp.server(t), toolreg.ModeAcceptEdits)
	caller, err := s.buildCaller()
	if err != nil {
		t.Fatal(err)
	}
	prompter := newDialogPrompter(nil)
	asker := newDialogAsker()
	rt := buildSession(context.Background(), env, s, caller, sessionConfig{
		dispatcherOpts: []toolrun.Option{
			toolrun.WithPermissionPrompter(prompter),
			toolrun.WithAsker(asker),
		},
	})
	defer rt.Close("other")

	if rt.dispatcher.Prompter == nil {
		t.Fatal("parent dispatcher has no prompter")
	}
	child := rt.childDispatcher(rt.agentTool, 1, []string{"*"})
	if child.Prompter == nil {
		t.Fatal("child dispatcher has no prompter (subagent asks would silently deny)")
	}
	if child.Asker == nil {
		t.Fatal("child dispatcher has no asker")
	}
	// A rejected subagent prompt never ends a turn; the main one can.
	if !child.Subagent || rt.dispatcher.Subagent {
		t.Fatalf("Subagent flag: child %v, parent %v; want true, false", child.Subagent, rt.dispatcher.Subagent)
	}
	if child.CurrentMode() != rt.dispatcher.CurrentMode() {
		t.Fatalf("child mode = %q, want inherited %q", child.CurrentMode(), rt.dispatcher.CurrentMode())
	}
	// The inherited mode is meaningful at the decider: acceptEdits auto-allows an
	// Edit in the subagent too.
	dec := child.Decider.Decide(toolreg.Call{Tool: protocol.ToolEdit}, child.CurrentMode(), child.SnapshotRules(), nil)
	if dec.Behavior != toolreg.BehaviorAllow {
		t.Fatalf("subagent acceptEdits Edit behavior = %q, want allow", dec.Behavior)
	}
}
