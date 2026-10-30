package settingsload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// localCtx builds a Context whose local source resolves under a temp project
// directory (projectBaseDir/localBaseDir both fall back to Cwd).
func localCtx(t *testing.T) (*Context, string) {
	t.Helper()
	dir := t.TempDir()
	return &Context{Cwd: dir, Env: func(string) string { return "" }}, dir
}

func readLocal(t *testing.T, ctx *Context) map[string]any {
	t.Helper()
	path, ok := ctx.PathForSource(settingspolicy.SourceLocal)
	if !ok {
		t.Fatal("no local settings path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse settings: %v (%s)", err, data)
	}
	return obj
}

func allowList(t *testing.T, obj map[string]any) []string {
	t.Helper()
	perms, _ := obj["permissions"].(map[string]any)
	raw, _ := perms["allow"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func TestAddPermissionRuleCreatesFile(t *testing.T) {
	ctx, dir := localCtx(t)

	added, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Bash(npm run test:*)")
	if err != nil {
		t.Fatalf("AddPermissionRule: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true for a new rule")
	}

	// The file lands under <cwd>/.gocode/settings.local.json.
	want := filepath.Join(dir, ".gocode", "settings.local.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("settings file not created at %s: %v", want, err)
	}

	got := allowList(t, readLocal(t, ctx))
	if len(got) != 1 || got[0] != "Bash(npm run test:*)" {
		t.Fatalf("allow = %v, want [Bash(npm run test:*)]", got)
	}
}

func TestAddPermissionRulePreservesUnknownKeys(t *testing.T) {
	ctx, dir := localCtx(t)
	path := filepath.Join(dir, ".gocode", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-written file with unrelated keys and a pre-existing allow entry.
	seed := `{
  "model": "claude-opus",
  "env": {"FOO": "bar"},
  "permissions": {"allow": ["Read(//tmp/**)"], "deny": ["Bash(rm:*)"]}
}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "WebFetch(domain:example.com)")
	if err != nil {
		t.Fatalf("AddPermissionRule: %v", err)
	}
	if !added {
		t.Fatal("added = false, want true")
	}

	obj := readLocal(t, ctx)
	if obj["model"] != "claude-opus" {
		t.Errorf("model key lost: %v", obj["model"])
	}
	env, _ := obj["env"].(map[string]any)
	if env == nil || env["FOO"] != "bar" {
		t.Errorf("env key lost: %v", obj["env"])
	}
	perms, _ := obj["permissions"].(map[string]any)
	deny, _ := perms["deny"].([]any)
	if len(deny) != 1 || deny[0] != "Bash(rm:*)" {
		t.Errorf("deny list lost: %v", perms["deny"])
	}
	got := allowList(t, obj)
	if len(got) != 2 || got[0] != "Read(//tmp/**)" || got[1] != "WebFetch(domain:example.com)" {
		t.Errorf("allow = %v, want [Read(//tmp/**) WebFetch(domain:example.com)]", got)
	}
}

func TestAddPermissionRuleDeduplicates(t *testing.T) {
	ctx, _ := localCtx(t)

	if _, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Bash(ls:*)"); err != nil {
		t.Fatal(err)
	}
	added, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Bash(ls:*)")
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if added {
		t.Fatal("added = true on duplicate, want false (no-op)")
	}
	got := allowList(t, readLocal(t, ctx))
	if len(got) != 1 {
		t.Fatalf("allow = %v, want a single entry after de-dup", got)
	}
}

func TestAddPermissionRuleRoundTripsThroughLoader(t *testing.T) {
	ctx, _ := localCtx(t)
	if _, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Edit(//src/**)"); err != nil {
		t.Fatal(err)
	}
	// The loader must read the persisted rule straight back.
	res := LoadSource(fsfacade.OSFS{}, ctx, settingspolicy.SourceLocal)
	if res.LoadState != settingspolicy.LoadLoaded {
		t.Fatalf("load state = %v, want loaded", res.LoadState)
	}
	perms, _ := res.Settings["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Edit(//src/**)" {
		t.Fatalf("loaded allow = %v, want [Edit(//src/**)]", perms["allow"])
	}
}

func TestAddPermissionRuleRejectsMalformedFile(t *testing.T) {
	ctx, dir := localCtx(t)
	path := filepath.Join(dir, ".gocode", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[1,2,3]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "Bash(ls)"); err == nil {
		t.Fatal("expected an error refusing to overwrite a non-object settings file")
	}
	// The original file is untouched.
	data, _ := os.ReadFile(path)
	if string(data) != "[1,2,3]" {
		t.Fatalf("malformed file was modified: %q", data)
	}
}

func TestAddPermissionRuleEmptyIsNoop(t *testing.T) {
	ctx, _ := localCtx(t)
	added, err := AddPermissionRule(ctx, settingspolicy.SourceLocal, PermissionAllow, "")
	if err != nil || added {
		t.Fatalf("empty rule: added=%v err=%v, want false,nil", added, err)
	}
}
