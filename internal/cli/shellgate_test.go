package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/umairge66/gocode/internal/automode"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// Tests for the permission gate on custom slash commands' `!`-blocks: each
// block is checked as a Bash call through the session dispatcher (same
// Decider, mode and shared rules as the Bash tool), with the command's
// allowed-tools as temporary allow rules. All offline: fake model, fake shell
// runner, fake prompter.

const gateCmdName = "gatecheck"

// gateCaller is a fake model that records how often it was called.
type gateCaller struct {
	mu    sync.Mutex
	calls int
	last  agent.ModelRequest
}

func (c *gateCaller) CallModel(_ context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c.mu.Lock()
	c.calls++
	c.last = req
	c.mu.Unlock()
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

func (c *gateCaller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// gateRunner is a fake shell runner that records every command it ran.
type gateRunner struct {
	mu  sync.Mutex
	ran []string
}

func (r *gateRunner) run(_ context.Context, cmd string) (string, error) {
	r.mu.Lock()
	r.ran = append(r.ran, cmd)
	r.mu.Unlock()
	return "OUT(" + cmd + ")", nil
}

func (r *gateRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...)
}

// gatePrompter is a fake interactive permission prompter.
type gatePrompter struct {
	mu      sync.Mutex
	reqs    []toolrun.PermissionRequest
	respond func(toolrun.PermissionRequest) toolrun.PermissionResponse
}

func (p *gatePrompter) Prompt(_ context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.respond(req), nil
}

func (p *gatePrompter) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

// failPrompter fails the test if it is ever asked.
func failPrompter(t *testing.T) *gatePrompter {
	return &gatePrompter{respond: func(req toolrun.PermissionRequest) toolrun.PermissionResponse {
		t.Errorf("unexpected permission prompt for %q", req.RuleContent)
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}
	}}
}

type gateFixture struct {
	conv   *conversation.Conversation
	d      *toolrun.Dispatcher
	model  *gateCaller
	runner *gateRunner
}

// newGateFixture builds a conversation wired exactly like a live session's
// shell seam (configureSession) over d, with the given commands registered.
func newGateFixture(t *testing.T, d *toolrun.Dispatcher, cmds ...*commands.Command) *gateFixture {
	t.Helper()
	model := &gateCaller{}
	conv := conversation.New(model, d)
	reg := commands.NewRegistry()
	for _, c := range cmds {
		if c.Type == "" {
			c.Type = "prompt"
		}
		if c.Source == "" {
			c.Source = commands.SourceProject
		}
		reg.Add(c)
	}
	conv.Commands = reg
	runner := &gateRunner{}
	configureSession(conv, &printSession{shellRunner: runner.run}, nil, nil, nil, d)
	return &gateFixture{conv: conv, d: d, model: model, runner: runner}
}

func gateCmd(body string, allowed ...string) *commands.Command {
	return &commands.Command{Name: gateCmdName, Body: body, AllowedTools: allowed}
}

func allowRule(content string) toolreg.Rule {
	return toolreg.Rule{Behavior: toolreg.RuleAllow, ToolName: "Bash", Content: content}
}

func denyRule(content string) toolreg.Rule {
	return toolreg.Rule{Behavior: toolreg.RuleDeny, ToolName: "Bash", Content: content}
}

// A block matching an allow rule runs and its output reaches the model.
func TestShellGate_AllowRuleRuns(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{allowRule("echo:*")})
	d.SetPermissionPrompter(failPrompter(t))
	f := newGateFixture(t, d, gateCmd("status: !`echo hi`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := f.runner.commands(); len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("ran = %v", got)
	}
	if f.model.count() != 1 {
		t.Fatalf("model calls = %d, want 1", f.model.count())
	}
}

