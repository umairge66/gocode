package conversation

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/skills"
	"github.com/umairge66/gocode/internal/toolreg"
)

// recordingModel is a fake agent.ModelCaller that also implements ModelSelector,
// so a /model command can retarget it. It records the active model id at each
// call so a test can assert which model the NEXT turn used.
type recordingModel struct {
	model string
	seen  []string
}

func (m *recordingModel) SetModel(id string) { m.model = id }

func (m *recordingModel) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.seen = append(m.seen, m.model)
	return agent.ModelResponse{
		Message:    agent.AssistantMessage(agent.TextBlock("ok")),
		StopReason: agent.StopEndTurn,
	}, nil
}

// --- (1) /clear resets history with NO model call --------------------------

func TestBuiltinClearResetsHistoryNoModelCall(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("old")),
		agent.AssistantMessage(agent.TextBlock("older")),
	}

	res, err := c.Submit(context.Background(), "/clear")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Kind != ResultLocal {
		t.Fatalf("kind = %s, want Local", res.Kind)
	}
	if model.callCount != 0 {
		t.Fatalf("model was called %d times on /clear, want 0", model.callCount)
	}
	if len(c.History) != 0 {
		t.Fatalf("history not reset: %+v", c.History)
	}
	if res.Effect == nil || res.Effect.Kind.String() != "ClearTranscript" {
		t.Fatalf("effect = %+v, want ClearTranscript", res.Effect)
	}
}

// --- (2) /model sonnet changes the model used by the next turn -------------

func TestBuiltinModelRetargetsNextTurn(t *testing.T) {
	model := &recordingModel{model: "opus"}
	c := New(model, fakeDispatcher{})

	// /model sonnet is handled locally (no model call) and retargets the caller.
	res, err := c.Submit(context.Background(), "/model sonnet")
	if err != nil {
		t.Fatalf("Submit /model: %v", err)
	}
	if res.Kind != ResultLocal {
		t.Fatalf("kind = %s, want Local", res.Kind)
	}
	if len(model.seen) != 0 {
		t.Fatalf("model was called during /model: seen=%v", model.seen)
	}
	if c.ModelID != "sonnet" {
		t.Fatalf("ModelID = %q, want sonnet", c.ModelID)
	}

	// The NEXT real turn must run against the retargeted model.
	if _, err := c.Submit(context.Background(), "hello"); err != nil {
		t.Fatalf("Submit hello: %v", err)
	}
	if len(model.seen) != 1 || model.seen[0] != "sonnet" {
		t.Fatalf("next turn model = %v, want [sonnet]", model.seen)
	}
}

// --- (3) /compact triggers the compactor -----------------------------------

func TestBuiltinCompactTriggersCompactor(t *testing.T) {
	model := &fakeModel{}
	summ := &fakeSummarizer{}
	c := New(model, fakeDispatcher{})
	// A manual /compact fires regardless of the autocompact threshold; wire a
	// low estimate so the threshold check would NOT auto-fire.
	c.Compactor = newCompactor(10, summ)
	c.History = []agent.Message{
		agent.UserMessage(agent.TextBlock("q1")),
		agent.AssistantMessage(agent.TextBlock("a1")),
	}

	res, err := c.Submit(context.Background(), "/compact focus on the API")
	if err != nil {
		t.Fatalf("Submit /compact: %v", err)
	}
	if res.Kind != ResultLocal {
		t.Fatalf("kind = %s, want Local", res.Kind)
	}
	if model.callCount != 0 {
		t.Fatalf("model called on /compact: %d", model.callCount)
	}
	if summ.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summ.calls)
	}
	if !res.Compacted {
		t.Fatalf("Compacted = false, want true")
	}
	summaryText := messageText(c.History[0])
	if !strings.Contains(summaryText, "summarized") {
		t.Fatalf("history[0] is not the summary turn: %q", summaryText)
	}
	for _, m := range c.History {
		if strings.Contains(messageText(m), "q1") || strings.Contains(messageText(m), "a1") {
			t.Fatalf("pre-compaction transcript survived: %q", messageText(m))
		}
	}
}

