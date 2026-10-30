package hookhost

import (
	"testing"

	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// TestConfiguredListsHooksBySourceAndEvent asserts Configured reports every
// registered hook with its event, raw matcher and source, events in name
// order and hooks in registration order.
func TestConfiguredListsHooksBySourceAndEvent(t *testing.T) {
	fsys, p := pluginFS(t, "/plugins/demo", `{"description":"d","hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"plugin"}]}]}}`)
	h := New(Options{
		Settings: []SettingsLayer{
			layer(settingspolicy.SourceUser, hooksMap("PreToolUse", "Edit", "user")),
			layer(settingspolicy.SourceProject, hooksMap("PostToolUse", "", "project")),
		},
		Plugins: []*plugins.Plugin{p},
		FS:      fsys,
		Runner:  &fakeRunner{},
	})
	got := h.Configured()
	if len(got) != 3 {
		t.Fatalf("Configured = %+v, want 3 hooks", got)
	}
	if got[0].Event != hooks.EventPostToolUse || got[0].Source != settingspolicy.SourceProject || got[0].Hook.Command != "project" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Source != SourcePlugin || got[1].Plugin != p.Name() || got[1].Matcher != "Bash" {
		t.Fatalf("plugin hook = %+v", got[1])
	}
	if got[2].Event != hooks.EventPreToolUse || got[2].Matcher != "Edit" || got[2].Source != settingspolicy.SourceUser {
		t.Fatalf("user hook = %+v", got[2])
	}
	var nilHost *Host
	if nilHost.Configured() != nil {
		t.Fatal("a nil host lists hooks")
	}
}