// A deny rule refuses its block (even in bypass mode), no block runs, and the
// model is never called; the user sees the refusal message.
func TestShellGate_DenyRuleAbortsCommand(t *testing.T) {
	d := toolrun.New(toolreg.ModeBypassPermissions, toolreg.RuleSet{denyRule("rm:*")})
	f := newGateFixture(t, d, gateCmd("a !`echo a` b !`rm -rf x` c !`echo b`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil {
		t.Fatal("expected the command to be aborted")
	}
	var perr *commands.ShellPermissionError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a ShellPermissionError", err)
	}
	if !strings.Contains(err.Error(), "Shell command permission check failed for pattern \"!`rm -rf x`\"") ||
		!strings.Contains(err.Error(), "deny rule") {
		t.Fatalf("err text = %q", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "/"+gateCmdName+": ") {
		t.Fatalf("err should name the command: %q", err.Error())
	}
	if got := f.runner.commands(); len(got) != 0 {
		t.Fatalf("blocks ran despite a deny: %v", got)
	}
	if f.model.count() != 0 {
		t.Fatalf("model was called %d times after a denied block", f.model.count())
	}
	if len(f.conv.History) != 0 {
		t.Fatalf("history grew on an aborted command: %d", len(f.conv.History))
	}
}

// A deny rule also wins over the command's own allowed-tools.
func TestShellGate_DenyBeatsAllowedTools(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, toolreg.RuleSet{denyRule("git push:*")})
	f := newGateFixture(t, d, gateCmd("!`git push origin main`", "Bash(git push:*)"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err == nil {
		t.Fatal("deny rule must beat allowed-tools")
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 0 {
		t.Fatal("denied block ran or model called")
	}
}

// allowed-tools: Bash(git status:*) lets `git status` run without a prompt, but
// not `rm -rf x` (which is asked, and denied here) nor a chained command.
func TestShellGate_AllowedToolsScopeOnlyMatchingCommands(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	pr := &gatePrompter{respond: func(toolrun.PermissionRequest) toolrun.PermissionResponse {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}
	}}
	d.SetPermissionPrompter(pr)

	ok := newGateFixture(t, d, gateCmd("!`git status --short`", "Bash(git status:*)"))
	if _, err := ok.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("git status should be allowed by allowed-tools: %v", err)
	}
	if pr.count() != 0 {
		t.Fatalf("prompted %d times for an allowed-tools command", pr.count())
	}

	for _, body := range []string{"!`rm -rf x`", "!`git status; rm -rf x`"} {
		bad := newGateFixture(t, d, gateCmd(body, "Bash(git status:*)"))
		if _, err := bad.conv.Submit(context.Background(), "/"+gateCmdName); err == nil {
			t.Fatalf("%s must not be covered by Bash(git status:*)", body)
		}
		if len(bad.runner.commands()) != 0 || bad.model.count() != 0 {
			t.Fatalf("%s ran or reached the model", body)
		}
	}
	if pr.count() != 2 {
		t.Fatalf("prompts = %d, want 2 (one per uncovered command)", pr.count())
	}
}

// The command's allowed-tools are temporary: they are not added to the shared
// rules, not persisted, and do not cover the next command or a model Bash call.
// (`git clean -n` is a dry run that is not read-only, so only a rule allows it;
// a read-only command such as `git status` is allowed by the Bash check itself.)
func TestShellGate_AllowedToolsDoNotLeak(t *testing.T) {
	shared := toolrun.NewSharedRules(nil)
	d := toolrun.New(toolreg.ModeDefault, nil, toolrun.WithSharedRules(shared))
	f := newGateFixture(t, d,
		gateCmd("!`git clean -n`", "Bash(git clean:*)"),
		&commands.Command{Name: "other", Body: "!`git clean -n`"},
	)
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rules := shared.Snapshot(); len(rules) != 0 {
		t.Fatalf("allowed-tools leaked into the shared rules: %+v", rules)
	}
	if rules := d.SnapshotRules(); len(rules) != 0 {
		t.Fatalf("allowed-tools leaked into the dispatcher rules: %+v", rules)
	}
	// Next command without allowed-tools (-p: no prompter) is refused.
	if _, err := f.conv.Submit(context.Background(), "/other"); err == nil {
		t.Fatal("/other ran `git clean -n` using another command's allowed-tools")
	}
	// A model Bash tool call for the same command still needs approval.
	input, _ := json.Marshal(map[string]string{"command": "git clean -n"})
	res, err := d.Dispatch(context.Background(), agent.ToolUse{ID: "t1", Name: "Bash", Input: input})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Fatal("model Bash call was allowed by a slash command's allowed-tools")
	}
}

