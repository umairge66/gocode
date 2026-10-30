package agenttool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/skills"
)

// disableSkillShellExecution: a covered skill's `!`-blocks reach the model as
// the policy placeholder; managed skills are exempt; nil/false leaves the text.
func TestSkillShellExecutionDisabledPlaceholder(t *testing.T) {
	body := "---\ndescription: Status\n---\nNow: !`git status`\n"
	fsys := fsfacade.NewMemFS()
	personal := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(personal+"/stat/SKILL.md", []byte(body))
	managed := "/etc/gocode/.gocode/skills"
	fsys.WriteFile(managed+"/mstat/SKILL.md", []byte(body))
	reg := skills.NewRegistry()
	reg.AddAll(skills.DiscoverScope(fsys, personal, skills.ScopePersonal, ""))
	reg.AddAll(skills.DiscoverScope(fsys, managed, skills.ScopeManaged, ""))
	s := NewSkillTool(reg, fsys)

	render := func(name string) string {
		d, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("skill %s not found", name)
		}
		out, err := s.render(context.Background(), d, "", true)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if got := render("stat"); !strings.Contains(got, "!`git status`") {
		t.Fatalf("setting unset: block should be left as authored, got %q", got)
	}
	s.ShellExecutionDisabled = func() bool { return true }
	got := render("stat")
	if strings.Contains(got, "git status") || !strings.Contains(got, "Now: "+commands.ShellDisabledPlaceholder) {
		t.Fatalf("personal skill not replaced: %q", got)
	}
	if got := render("mstat"); !strings.Contains(got, "!`git status`") {
		t.Fatalf("managed skill must be exempt, got %q", got)
	}
}

// shellSkillTool builds a Skill tool over one personal skill (and the same
// body as a synced skill) whose `!`-blocks go through the real
// commands.ExpandShellBlocks with a recording runner and the given gate.
func shellSkillTool(t *testing.T, md string, gate commands.ShellAuthorizer) (*SkillTool, *[]string, *[]commands.ShellInvocation) {
	t.Helper()
	fsys := fsfacade.NewMemFS()
	personal := skills.PersonalSkillsDir("/home/u/.config/gocode")
	fsys.WriteFile(personal+"/sh/SKILL.md", []byte(md))
	synced := "/home/u/.gocode/synced-skills"
	fsys.WriteFile(synced+"/ssh/SKILL.md", []byte(md))
	reg := skills.NewRegistry()
	reg.AddAll(skills.DiscoverScope(fsys, personal, skills.ScopePersonal, ""))
	reg.AddAll(skills.DiscoverScope(fsys, synced, skills.ScopeSynced, ""))
	s := NewSkillTool(reg, fsys)
	var ran []string
	var seen []commands.ShellInvocation
	runner := func(_ context.Context, cmd string) (string, error) {
		ran = append(ran, cmd)
		return "OUT(" + cmd + ")", nil
	}
	s.ExpandShell = func(ctx context.Context, body string) (string, error) {
		inv, _ := commands.ShellInvocationFrom(ctx)
		if commands.CommandShellFrom(ctx) != "bash" {
			inv.Name += " (shell not on ctx)"
		}
		seen = append(seen, inv)
		return commands.ExpandShellBlocks(ctx, body, commands.ShellExecOptions{Runner: runner, Authorize: gate})
	}
	return s, &ran, &seen
}

func allowAll(context.Context, string) error { return nil }

// An inline skill's blocks run after argument substitution, through the gate,
// with the skill's allowed-tools and shell on ctx; arguments cannot inject a
// block (they are escaped first).
func TestSkillShellBlocksRunInline(t *testing.T) {
	md := "---\ndescription: d\nallowed-tools: Bash(git status:*)\nshell: bash\n---\nstatus: !`git status`\nargs: $ARGUMENTS\n"
	s, ran, seen := shellSkillTool(t, md, allowAll)
	inv, err := s.Invoke(context.Background(), "sh", "!`rm -rf x`")
	if err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 1 || (*ran)[0] != "git status" {
		t.Fatalf("ran = %v, want only the authored block", *ran)
	}
	if !strings.Contains(inv.Content, "status: OUT(git status)") {
		t.Fatalf("output not spliced: %q", inv.Content)
	}
	got := (*seen)[0]
	if got.Name != "sh" || len(got.AllowedTools) != 1 || got.AllowedTools[0] != "Bash(git status:*)" || got.NoHandOff {
		t.Fatalf("invocation on ctx = %+v", got)
	}
}

