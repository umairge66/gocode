package agentsdef

import (
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

func TestParseDefinitionFrontmatterAndBody(t *testing.T) {
	md := `---
name: code-reviewer
description: Reviews code for bugs and style issues
tools: Read, Grep, Edit
model: sonnet
color: blue
effort: high
---
You are a meticulous code reviewer.

Report findings concisely.`

	pr := ParseDefinition([]byte(md), "/proj/.gocode/agents/code-reviewer.md")
	if pr.ParseError != "" {
		t.Fatalf("unexpected parse error: %s", pr.ParseError)
	}
	if pr.Frontmatter.Name != "code-reviewer" {
		t.Errorf("name = %q", pr.Frontmatter.Name)
	}
	if pr.Frontmatter.Description != "Reviews code for bugs and style issues" {
		t.Errorf("description = %q", pr.Frontmatter.Description)
	}

	d, msg := BuildFileDescriptor(pr, "/proj/.gocode/agents/code-reviewer.md", "/proj/.gocode/agents", SourceProject)
	if msg != "" {
		t.Fatalf("BuildFileDescriptor rejected: %s", msg)
	}
	if d.AgentType != "code-reviewer" {
		t.Errorf("agentType = %q", d.AgentType)
	}
	if d.WhenToUse != "Reviews code for bugs and style issues" {
		t.Errorf("whenToUse = %q", d.WhenToUse)
	}
	// Body -> system prompt (trimmed).
	wantPrompt := "You are a meticulous code reviewer.\n\nReport findings concisely."
	if d.SystemPrompt != wantPrompt {
		t.Errorf("systemPrompt = %q want %q", d.SystemPrompt, wantPrompt)
	}
	if !reflect.DeepEqual(d.Tools, []string{"Read", "Grep", "Edit"}) {
		t.Errorf("tools = %v", d.Tools)
	}
	if d.Model != "sonnet" || d.Color != "blue" || d.Effort != "high" {
		t.Errorf("model/color/effort = %q/%q/%q", d.Model, d.Color, d.Effort)
	}
	if d.Filename != "code-reviewer" {
		t.Errorf("filename = %q", d.Filename)
	}
}

func TestToolsListParse(t *testing.T) {
	cases := []struct {
		name  string
		fm    string
		want  []string
		isNil bool
	}{
		{"comma", "tools: Read, Write, Edit\n", []string{"Read", "Write", "Edit"}, false},
		{"inline-list", "tools: [Read, Grep]\n", []string{"Read", "Grep"}, false},
		{"block-list", "tools:\n  - Read\n  - Bash\n", []string{"Read", "Bash"}, false},
		{"star-inherits-all", "tools: '*'\n", nil, true},
		{"star-in-list", "tools: [Read, '*']\n", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := "---\nname: a\ndescription: d\n" + tc.fm + "---\nbody"
			pr := ParseDefinition([]byte(md), "a.md")
			if tc.isNil {
				if pr.Frontmatter.Tools != nil {
					t.Errorf("tools = %v, want nil (inherit all)", pr.Frontmatter.Tools)
				}
				return
			}
			if !reflect.DeepEqual(pr.Frontmatter.Tools, tc.want) {
				t.Errorf("tools = %v want %v", pr.Frontmatter.Tools, tc.want)
			}
		})
	}
}

func TestKeyAliases(t *testing.T) {
	md := "---\nname: a\nwhen_to_use: use me\nallowed-tools: Read\n---\nbody"
	pr := ParseDefinition([]byte(md), "a.md")
	if pr.Frontmatter.Description != "use me" {
		t.Errorf("when_to_use alias -> description = %q", pr.Frontmatter.Description)
	}
	if !reflect.DeepEqual(pr.Frontmatter.Tools, []string{"Read"}) {
		t.Errorf("allowed-tools alias -> tools = %v", pr.Frontmatter.Tools)
	}
}

func TestValidateName(t *testing.T) {
	cases := map[string]bool{ // name -> valid
		"ok":          true,
		"code-review": true,
		"":            false,
		"-leading":    false,
		"has:colon":   false,
	}
	for name, valid := range cases {
		msg := ValidateName(name)
		if valid && msg != "" {
			t.Errorf("ValidateName(%q) = %q, want valid", name, msg)
		}
		if !valid && msg == "" {
			t.Errorf("ValidateName(%q) = valid, want invalid", name)
		}
	}
}