// Interactive ask: the prompter sees the exact command and the slash command it
// comes from; allow-once runs it without adding any rule.
func TestShellGate_InteractiveAllowOnce(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	pr := &gatePrompter{respond: func(toolrun.PermissionRequest) toolrun.PermissionResponse {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedOnce}
	}}
	d.SetPermissionPrompter(pr)
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if pr.count() != 1 {
		t.Fatalf("prompts = %d, want 1", pr.count())
	}
	req := pr.reqs[0]
	if req.Tool != protocol.ToolBash || req.RuleContent != "make build" || req.Origin != "/"+gateCmdName {
		t.Fatalf("prompt request = %+v", req)
	}
	if !strings.Contains(string(req.Input), "make build") {
		t.Fatalf("prompt input = %s", req.Input)
	}
	if got := f.runner.commands(); len(got) != 1 || got[0] != "make build" {
		t.Fatalf("ran = %v", got)
	}
	if len(d.SnapshotRules()) != 0 {
		t.Fatal("allow-once added a rule")
	}
}

// Interactive deny aborts the command: nothing runs and the model is not called.
func TestShellGate_InteractiveDenyAborts(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(&gatePrompter{respond: func(toolrun.PermissionRequest) toolrun.PermissionResponse {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}
	}})
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil || !strings.Contains(err.Error(), "denied at the permission prompt") {
		t.Fatalf("err = %v", err)
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 0 {
		t.Fatal("denied block ran or model called")
	}
}

// "Always" persists the Bash tool's prefix rule (via the prompter's persist
// seam) and adds it to the live shared rules, so the next run does not prompt.
func TestShellGate_InteractiveAlwaysPersistsPrefixRule(t *testing.T) {
	shared := toolrun.NewSharedRules(nil)
	d := toolrun.New(toolreg.ModeDefault, nil, toolrun.WithSharedRules(shared))
	var persisted []string
	dp := newDialogPrompter(func(rule string) error { persisted = append(persisted, rule); return nil })
	pr2 := &gatePrompter{respond: func(req toolrun.PermissionRequest) toolrun.PermissionResponse {
		// Exactly what the real dialog does on "Yes, and don't ask again".
		return dp.decide(req, &ui.Dialog{}, ui.PermAllowAlways)
	}}
	d.SetPermissionPrompter(pr2)
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(persisted) != 1 || persisted[0] != "Bash(make:*)" {
		t.Fatalf("persisted = %v, want the Bash prefix rule [Bash(make:*)]", persisted)
	}
	if shared.Snapshot().Match(toolreg.Call{Tool: protocol.ToolBash, Rule: "Bash", RuleContent: "make test"}, toolreg.RuleAllow) == nil {
		t.Fatal("always rule not added to the live shared rules")
	}
	// Second run: no prompt.
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if pr2.count() != 1 {
		t.Fatalf("prompts = %d, want 1", pr2.count())
	}
}

