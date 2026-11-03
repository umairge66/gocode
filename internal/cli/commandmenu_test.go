package cli

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/skills"
)

// buildTestConv returns a conversation whose Builtins are the default table and
// whose Commands registry carries a couple of custom commands (one hidden).
func buildTestConv() *conversation.Conversation {
	conv := conversation.New(fakeCaller{reply: "ok"}, fakeToolDispatcher{})
	reg := commands.NewRegistry()
	reg.Add(&commands.Command{Type: "prompt", Name: "deploy", Description: "Deploy the app", Source: commands.SourceProject, UserInvocable: true})
	reg.Add(&commands.Command{Type: "prompt", Name: "secret", Description: "hidden", Source: commands.SourceProject, Hidden: true})
	conv.Commands = reg
	return conv
}

func TestCommandSuggestions_BuiltinsAndCustom(t *testing.T) {
	conv := buildTestConv()
	sugs := commandSuggestions(conv)
	if len(sugs) == 0 {
		t.Fatal("expected a non-empty suggestion list")
	}
	byName := map[string]bool{}
	for _, s := range sugs {
		byName[s.Name] = true
		// Every suggestion is well-formed.
		if s.Display != "/"+s.Name || s.Value != "/"+s.Name {
			t.Fatalf("malformed suggestion %+v", s)
		}
	}
	// A couple of known builtins must be present.
	for _, want := range []string{"model", "help", "clear"} {
		if !byName[want] {
			t.Fatalf("builtin /%s missing from suggestions", want)
		}
	}
	// The custom command is present; the hidden one is not.
	if !byName["deploy"] {
		t.Fatal("custom command /deploy missing from suggestions")
	}
	if byName["secret"] {
		t.Fatal("hidden custom command /secret must be excluded")
	}
	// A hidden builtin (/agents) must be excluded.
	if byName["agents"] {
		t.Fatal("hidden builtin /agents must be excluded")
	}
}

func TestIsRecognizedCommand(t *testing.T) {
	conv := buildTestConv()
	cases := []struct {
		in   string
		want bool
	}{
		{"/model", true},        // builtin
		{"/mcp", true},          // builtin
		{"/clear", true},        // builtin
		{"/reset", true},        // builtin alias (of /clear)
		{"/model sonnet", true}, // builtin with args
		{"  /model  ", true},    // leading/trailing space trimmed
		{"/deploy", true},       // loaded custom command
		{"/secret", true},       // hidden custom command still resolves when typed
		{"/foo", false},         // unknown slash command: not recognized
		{"hello", false},        // plain message
		{"", false},             // empty
		{"/", false},            // no name token
	}
	for _, c := range cases {
		if got := isRecognizedCommand(conv, c.in); got != c.want {
			t.Errorf("isRecognizedCommand(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// A nil conversation recognizes nothing.
	if isRecognizedCommand(nil, "/model") {
		t.Error("isRecognizedCommand(nil, …) should be false")
	}
}

func TestHelpText_ListsCommands(t *testing.T) {
	conv := buildTestConv()
	txt := helpTextForConversation(conv)
	if strings.TrimSpace(txt) == "" {
		t.Fatal("/help produced empty text")
	}
	if !strings.Contains(txt, "Available commands:") {
		t.Fatalf("help text missing header:\n%s", txt)
	}
	for _, want := range []string{"/model", "/help", "/clear", "/deploy"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("help text missing %q:\n%s", want, txt)
		}
	}
	// Descriptions are shown alongside the command names.
	if !strings.Contains(txt, "Deploy the app") {
		t.Fatalf("help text missing custom description:\n%s", txt)
	}
}

// TestCommandSuggestions_Skills: user-invocable skills join the menu (a
// model-hidden one too) and are recognized commands; a model-only skill and a
// name a command owns are not offered as skills.
func TestCommandSuggestions_Skills(t *testing.T) {
	conv := buildTestConv()
	reg := skills.NewRegistry()
	reg.Add(skills.Descriptor{Name: "review", Description: "Review the diff", UserInvocable: true, Scope: skills.ScopeProject})
	reg.Add(skills.Descriptor{Name: "userish", Description: "u", UserInvocable: true, DisableModelInvocation: true, Scope: skills.ScopeProject})
	reg.Add(skills.Descriptor{Name: "modelish", Description: "m", Scope: skills.ScopeProject})
	reg.Add(skills.Descriptor{Name: "deploy", Description: "skill deploy", UserInvocable: true, Scope: skills.ScopeProject})
	conv.Skills = reg
	conv.UserSkills = userSkillRunner{}

	kinds := map[string]string{}
	descs := map[string]string{}
	for _, s := range commandSuggestions(conv) {
		kinds[s.Name], descs[s.Name] = s.Kind, s.Description
	}
	if kinds["review"] != "skill" || kinds["userish"] != "skill" {
		t.Fatalf("skills missing from the menu: %v", kinds)
	}
	if _, ok := kinds["modelish"]; ok {
		t.Error("a user-invocable: false skill was offered")
	}
	if descs["deploy"] != "Deploy the app" {
		t.Errorf("/deploy = %q, want the command's entry", descs["deploy"])
	}
	if !isRecognizedCommand(conv, "/review now") || isRecognizedCommand(conv, "/modelish") {
		t.Error("isRecognizedCommand mismatch for skills")
	}
}