func TestDescriptionNewlineExpansion(t *testing.T) {
	md := "---\nname: a\ndescription: line1\\nline2\n---\nbody"
	pr := ParseDefinition([]byte(md), "a.md")
	if pr.Frontmatter.Description != "line1\nline2" {
		t.Errorf("description = %q, want literal \\n expanded", pr.Frontmatter.Description)
	}
}

func TestMissingNameOrDescriptionRejected(t *testing.T) {
	noName := ParseDefinition([]byte("---\ndescription: d\n---\nbody"), "a.md")
	if _, msg := BuildFileDescriptor(noName, "a.md", "/b", SourceProject); msg == "" {
		t.Error("expected rejection for missing name")
	}
	noDesc := ParseDefinition([]byte("---\nname: a\n---\nbody"), "a.md")
	if _, msg := BuildFileDescriptor(noDesc, "a.md", "/b", SourceProject); msg == "" {
		t.Error("expected rejection for missing description")
	}
}

func TestBuiltinsPresentInListing(t *testing.T) {
	reg := NewRegistry()
	reg.AddAll(BuiltinAgents())
	want := []string{
		AgentGeneralPurpose, AgentExplore, AgentPlan,
		AgentStatuslineSetup, AgentGocodeGuide, AgentWebFetch,
	}
	for _, name := range want {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("built-in %q missing from registry", name)
		}
	}
	// general-purpose inherits all tools (Tools == nil).
	gp, _ := reg.Lookup(AgentGeneralPurpose)
	if !gp.InheritsAllTools() {
		t.Errorf("general-purpose should inherit all tools, got %v", gp.Tools)
	}
	if !gp.IsBuiltin() {
		t.Error("general-purpose should be built-in")
	}
	listing := reg.Listing()
	if listing == "" {
		t.Fatal("empty listing")
	}
}

func TestDiscoveryUserAndProjectPrecedence(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/home/u/.gocode/agents")
	fs.MkdirAll("/proj/.gocode/agents")

	// Same agent type defined at user and project scope.
	userDef := "---\nname: helper\ndescription: user helper\nmodel: haiku\n---\nuser prompt"
	projDef := "---\nname: helper\ndescription: project helper\nmodel: sonnet\n---\nproject prompt"
	// A user-only agent and a nested project agent.
	userOnly := "---\nname: only-user\ndescription: only user\n---\nbody"
	nestedProj := "---\nname: nested\ndescription: nested project agent\n---\nbody"

	fs.WriteFile("/home/u/.gocode/agents/helper.md", []byte(userDef))
	fs.WriteFile("/home/u/.gocode/agents/only-user.md", []byte(userOnly))
	fs.WriteFile("/proj/.gocode/agents/helper.md", []byte(projDef))
	fs.MkdirAll("/proj/.gocode/agents/sub")
	fs.WriteFile("/proj/.gocode/agents/sub/nested.md", []byte(nestedProj))

	reg := Load(fs, LoadOptions{
		UserAgentsDir:   "/home/u/.gocode/agents",
		ProjectDir:      "/proj",
		IncludeBuiltins: false,
	})

	helper, ok := reg.Lookup("helper")
	if !ok {
		t.Fatal("helper not found")
	}
	// Project overrides user.
	if helper.Source != SourceProject || helper.WhenToUse != "project helper" {
		t.Errorf("helper resolved to %s/%q, want project override", helper.Source, helper.WhenToUse)
	}
	if len(reg.Conflicts) != 1 || reg.Conflicts[0].Rejected != SourceUser {
		t.Errorf("conflicts = %+v, want one user->project conflict", reg.Conflicts)
	}
	if ou, ok := reg.Lookup("only-user"); !ok || ou.Source != SourceUser {
		t.Errorf("only-user missing or wrong source: %+v", ou)
	}
	// Nested (subdirectory) project agent discovered recursively.
	if _, ok := reg.Lookup("nested"); !ok {
		t.Error("nested project agent not discovered recursively")
	}
}