// -p (no prompter): an ask is denied with guidance naming the command and the
// rule to add.
func TestShellGate_PrintModeAskDeniedWithGuidance(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	f := newGateFixture(t, d, gateCmd("!`npm test`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil {
		t.Fatal("expected a denial")
	}
	msg := err.Error()
	for _, want := range []string{
		"Shell command permission check failed for pattern \"!`npm test`\"",
		"gocode requested permissions to use Bash, but you haven't granted it yet.",
		"\"Bash(npm:*)\"",
		"allowed-tools frontmatter of /" + gateCmdName,
		"permissions.allow",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 0 {
		t.Fatal("block ran or model called")
	}
}

// stubClassifier answers every auto mode classification with one verdict.
type stubClassifier struct{ verdict automode.Verdict }

func (s stubClassifier) Classify(context.Context, automode.Request) automode.Result {
	return automode.Result{Verdict: s.verdict, Reason: "[Stub] stub verdict"}
}

// Auto mode runs a block the classifier allows without prompting.
func TestShellGate_AutoModeRunsWithoutPrompt(t *testing.T) {
	d := toolrun.New(toolreg.ModeAuto, nil, toolrun.WithAutoClassifier(stubClassifier{automode.VerdictAllow}))
	d.SetPermissionPrompter(failPrompter(t))
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(f.runner.commands()) != 1 || f.model.count() != 1 {
		t.Fatal("auto mode did not run the block")
	}
}

// Plan mode never runs a block silently: -p denies it with the plan message,
// interactive holds it for the prompt.
func TestShellGate_PlanModeDoesNotSilentlyRun(t *testing.T) {
	d := toolrun.New(toolreg.ModePlan, nil)
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	_, err := f.conv.Submit(context.Background(), "/"+gateCmdName)
	if err == nil || !strings.Contains(err.Error(), "plan mode") {
		t.Fatalf("plan -p err = %v", err)
	}
	if len(f.runner.commands()) != 0 {
		t.Fatal("plan mode ran the block")
	}

	di := toolrun.New(toolreg.ModePlan, nil)
	pr := &gatePrompter{respond: func(toolrun.PermissionRequest) toolrun.PermissionResponse {
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}
	}}
	di.SetPermissionPrompter(pr)
	fi := newGateFixture(t, di, gateCmd("!`make build`"))
	if _, err := fi.conv.Submit(context.Background(), "/"+gateCmdName); err == nil {
		t.Fatal("denied plan-mode block should abort")
	}
	if pr.count() != 1 || len(fi.runner.commands()) != 0 {
		t.Fatalf("plan interactive: prompts=%d ran=%v", pr.count(), fi.runner.commands())
	}
}

// The live mode is read on each check: a mode change takes effect.
func TestShellGate_FollowsLiveMode(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	f := newGateFixture(t, d, gateCmd("!`make build`"))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err == nil {
		t.Fatal("default -p should deny")
	}
	d.SetMode(toolreg.ModeBypassPermissions)
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName); err != nil {
		t.Fatalf("bypass should allow: %v", err)
	}
}

// A command with no `!` blocks is unaffected by the gate.
func TestShellGate_NoBlocksUnaffected(t *testing.T) {
	d := toolrun.New(toolreg.ModeDefault, nil)
	d.SetPermissionPrompter(failPrompter(t))
	f := newGateFixture(t, d, gateCmd("Just review $ARGUMENTS please."))
	if _, err := f.conv.Submit(context.Background(), "/"+gateCmdName+" main.go"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(f.runner.commands()) != 0 || f.model.count() != 1 {
		t.Fatal("plain command misbehaved")
	}
}

// Without a dispatcher the gate is unwired and expansion fails closed.
func TestShellGate_NilDispatcherFailsClosed(t *testing.T) {
	conv := &conversation.Conversation{}
	runner := &gateRunner{}
	configureSession(conv, &printSession{shellRunner: runner.run}, nil, nil, nil, nil)
	if _, err := conv.ExpandShell(context.Background(), "!`echo hi`"); !errors.Is(err, commands.ErrNoShellAuthorizer) {
		t.Fatalf("err = %v, want ErrNoShellAuthorizer", err)
	}
	if len(runner.commands()) != 0 {
		t.Fatal("ran without a gate")
	}
}
