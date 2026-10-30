package toolrun

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/inbox"
	"github.com/umairge66/gocode/internal/toolreg"
)

func peerTools() (*inbox.SendMessageTool, *inbox.ListAgentsTool) {
	pm := &inbox.PeerMessaging{Enabled: true, QueueMain: func(inbox.AgentMessage) {}}
	return &inbox.SendMessageTool{P: pm, HasListTool: true}, &inbox.ListAgentsTool{P: pm, CallerCanSend: true}
}

// SendMessage and ListAgents are advertised with their schemas once
// registered, and removed again with nil.
func TestPeerToolsRegisteredAndAdvertised(t *testing.T) {
	send, list := peerTools()
	d := New(toolreg.ModeDefault, nil, WithPeerTools(send, list))
	names := map[string]string{}
	for _, s := range d.Schemas() {
		names[s.Name] = s.Description
	}
	if !strings.HasPrefix(names["SendMessage"], "# SendMessage") || !strings.HasPrefix(names["ListAgents"], "Lists agents you can SendMessage to") {
		t.Fatalf("schemas: %v", names)
	}
	d.SetPeerTools(nil, nil)
	for _, s := range d.Schemas() {
		if s.Name == "SendMessage" || s.Name == "ListAgents" {
			t.Fatalf("%s still advertised after removal", s.Name)
		}
	}
}

// The tools go through the normal permission pipeline: allowed by their own
// check in default mode, denied by a settings deny rule, and a validation
// failure is an is_error result.
func TestPeerToolsPermission(t *testing.T) {
	send, list := peerTools()
	d := New(toolreg.ModeDefault, nil, WithPeerTools(send, list))
	ctx := context.Background()

	res, err := d.Dispatch(ctx, call("ListAgents", `{}`))
	if err != nil || res.IsError || res.Content != "No reachable agents." {
		t.Fatalf("ListAgents: %+v %v", res, err)
	}
	res, _ = d.Dispatch(ctx, call("SendMessage", `{"to":"main","message":"hi"}`))
	if res.IsError || !strings.Contains(res.Content, `"success":false`) {
		t.Fatalf("SendMessage main-from-main: %+v", res)
	}
	res, _ = d.Dispatch(ctx, call("SendMessage", `{"to":"*","message":"hi"}`))
	if !res.IsError || !strings.Contains(res.Content, "broadcast") {
		t.Fatalf("validation error must be is_error: %+v", res)
	}

	dd := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "SendMessage"}}, WithPeerTools(send, list))
	if res, _ := dd.Dispatch(ctx, call("SendMessage", `{"to":"x","message":"hi"}`)); !res.IsError {
		t.Fatalf("a deny rule must deny SendMessage: %+v", res)
	}
	// An ask rule with no prompter (headless) does not send.
	da := New(toolreg.ModeDefault, toolreg.RuleSet{{Behavior: toolreg.RuleAsk, ToolName: "SendMessage"}}, WithPeerTools(send, list))
	if res, _ := da.Dispatch(ctx, call("SendMessage", `{"to":"x","message":"hi"}`)); !res.IsError {
		t.Fatalf("an ask rule without a prompter must not send: %+v", res)
	}
}