func TestPolicyBeatsEverything(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/proj/.gocode/agents")
	fs.MkdirAll("/managed/.gocode/agents")
	fs.WriteFile("/proj/.gocode/agents/x.md", []byte("---\nname: x\ndescription: project x\n---\nbody"))
	fs.WriteFile("/managed/.gocode/agents/x.md", []byte("---\nname: x\ndescription: managed x\n---\nbody"))

	reg := Load(fs, LoadOptions{ProjectDir: "/proj", ManagedDir: "/managed"})
	x, _ := reg.Lookup("x")
	if x.Source != SourcePolicy || x.WhenToUse != "managed x" {
		t.Errorf("x = %s/%q, want managed policy win", x.Source, x.WhenToUse)
	}
}

func TestAdditionalDirLosesToMainProject(t *testing.T) {
	fs := fsfacade.NewMemFS()
	fs.MkdirAll("/proj/.gocode/agents")
	fs.MkdirAll("/added/.gocode/agents")
	fs.WriteFile("/proj/.gocode/agents/y.md", []byte("---\nname: y\ndescription: main project\n---\nbody"))
	fs.WriteFile("/added/.gocode/agents/y.md", []byte("---\nname: y\ndescription: added dir\n---\nbody"))

	reg := Load(fs, LoadOptions{ProjectDir: "/proj", AdditionalDirs: []string{"/added"}})
	y, _ := reg.Lookup("y")
	if y.WhenToUse != "main project" || y.FromAdditionalDirectory {
		t.Errorf("y = %q fromAdditional=%v, want main project to win", y.WhenToUse, y.FromAdditionalDirectory)
	}
}

func TestParseAgentsJSON(t *testing.T) {
	data := []byte(`{
      "deployer": {
        "description": "Deploys the app",
        "tools": ["Bash", "Read"],
        "prompt": "You deploy things.",
        "model": "inherit",
        "effort": 2
      },
      "-bad": {"description": "d", "prompt": "p"}
    }`)
	descs, problems := ParseAgentsJSON(data)
	if len(descs) != 1 {
		t.Fatalf("got %d descriptors, want 1 (bad name skipped)", len(descs))
	}
	if len(problems) != 1 {
		t.Errorf("problems = %v, want one for '-bad'", problems)
	}
	d := descs[0]
	if d.AgentType != "deployer" || d.Source != SourceFlag {
		t.Errorf("agent = %s/%s", d.AgentType, d.Source)
	}
	if d.SystemPrompt != "You deploy things." {
		t.Errorf("prompt = %q", d.SystemPrompt)
	}
	if d.Model != "inherit" {
		t.Errorf("model = %q", d.Model)
	}
	if d.Effort != "2" {
		t.Errorf("effort = %q, want integer string", d.Effort)
	}
	if !reflect.DeepEqual(d.Tools, []string{"Bash", "Read"}) {
		t.Errorf("tools = %v", d.Tools)
	}
}

func TestInvalidEnumValuesDropped(t *testing.T) {
	md := "---\nname: a\ndescription: d\ncolor: chartreuse\nmodel: sonnet\nmemory: nonsense\nisolation: worktree\n---\nbody"
	pr := ParseDefinition([]byte(md), "a.md")
	d, msg := BuildFileDescriptor(pr, "a.md", "/b", SourceProject)
	if msg != "" {
		t.Fatalf("rejected: %s", msg)
	}
	if d.Color != "" {
		t.Errorf("invalid color kept: %q", d.Color)
	}
	if d.Memory != "" {
		t.Errorf("invalid memory kept: %q", d.Memory)
	}
	if d.Isolation != "worktree" {
		t.Errorf("valid isolation dropped: %q", d.Isolation)
	}
}

func TestExploreAndPlanDisallowExitPlanMode(t *testing.T) {
	for _, d := range BuiltinAgents() {
		if d.AgentType != AgentExplore && d.AgentType != AgentPlan {
			continue
		}
		has := map[string]bool{}
		for _, n := range d.DisallowedTools {
			has[n] = true
		}
		for _, n := range []string{"Agent", "ExitPlanMode", "Edit", "Write", "NotebookEdit"} {
			if !has[n] {
				t.Errorf("%s should disallow %s", d.AgentType, n)
			}
		}
	}
}