// --- (3b) /exit signals exit ------------------------------------------------

func TestBuiltinExitSignalsExit(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})
	res, err := c.Submit(context.Background(), "/exit")
	if err != nil {
		t.Fatalf("Submit /exit: %v", err)
	}
	if res.Kind != ResultExit {
		t.Fatalf("kind = %s, want Exit", res.Kind)
	}
	if model.callCount != 0 {
		t.Fatalf("model called on /exit: %d", model.callCount)
	}
}

// --- (4) a custom command with a !-echo block splices output via a seam -----

func TestCustomCommandShellBlocksSplicedViaSeam(t *testing.T) {
	model := &fakeModel{}
	c := New(model, fakeDispatcher{})

	cmds := commands.NewRegistry()
	cmds.Add(&commands.Command{
		Type:   "prompt",
		Name:   "deploy",
		Body:   "Repo deploy:\n!`echo CLEAN`\nProceed.",
		Source: commands.SourceProject,
	})
	c.Commands = cmds

	// Fake shell seam: splice a deterministic value in place of the block, so the
	// unit test runs no real shell.
	var sawBody string
	c.ExpandShell = func(_ context.Context, body string) (string, error) {
		sawBody = body
		return strings.Replace(body, "!`echo CLEAN`", "CLEAN", 1), nil
	}

	if _, err := c.Submit(context.Background(), "/deploy"); err != nil {
		t.Fatalf("Submit /deploy: %v", err)
	}
	if !strings.Contains(sawBody, "!`echo CLEAN`") {
		t.Fatalf("seam did not receive the raw block body: %q", sawBody)
	}
	userText := messageText(c.History[0])
	if strings.Contains(userText, "!`echo CLEAN`") {
		t.Fatalf("raw block not spliced out of the prompt: %q", userText)
	}
	if !strings.Contains(userText, "CLEAN") || !strings.Contains(userText, "Repo deploy:") {
		t.Fatalf("spliced prompt wrong: %q", userText)
	}
	// And the model saw the spliced prompt.
	last := model.lastReq.Messages[len(model.lastReq.Messages)-1]
	if strings.Contains(messageText(last), "!`echo") {
		t.Fatalf("raw block leaked to the model: %q", messageText(last))
	}
}

// --- (5) a plugin-contributed command is resolvable ------------------------

func TestLoadPluginsMergesContributedCommand(t *testing.T) {
	m := fsfacade.NewMemFS()
	root := "/plugins/eng"
	m.WriteFile(root+"/.gocode-plugin/plugin.json", []byte(`{"name":"eng","version":"0.1.0","description":"d"}`))
	m.WriteFile(root+"/commands/ship.md", []byte("---\ndescription: ship it\n---\nShip the release: $ARGUMENTS"))
	m.WriteFile(root+"/skills/release-notes/SKILL.md", []byte("---\nname: release-notes\ndescription: notes\n---\nbody"))
	m.WriteFile(root+"/agents/reviewer.md", []byte("---\nname: reviewer\ndescription: reviews code\n---\nyou review"))

	p, err := plugins.LoadPluginDir(m, root, "mkt", plugins.OriginInstalled)
	if err != nil {
		t.Fatalf("LoadPluginDir: %v", err)
	}
	reg := plugins.NewRegistry([]*plugins.Plugin{p}, nil)

	c := New(&fakeModel{}, fakeDispatcher{})
	c.LoadPlugins(m, reg)

	if c.Commands == nil {
		t.Fatal("Commands registry not created")
	}
	cmd := c.Commands.Resolve("ship")
	if cmd == nil {
		t.Fatalf("plugin command /ship not resolvable; have %d commands", len(c.Commands.All()))
	}
	if cmd.Source != commands.SourcePlugin {
		t.Errorf("command source = %q, want plugin", cmd.Source)
	}
	if c.Skills == nil {
		t.Fatal("Skills registry not created")
	}
	if _, ok := c.Skills.Lookup("eng:release-notes"); !ok {
		// Plugin skills are namespaced "<id>:<name>"; accept either namespacing.
		if _, ok2 := c.Skills.Lookup("release-notes"); !ok2 {
			t.Errorf("plugin skill not resolvable; listing:\n%s", c.Skills.Listing())
		}
	}
	if c.Agents == nil {
		t.Fatal("Agents registry not created")
	}
	if _, ok := c.Agents.Lookup("reviewer"); !ok {
		t.Errorf("plugin agent 'reviewer' not resolvable")
	}
}

