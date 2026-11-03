package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/builtincmds"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/skills"
)

// fakeUserSkills records typed skills and answers with a canned outcome.
type fakeUserSkills struct {
	calls []string
	out   UserSkillResult
}

func (f *fakeUserSkills) InvokeUserSkill(_ context.Context, name, args string) (UserSkillResult, error) {
	f.calls = append(f.calls, name+"|"+args)
	return f.out, nil
}

func skillConversation(model *fakeModel, inv *fakeUserSkills) *Conversation {
	c := New(model, fakeDispatcher{})
	reg := skills.NewRegistry()
	reg.Add(skills.Descriptor{Name: "deploy", Description: "ship it", UserInvocable: true, DisableModelInvocation: true, Scope: skills.ScopeProject})
	reg.Add(skills.Descriptor{Name: "modelonly", Description: "x", UserInvocable: false, Scope: skills.ScopeProject})
	reg.Add(skills.Descriptor{Name: "foo", Description: "shadowed", UserInvocable: true, Scope: skills.ScopeProject})
	c.Skills = reg
	cmds := commands.NewRegistry()
	cmds.Add(&commands.Command{Type: "prompt", Name: "foo", Body: "command foo", Source: commands.SourceProject})
	c.Commands = cmds
	c.UserSkills = inv
	return c
}

// TestUserSkillSlashCommand: `/deploy prod` runs the skill (even though it
// is hidden from the model) and its content is the user turn.
func TestUserSkillSlashCommand(t *testing.T) {
	model := &fakeModel{}
	inv := &fakeUserSkills{out: UserSkillResult{Content: "Deploy steps for prod"}}
	c := skillConversation(model, inv)

	if _, err := c.Submit(context.Background(), "/deploy prod"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(inv.calls) != 1 || inv.calls[0] != "deploy|prod" {
		t.Fatalf("calls = %v", inv.calls)
	}
	got := messageText(c.History[0])
	if !strings.Contains(got, "<command-name>/deploy</command-name>") || !strings.Contains(got, "Deploy steps for prod") {
		t.Fatalf("user turn = %q", got)
	}
	if model.callCount != 1 {
		t.Fatalf("model calls = %d", model.callCount)
	}
	// The model's listing still hides it (disable-model-invocation).
	if strings.Contains(model.lastReq.System, "deploy") {
		t.Errorf("model-hidden skill listed: %q", model.lastReq.System)
	}
}

// TestUserSkillPrecedenceAndGates: a command of the same name wins; a
// user-invocable: false skill is not a slash command.
func TestUserSkillPrecedenceAndGates(t *testing.T) {
	model := &fakeModel{}
	inv := &fakeUserSkills{}
	c := skillConversation(model, inv)

	if _, err := c.Submit(context.Background(), "/foo"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(inv.calls) != 0 || messageText(c.History[0]) != "command foo" {
		t.Fatalf("command did not win: calls %v turn %q", inv.calls, messageText(c.History[0]))
	}
	_, err := c.Submit(context.Background(), "/modelonly")
	if _, ok := err.(*ErrUnknownCommand); !ok {
		t.Fatalf("err = %v, want ErrUnknownCommand", err)
	}
	if _, ok := c.UserSkill("modelonly"); ok {
		t.Error("UserSkill reported a user-invocable: false skill")
	}
	if _, ok := c.UserSkill("foo"); ok {
		t.Error("UserSkill reported a name a command owns")
	}
	c.UserSkills = nil
	if _, ok := c.UserSkill("deploy"); ok {
		t.Error("UserSkill without an invoker")
	}
}

// TestUserSkillForked: a forked skill's report answers locally, with no model
// turn, and rides the next turn as context.
func TestUserSkillForked(t *testing.T) {
	model := &fakeModel{}
	inv := &fakeUserSkills{out: UserSkillResult{Forked: true, Result: "Skill \"deploy\" completed (forked execution).\n\nResult:\nall green"}}
	c := skillConversation(model, inv)

	res, err := c.Submit(context.Background(), "/deploy")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Kind != ResultLocal || res.Effect == nil || res.Effect.Kind != builtincmds.EffectPrintText || !strings.Contains(res.Effect.Text, "all green") {
		t.Fatalf("result = %+v", res)
	}
	if model.callCount != 0 {
		t.Fatalf("model called %d times", model.callCount)
	}
	if _, err := c.Submit(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range model.lastReq.Messages {
		if strings.Contains(messageText(m), "all green") {
			seen = true
		}
	}
	if !seen {
		t.Error("the forked result did not reach the next turn")
	}
}