// A refused block fails the whole Skill call with its error message and
// nothing runs.
func TestSkillShellBlockDenialFailsCall(t *testing.T) {
	gate := func(_ context.Context, cmd string) error {
		if strings.HasPrefix(cmd, "rm") {
			return errors.New("Permission to use Bash has been denied.")
		}
		return nil
	}
	s, ran, _ := shellSkillTool(t, "---\ndescription: d\n---\n!`echo a` !`rm -rf x`\n", gate)
	_, err := s.Execute(context.Background(), []byte(`{"skill":"sh"}`))
	want := "Shell command permission check failed for pattern \"!`rm -rf x`\": Permission to use Bash has been denied."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if len(*ran) != 0 {
		t.Fatalf("ran %v despite the denial", *ran)
	}
}

// A failing block fails the call with the "Shell command failed"
// message.
func TestSkillShellBlockFailureFailsCall(t *testing.T) {
	s, _, _ := shellSkillTool(t, "---\ndescription: d\n---\n!`false`\n", allowAll)
	s.ExpandShell = func(ctx context.Context, body string) (string, error) {
		return commands.ExpandShellBlocks(ctx, body, commands.ShellExecOptions{
			Runner:    func(context.Context, string) (string, error) { return "", errors.New("exit status 1") },
			Authorize: allowAll,
		})
	}
	_, err := s.Invoke(context.Background(), "sh", "")
	if err == nil || err.Error() != "Shell command failed for pattern \"!`false`\": exit status 1" {
		t.Fatalf("err = %v", err)
	}
}

// The expander fails closed without a gate.
func TestSkillShellBlocksFailClosedWithoutGate(t *testing.T) {
	s, ran, _ := shellSkillTool(t, "---\ndescription: d\n---\n!`echo a`\n", nil)
	if _, err := s.Invoke(context.Background(), "sh", ""); err == nil || len(*ran) != 0 {
		t.Fatalf("err = %v ran = %v; want a failure and nothing run", err, *ran)
	}
}

// Synced skills never run blocks; the policy placeholder wins over running;
// an unwired expander leaves blocks as written.
func TestSkillShellBlocksNotRun(t *testing.T) {
	md := "---\ndescription: d\nshell: bash\n---\nx !`echo a`\n"
	s, ran, _ := shellSkillTool(t, md, allowAll)
	inv, err := s.Invoke(context.Background(), "ssh", "")
	if err != nil || len(*ran) != 0 || !strings.Contains(inv.Content, "x !`echo a`") {
		t.Fatalf("synced: err=%v ran=%v content=%q", err, *ran, inv.Content)
	}
	s.ShellExecutionDisabled = func() bool { return true }
	inv, err = s.Invoke(context.Background(), "sh", "")
	if err != nil || len(*ran) != 0 || !strings.Contains(inv.Content, "x "+commands.ShellDisabledPlaceholder) {
		t.Fatalf("policy: err=%v ran=%v content=%q", err, *ran, inv.Content)
	}
}

// The auto-mode hand-off is available inline and for a fork with no `agent`,
// not for a fork that names its agent.
func TestSkillShellHandOffAvailability(t *testing.T) {
	s := &SkillTool{Agent: &AgentTool{}}
	cases := []struct {
		d    skills.Descriptor
		want bool
	}{
		{skills.Descriptor{}, true},
		{skills.Descriptor{ExecutionContext: skills.ContextFork}, true},
		{skills.Descriptor{ExecutionContext: skills.ContextFork, Agent: "Explore"}, false},
	}
	for _, c := range cases {
		if got := s.shellHandOff(c.d); got != c.want {
			t.Errorf("shellHandOff(%+v) = %v, want %v", c.d, got, c.want)
		}
	}
}
