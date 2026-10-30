package conversation

import (
	"context"
	"errors"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

type fakePrompts struct {
	gotName, gotArgs string
	err              error
}

func (f *fakePrompts) ExpandPromptCommand(_ context.Context, name, args string) ([]agent.ContentBlock, bool, error) {
	if name != "mcp__srv__review" {
		return nil, false, nil
	}
	f.gotName, f.gotArgs = name, args
	if f.err != nil {
		return nil, true, f.err
	}
	return []agent.ContentBlock{agent.TextBlock("review " + args), agent.TextBlock("[Resource link: spec] x")}, true, nil
}

func TestMCPPromptCommandBecomesUserTurn(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	fp := &fakePrompts{}
	c.MCPPrompts = fp
	res, err := c.Submit(context.Background(), "/mcp__srv__review a.go bugs")
	if err != nil {
		t.Fatal(err)
	}
	if fp.gotArgs != "a.go bugs" || res.Kind != ResultModel {
		t.Fatalf("args=%q kind=%v", fp.gotArgs, res.Kind)
	}
	first := res.History[0]
	if first.Role != agent.RoleUser || len(first.Content) != 2 || first.Content[0].Text != "review a.go bugs" {
		t.Fatalf("user turn = %+v", first)
	}

	// An unknown mcp__ name falls through to the custom-command path (no
	// registry here: passed through as plain text).
	res, err = c.Submit(context.Background(), "/mcp__other__x hi")
	if err != nil || fp.gotName != "mcp__srv__review" {
		t.Fatalf("fallthrough: %v", err)
	}

	// A prompt error aborts the command without a model call.
	fp.err = errors.New("Missing required argument: file. Usage: /mcp__srv__review file")
	n := len(c.History)
	if _, err := c.Submit(context.Background(), "/mcp__srv__review"); err == nil || len(c.History) != n {
		t.Fatalf("err=%v history grew=%v", err, len(c.History) != n)
	}
}
