package cli

import (
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/ui"
)

// TestResolveUISettings: the `ui` settings object selects the style and icon
// set, a higher-precedence file overrides it, GOCODE_ICONS overrides the icon
// setting, and unknown values fall back to modern + Unicode.
func TestResolveUISettings(t *testing.T) {
	env, home, cwd := settingsTestEnv(t)
	if s := resolveUISettings(env); s.style != ui.StyleModern || s.icons != ui.IconsUnicode ||
		!s.animations || s.sidebar != ui.SidebarAuto || s.theme != "" {
		t.Fatalf("defaults = %+v, want modern/unicode/animations/auto sidebar", s)
	}

	writeSettingsFile(t, userSettingsPath(home), `{"ui":{"style":"classic","icons":"nerd","animations":false,"sidebar":"off","theme":"ember"}}`)
	if s := resolveUISettings(env); s.style != ui.StyleClassic || s.icons != ui.IconsNerd ||
		s.animations || s.sidebar != ui.SidebarOff || s.theme != "ember" {
		t.Fatalf("user settings = %+v, want classic/nerd/no animations/sidebar off/ember", s)
	}

	writeSettingsFile(t, filepath.Join(cwd, ".gocode", "settings.json"), `{"ui":{"style":"Cards","icons":"bogus","sidebar":"bogus","animations":"on"}}`)
	if s := resolveUISettings(env); s.style != ui.StyleCards || s.icons != ui.IconsNerd ||
		!s.animations || s.sidebar != ui.SidebarOff {
		t.Fatalf("project override = %+v, want cards/nerd (bogus icons and sidebar ignored), animations on", s)
	}

	env.Getenv = func(k string) string {
		switch k {
		case "HOME":
			return home
		case ui.IconsEnv:
			return "ascii"
		}
		return ""
	}
	if s := resolveUISettings(env); s.icons != ui.IconsASCII {
		t.Fatalf("GOCODE_ICONS=ascii gave %q", s.icons)
	}
}

// TestChooseTheme: `ui.theme` overrides `theme`; a concrete theme is kept;
// auto/unset follows the terminal background between the Prompty pair in the
// modern style (the classic pair for "auto" otherwise).
func TestChooseTheme(t *testing.T) {
	cases := []struct {
		setting, uiTheme string
		style            ui.UIStyle
		want             themeChoice
	}{
		{"", "", ui.StyleModern, themeChoice{name: "prompty", autoDark: "prompty", autoLight: "prompty-light"}},
		{"auto", "", ui.StyleModern, themeChoice{name: "prompty", autoDark: "prompty", autoLight: "prompty-light"}},
		{"", "auto", ui.StyleModern, themeChoice{name: "prompty", autoDark: "prompty", autoLight: "prompty-light"}},
		{"nope", "", ui.StyleModern, themeChoice{name: "prompty", autoDark: "prompty", autoLight: "prompty-light"}},
		{"midnight", "", ui.StyleModern, themeChoice{name: "midnight"}},
		{"", "daylight", ui.StyleModern, themeChoice{name: "daylight"}},
		{"prompty-light", "", ui.StyleModern, themeChoice{name: "prompty-light"}},
		{"dark", "", ui.StyleModern, themeChoice{name: "dark"}},
		{"light", "", ui.StyleModern, themeChoice{name: "light"}},
		{"light", "frost", ui.StyleModern, themeChoice{name: "frost"}},
		{"", "", ui.StyleCards, themeChoice{name: "dark"}},
		{"auto", "", ui.StyleClassic, themeChoice{name: "dark", autoDark: "dark", autoLight: "light"}},
		{"custom:x", "", ui.StyleCards, themeChoice{name: "dark"}},
	}
	for _, c := range cases {
		if got := chooseTheme(c.setting, c.uiTheme, c.style); got != c.want {
			t.Errorf("chooseTheme(%q, %q, %s) = %+v, want %+v", c.setting, c.uiTheme, c.style, got, c.want)
		}
	}
}

// TestPersistUISetting: a palette change is written to the user settings'
// `ui` object (animations as a boolean), keeping what is already there.
func TestPersistUISetting(t *testing.T) {
	env, home, _ := settingsTestEnv(t)
	writeSettingsFile(t, userSettingsPath(home), `{"theme":"dark","ui":{"icons":"nerd"}}`)
	persistUISetting(env, "ui.theme", "aurora")
	persistUISetting(env, "ui.animations", "off")
	s := resolveUISettings(env)
	if s.theme != "aurora" || s.animations || s.icons != ui.IconsNerd {
		t.Fatalf("after persisting: %+v", s)
	}
}

// TestTodosFromInput: a TodoWrite input becomes the sidebar's todo list (the
// active item by its activeForm); other tools are ignored.
func TestTodosFromInput(t *testing.T) {
	in := []byte(`{"todos":[{"content":"Write tests","status":"completed","activeForm":"Writing tests"},{"content":"Fix bug","status":"in_progress","activeForm":"Fixing the bug"}]}`)
	todos, ok := todosFromInput("TodoWrite", in)
	if !ok || len(todos) != 2 || todos[0].Text != "Write tests" || todos[1].Text != "Fixing the bug" || todos[1].Status != ui.TodoInProgress {
		t.Fatalf("todos = %+v, %v", todos, ok)
	}
	if _, ok := todosFromInput("Bash", in); ok {
		t.Error("a non-TodoWrite tool produced todos")
	}
}

// TestFooterCwd: the home directory is shown as "~".
func TestFooterCwd(t *testing.T) {
	env, home, _ := settingsTestEnv(t)
	for in, want := range map[string]string{
		home:                                    "~",
		filepath.Join(home, "src", "myproject"): filepath.Join("~", "src", "myproject"),
		filepath.Join(home+"x", "elsewhere"):    filepath.Join(home+"x", "elsewhere"),
		filepath.Join(string(filepath.Separator), "opt"): filepath.Join(string(filepath.Separator), "opt"),
	} {
		if got := footerCwd(env, in); got != want {
			t.Errorf("footerCwd(%q) = %q, want %q", in, got, want)
		}
	}
}
