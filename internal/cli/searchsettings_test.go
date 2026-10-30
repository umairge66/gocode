package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/ui"
)

func TestEffectiveSearchTogglesDefaults(t *testing.T) {
	got := effectiveSearchToggles(filetools.SearchSettings{})
	want := map[string]bool{
		searchOptUseUgrep: true, searchOptPreferSystem: false,
		searchOptIncludeHidden: true, searchOptGitignore: true,
	}
	for _, tg := range got {
		if tg.on != want[tg.value] {
			t.Errorf("default %s = %v, want %v", tg.value, tg.on, want[tg.value])
		}
	}
}

// /search opens a checkbox dialog pre-marked from the effective settings;
// unticking "Use ugrep" and confirming saves search.useUgrep=false to the
// local settings (keeping other keys) and switches the live engine to built-in.
func TestSearchCommandTogglesAndSavesLive(t *testing.T) {
	dir := t.TempDir()
	env, _, _ := dialogEnv(dir, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceLocal)
	if !ok {
		t.Fatal("no local settings path")
	}
	if err := writeLocalSettingsObject(path, map[string]any{
		"model":  "keep-me",
		"search": map[string]any{"ugrepPath": "/opt/custom/ugrep"},
	}); err != nil {
		t.Fatal(err)
	}

	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	go func() { runInteractiveSession(env, sess, "") }()
	prog := mustProgram(t, progCh)
	defer prog.Quit()
	prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)

	submitLine(prog, "/search")
	var dlg *ui.Dialog
	if !prog.waitFor(func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		if ok && d.Dialog != nil && d.Dialog.MultiSelect && len(d.Dialog.Options) == 4 {
			dlg = d.Dialog
			return true
		}
		return false
	}, 2*time.Second) {
		t.Fatal("/search did not open the 4-checkbox settings dialog")
	}
	if !dlg.AllowNone || !dlg.IsMarked(0) || dlg.IsMarked(1) || !dlg.IsMarked(2) || !dlg.IsMarked(3) {
		t.Fatalf("dialog not pre-marked from defaults: marked=%v allowNone=%v", dlg.Marked, dlg.AllowNone)
	}

	// The first option (Use ugrep) is highlighted: Space unticks it, Enter saves.
	prog.press(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !sawSystemMsg(prog, "Search settings saved", "ugrep off", "Engine: built-in") {
		t.Fatal("no confirmation showing ugrep off and the built-in engine")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	search, _ := obj["search"].(map[string]any)
	if search["useUgrep"] != false || search["includeHidden"] != true || obj["respectGitignore"] != true {
		t.Fatalf("saved settings wrong: %s", data)
	}
	if search["ugrepPath"] != "/opt/custom/ugrep" || obj["model"] != "keep-me" {
		t.Fatalf("other settings were not preserved: %s", data)
	}
}

func TestSearchEngineApplyIsLive(t *testing.T) {
	e := filetools.NewSearchEngine(filetools.SearchSettings{})
	off := false
	e.Apply(filetools.SearchSettings{UseUgrep: &off})
	if d := e.Describe(); !strings.HasPrefix(d, "built-in") {
		t.Fatalf("after Apply(useUgrep=false) engine = %q, want built-in", d)
	}
}
