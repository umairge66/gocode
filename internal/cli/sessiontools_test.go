package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBrowserToolsEnabledPrecedence(t *testing.T) {
	cases := []struct {
		chrome, cfc string
		interactive bool
		want        bool
	}{
		{"", "", true, true},
		{"", "", false, false},
		{"on", "", false, true},
		{"off", "", true, false},
		{"", "1", false, true},
		{"", "false", true, false},
		{"off", "1", false, false}, // the flag wins
		{"on", "0", false, true},
	}
	for _, c := range cases {
		env := &Env{Getenv: func(k string) string {
			if k == "GOCODE_CODE_ENABLE_CFC" {
				return c.cfc
			}
			return ""
		}}
		got := browserToolsEnabled(env, &printSession{Chrome: c.chrome}, c.interactive)
		if got != c.want {
			t.Errorf("chrome=%q cfc=%q interactive=%v: got %v, want %v", c.chrome, c.cfc, c.interactive, got, c.want)
		}
	}
}

func TestUnofferedTools(t *testing.T) {
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	env := &Env{Getenv: func(k string) string {
		if k == "BRAVE_SEARCH_API_KEY" {
			return "brave-test"
		}
		return ""
	}}
	print := unofferedTools(env, &printSession{}, false)
	if !slices.Contains(print, "AskUserQuestion") || !slices.Contains(print, "AmendAgent") || !slices.Contains(print, "navigate") || !slices.Contains(print, "browser_batch") {
		t.Fatalf("print session offers %v", print)
	}
	if got := unofferedTools(env, &printSession{}, true); len(got) != 0 {
		t.Fatalf("interactive session leaves out %v", got)
	}
	if got := unofferedTools(env, &printSession{Chrome: "on"}, false); !slices.Equal(got, []string{"AskUserQuestion", "AmendAgent", "EnterPlanMode", "ExitPlanMode"}) {
		t.Fatalf("print --chrome leaves out %v", got)
	}
}

// TestUnofferedTools_WebSearchNeedsBackend: without a search backend the
// WebSearch tool is not offered.
func TestUnofferedTools_WebSearchNeedsBackend(t *testing.T) {
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	env := &Env{Getenv: func(string) string { return "" }}
	if got := unofferedTools(env, &printSession{}, true); !slices.Equal(got, []string{"WebSearch"}) {
		t.Fatalf("interactive session without a search key leaves out %v", got)
	}
	env = &Env{Getenv: func(k string) string {
		if k == "BRAVE_SEARCH_API_KEY" {
			return "  "
		}
		return ""
	}}
	if got := unofferedTools(env, &printSession{}, true); !slices.Contains(got, "WebSearch") {
		t.Fatalf("blank key still offers WebSearch: %v", got)
	}
}

func TestParseRootConfigChromeFlags(t *testing.T) {
	for args, want := range map[string]string{
		"":                        "",
		"--chrome":                "on",
		"--no-chrome":             "off",
		"--chrome --no-chrome":    "off",
		"--no-chrome -p --chrome": "on",
	} {
		r := parseRootConfig(strings.Fields(args))
		if r.Config.Chrome != want {
			t.Errorf("%q: Chrome = %q, want %q", args, r.Config.Chrome, want)
		}
		if slices.Contains(r.Rest, "--chrome") || slices.Contains(r.Rest, "--no-chrome") {
			t.Errorf("%q: flag left in Rest %v", args, r.Rest)
		}
	}
}

func TestToolResultsDir(t *testing.T) {
	home := t.TempDir()
	env := &Env{Getenv: func(k string) string {
		if k == "GOCODE_CONFIG_DIR" {
			return home
		}
		return ""
	}}
	got := toolResultsDir(env, sessionPaths{cwd: "/work/proj"}, "sess-1")
	want := filepath.Join(home, "projects", "-work-proj", "sess-1", "tool-results")
	if got != want {
		t.Fatalf("toolResultsDir = %q, want %q", got, want)
	}
	if toolResultsDir(&Env{Getenv: func(string) string { return "" }}, sessionPaths{cwd: "/w"}, "s") != "" {
		t.Fatal("results dir without a config home")
	}
}
