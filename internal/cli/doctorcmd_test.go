package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doctorTestSeams(exe string) doctorSeams {
	return doctorSeams{
		executable: func() (string, error) { return exe, nil },
		goos:       "linux",
		goarch:     "amd64",
		chrome: func(*Env) (string, bool) {
			return "Not installed (no host manifest at /x/com.gocode.gocode.json)\n", false
		},
	}
}

func TestDoctorReport(t *testing.T) {
	f := newSubcmdFixture(t)
	bin := filepath.Join(f.home, "bin")
	exe := filepath.Join(bin, "gocode")
	subcmdWriteFile(t, exe, "#!/bin/sh\n")
	other := filepath.Join(f.home, "other")
	subcmdWriteFile(t, filepath.Join(other, "gocode"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(other, "gocode"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.vars["PATH"] = other
	f.vars["SHELL"] = "/bin/zsh"
	f.vars["OPENROUTER_API_KEY"] = "sk-or-SECRET-VALUE"
	f.vars["BASH_MAX_OUTPUT_LENGTH"] = "abc"
	subcmdWriteFile(t, f.userSettingsPath(), `{"bogusKey":1,"permissions":{"allow":["bash(ls)","WebFetch(https://x.y)","Write(src/**)","mcp__*"],"deny":["Read(src/:*)"]},"enabledPlugins":{"ghost@market":true}}`)
	subcmdWriteFile(t, filepath.Join(f.cwd, ".gocode", "settings.json"), `{not json`)

	env, out, _ := f.env()
	if code := runDoctor(env, nil, doctorTestSeams(exe)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{
		"gocode doctor\n\nRunning: native (",
		"Platform: linux-x64",
		"Path: " + exe,
		"Search: OK (",
		"Provider: openrouter",
		"API key (openrouter): found (source: OPENROUTER_API_KEY)",
		"API key (anthropic): not set",
		"Invalid settings",
		"Unrecognized field: bogusKey",
		"permissions.allow.0: Tool names must start with uppercase. Use \"Bash\"",
		"permissions.allow.1: WebFetch permissions use domain format, not URLs",
		"permissions.allow.3: Wildcard tool name \"mcp__*\" is not supported in allow rules",
		"permissions.deny.0: The \":*\" syntax is only for Bash prefix rules",
		"Settings file is not valid JSON",
		"- Write(src/**) is not matched by file permission checks — only Edit(path) rules are. Use Edit(src/**) instead",
		"Environment variables\n- BASH_MAX_OUTPUT_LENGTH: Invalid value \"abc\" (using default: 30000)",
		"Multiple installations found\n- native at " + exe + "\n- native at " + filepath.Join(other, "gocode"),
		"Plugins\n- \"ghost@market\" is enabled in settings but not installed",
		"Chrome native host: not installed",
		"1 warning found\n- Native installation exists but " + bin + " is not in your PATH",
		"source ~/.zshrc",
		"For a full setup checkup that can also fix issues, run /doctor in a gocode session.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Fatal("doctor printed an API key value")
	}
}

func TestDoctorCleanReportAndRouting(t *testing.T) {
	f := newSubcmdFixture(t)
	bin := filepath.Join(f.home, "bin")
	exe := filepath.Join(bin, "gocode")
	subcmdWriteFile(t, exe, "")
	f.vars["PATH"] = bin
	f.vars["OPENROUTER_API_KEY"] = "k"
	env, out, _ := f.env()
	if code := runDoctor(env, nil, doctorTestSeams(exe)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := out.String(); !strings.Contains(got, "No installation issues found.") || strings.Contains(got, "Invalid settings") || strings.Contains(got, "Multiple installations") {
		t.Fatalf("clean report:\n%s", got)
	}
	// Routed through Run, the doctor exits 0 and prints the header.
	code, routed, _ := f.run(t, "doctor")
	if code != 0 || !strings.HasPrefix(routed, "gocode doctor\n") {
		t.Fatalf("routed exit %d out %q", code, routed)
	}
}

func TestEnvVarCheck(t *testing.T) {
	cases := []struct {
		raw, status, msg string
	}{
		{"", "valid", ""},
		{"20000", "valid", ""},
		{"0", "invalid", `Invalid value "0" (using default: 30000)`},
		{"200000", "capped", "Capped from 200000 to 150000"},
	}
	for _, c := range cases {
		status, msg := envVarCheck(c.raw, 30000, 150000)
		if status != c.status || msg != c.msg {
			t.Errorf("envVarCheck(%q) = %q %q", c.raw, status, msg)
		}
	}
	if s, _ := envVarCheck("999999", 0, 0); s != "valid" {
		t.Error("no ceiling should not cap")
	}
}

func TestValidatePermissionRule(t *testing.T) {
	ok := []struct{ rule, behavior string }{
		{"Bash", "allow"}, {"Bash(git status)", "allow"}, {"mcp__github__get_*", "allow"}, {"mcp__*", "deny"},
		{"WebFetch(domain:example.com)", "allow"}, {"Read(src/**)", "deny"}, {"Edit(*)", "allow"},
	}
	for _, c := range ok {
		if p := validatePermissionRule(c.rule, c.behavior); p != nil {
			t.Errorf("%s (%s): unexpected %q", c.rule, c.behavior, p.message())
		}
	}
	bad := map[string]string{
		"":                  "Permission rule cannot be empty",
		"Bash(ls":           "Malformed Tool(content) rule",
		"Bash()":            "Empty parentheses",
		"()":                "Empty parentheses with no tool name",
		"mcp__srv__x(y)":    "MCP rules do not support patterns in parentheses",
		"Bash(:*)":          "Prefix cannot be empty before :*",
		"WebSearch(foo*)":   "WebSearch does not support wildcards",
		"WebFetch(foo.com)": `WebFetch permissions must use "domain:" prefix`,
	}
	for rule, want := range bad {
		p := validatePermissionRule(rule, "deny")
		if p == nil || p.Warning || !strings.HasPrefix(p.message(), want) {
			t.Errorf("%q: got %+v, want %q", rule, p, want)
		}
	}
	if p := validatePermissionRule("Glob(src/**)", "deny"); p == nil || !p.Warning || !strings.Contains(p.message(), "Use Read(src/**) instead") {
		t.Errorf("Glob warning = %+v", p)
	}
}
