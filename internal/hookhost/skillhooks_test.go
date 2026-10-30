package hookhost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/skills"
)

const skillWithHooks = "---\ndescription: Guarded\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: ${GOCODE_SKILL_DIR}/check.sh\n---\nBody.\n"

// A skill's frontmatter hooks (as parsed from SKILL.md YAML) register on
// invocation, run for matching events from then on with the skill dir
// expanded and exported, and register only once per skill.
func TestAddSkillHooksRegistersForSession(t *testing.T) {
	fm := skills.ParseSkillMD([]byte(skillWithHooks), "/s/guard/SKILL.md").Frontmatter
	if fm.Hooks == nil {
		t.Fatal("frontmatter hooks not parsed")
	}
	r := &fakeRunner{script: func(RunRequest) scripted { return scripted{exit: 2, stderr: "blocked by skill"} }}
	h := New(Options{Runner: r, BaseEnv: []string{}})
	if h.HasHooks(hooks.EventPreToolUse) {
		t.Fatal("no hooks expected before invocation")
	}
	n, err := h.AddSkillHooks("guard", "/s/guard", fm.Hooks, false)
	if err != nil || n != 1 {
		t.Fatalf("AddSkillHooks = %d, %v", n, err)
	}
	if n, _ := h.AddSkillHooks("guard", "/s/guard", fm.Hooks, false); n != 0 {
		t.Fatalf("second registration added %d hooks", n)
	}
	d, err := h.PreToolUse(context.Background(), "Bash", json.RawMessage(`{"command":"ls"}`))
	if err != nil || d == nil || d.Behavior != hooks.BehaviorDeny {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	cmds := r.commands()
	if len(cmds) != 1 || cmds[0] != "/s/guard/check.sh" {
		t.Fatalf("commands = %v", cmds)
	}
	if env := strings.Join(r.reqs[0].Env, "\n"); !strings.Contains(env, "GOCODE_SKILL_DIR=/s/guard") {
		t.Fatalf("skill dir not exported: %v", r.reqs[0].Env)
	}
}

// In managed-only mode a non-managed skill's hooks do not load.
func TestAddSkillHooksManagedOnly(t *testing.T) {
	fm := skills.ParseSkillMD([]byte(skillWithHooks), "/s/guard/SKILL.md").Frontmatter
	h := New(Options{Runner: &fakeRunner{}, BaseEnv: []string{}, Settings: []SettingsLayer{{
		Source: settingspolicy.SourcePolicy, Settings: map[string]any{"allowManagedHooksOnly": true},
	}}})
	if n, _ := h.AddSkillHooks("guard", "/s/guard", fm.Hooks, false); n != 0 || h.HasHooks(hooks.EventPreToolUse) {
		t.Fatalf("non-managed skill hooks loaded in managed-only mode (n=%d)", n)
	}
	if n, _ := h.AddSkillHooks("guard", "/s/guard", fm.Hooks, true); n != 1 {
		t.Fatalf("managed skill hooks not loaded (n=%d)", n)
	}
}