// A plugin command answers to "<plugin>:<command>" (and still to its bare
// name), and plugin text fills in ${CLAUDE_PLUGIN_ROOT} & co.
func TestLoadPluginsNamespacesAndExpandsVars(t *testing.T) {
	m := fsfacade.NewMemFS()
	root := "/plugins/eng"
	m.WriteFile(root+"/.claude-plugin/plugin.json", []byte(`{"name":"eng","version":"0.1.0"}`))
	m.WriteFile(root+"/commands/ship.md", []byte("---\ndescription: ship it\n---\nrun ${CLAUDE_PLUGIN_ROOT}/ship.sh in ${CLAUDE_PROJECT_DIR} data=${GOCODE_PLUGIN_DATA}"))
	m.WriteFile(root+"/skills/notes/SKILL.md", []byte("---\nname: notes\ndescription: notes\n---\nbody"))
	m.WriteFile(root+"/agents/reviewer.md", []byte("---\nname: reviewer\ndescription: reviews\n---\nscripts at ${CLAUDE_PLUGIN_ROOT}/bin"))
	p, err := plugins.LoadPluginDir(m, root, "mkt", plugins.OriginInstalled)
	if err != nil {
		t.Fatalf("LoadPluginDir: %v", err)
	}
	c := New(&fakeModel{}, fakeDispatcher{})
	c.PluginVars = func(p *plugins.Plugin) plugins.Vars { return p.Vars("/data/eng", "/proj") }
	c.LoadPlugins(m, plugins.NewRegistry([]*plugins.Plugin{p}, nil))

	cmd := c.Commands.Resolve("eng:ship")
	if cmd == nil || c.Commands.Resolve("ship") != cmd {
		t.Fatalf("eng:ship / ship do not resolve to the plugin command (%v)", cmd)
	}
	if want := "run " + root + "/ship.sh in /proj data=/data/eng"; cmd.Body != want {
		t.Errorf("command body = %q, want %q", cmd.Body, want)
	}
	var skill *skills.Descriptor
	for _, d := range c.Skills.List() {
		if d.UnqualifiedName == "notes" {
			skill = &d
		}
	}
	if skill == nil || skill.PluginRoot != root || skill.PluginData != "/data/eng" {
		t.Errorf("plugin skill vars not carried: %+v", skill)
	}
	ag, ok := c.Agents.Lookup("reviewer")
	if !ok || ag.SystemPrompt != "scripts at "+root+"/bin" {
		t.Errorf("agent prompt = %q", ag.SystemPrompt)
	}
}

// --- (6) a normal prompt still hits the model ------------------------------

func TestNormalPromptHitsModel(t *testing.T) {
	model := &fakeModel{reply: "answer"}
	c := New(model, fakeDispatcher{})
	res, err := c.Submit(context.Background(), "just a question")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Kind != ResultModel {
		t.Fatalf("kind = %s, want Model", res.Kind)
	}
	if model.callCount != 1 {
		t.Fatalf("model call count = %d, want 1", model.callCount)
	}
	if res.AssistantText != "answer" {
		t.Fatalf("assistant text = %q, want 'answer'", res.AssistantText)
	}
}

// Guard: the builtin table is wired by New so a plain prompt is unaffected and a
// builtin resolves without an explicit Commands registry.
func TestNewWiresBuiltinTable(t *testing.T) {
	c := New(&fakeModel{}, fakeDispatcher{})
	if c.Builtins == nil {
		t.Fatal("New did not wire a builtin table")
	}
	_ = toolreg.ModeDefault
}
