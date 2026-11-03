package conversation

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/toolreg"
)

// SubmitContent sends attached images in the user turn, after the text.
func TestSubmitContentCarriesImages(t *testing.T) {
	model := &fakeModel{reply: "a cat"}
	c := New(model, fakeDispatcher{})
	res, err := c.SubmitContent(context.Background(), []agent.ContentBlock{
		agent.TextBlock("what is this?"), agent.ImageBlock("image/png", "QUJD"),
	})
	if err != nil || res.AssistantText != "a cat" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	user := model.lastReq.Messages[0]
	if len(user.Content) < 2 || user.Content[0].Text != "what is this?" || user.Content[1].Type != agent.BlockImage ||
		user.Content[1].Source == nil || user.Content[1].Source.Data != "QUJD" {
		t.Fatalf("user turn = %+v", user.Content)
	}
	// Text alone takes the Submit path.
	if _, err := c.SubmitContent(context.Background(), []agent.ContentBlock{agent.TextBlock("again")}); err != nil {
		t.Fatal(err)
	}
	last := model.lastReq.Messages[len(model.lastReq.Messages)-1]
	if last.Content[0].Text != "again" {
		t.Fatalf("text turn = %+v", last.Content)
	}
}

// OnCompactBoundary reports each compaction with its trigger and the context
// size before it.
func TestOnCompactBoundary(t *testing.T) {
	c := New(&fakeModel{reply: "ok"}, fakeDispatcher{})
	c.Compactor = newCompactor(70000, &fakeSummarizer{})
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("old-request")),
		agent.AssistantMessage(agent.TextBlock("old-answer")),
	}
	var triggers []compaction.CompactTrigger
	var pre []int
	c.OnCompactBoundary = func(tr compaction.CompactTrigger, tokens int) {
		triggers = append(triggers, tr)
		pre = append(pre, tokens)
	}
	if _, err := c.Submit(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if len(triggers) != 1 || triggers[0] != compaction.TriggerAuto || pre[0] != 70000 {
		t.Fatalf("boundaries = %v %v", triggers, pre)
	}
}

// blockFeed is a mid-turn feed of whole content blocks.
type blockFeed struct{ blocks []agent.ContentBlock }

func (f *blockFeed) HasPending() bool      { return false }
func (f *blockFeed) TakePending() []string { return nil }
func (f *blockFeed) TakeMidTurnBlocks() []agent.ContentBlock {
	out := f.blocks
	f.blocks = nil
	return out
}

// A MidTurnBlockFeed's blocks ride the running turn's next model call.
func TestMidTurnBlockFeed(t *testing.T) {
	m := &toolThenEndModel{}
	c := New(m, fakeDispatcher{})
	c.AddContextFeed(&blockFeed{blocks: []agent.ContentBlock{agent.TextBlock("also this"), agent.ImageBlock("image/png", "QUJD")}})
	if _, err := c.Submit(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("model calls: %d", len(m.reqs))
	}
	last := m.reqs[1].Messages[len(m.reqs[1].Messages)-1]
	if len(last.Content) != 3 || last.Content[0].Type != agent.BlockToolResult || last.Content[1].Text != "also this" || last.Content[2].Type != agent.BlockImage {
		t.Fatalf("second round turn: %+v", last.Content)
	}
}

// SetModel consults the model policy; SetPermissionMode reaches the host.
func TestSetModelAndPermissionMode(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	c.ModelGate = func(m string) (string, string) {
		if m == "denied" {
			return "", "Model 'denied' is not available."
		}
		return m, ""
	}
	if got, refusal := c.SetModel("denied"); got != "" || refusal == "" {
		t.Fatalf("denied: %q %q", got, refusal)
	}
	if got, refusal := c.SetModel("m2"); got != "m2" || refusal != "" || c.ModelID != "m2" {
		t.Fatalf("allowed: %q %q %q", got, refusal, c.ModelID)
	}
	var seen toolreg.Mode
	c.OnModeChange = func(m toolreg.Mode) { seen = m }
	c.SetPermissionMode(toolreg.ModeAcceptEdits)
	if c.Mode != toolreg.ModeAcceptEdits || seen != toolreg.ModeAcceptEdits {
		t.Fatalf("mode = %q, seen = %q", c.Mode, seen)
	}
}
