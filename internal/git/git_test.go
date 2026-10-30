package git

import (
	"reflect"
	"strings"
	"testing"

	envtrust "github.com/umairge66/gocode/internal/envtrust"
)

func TestBaseConfigArgs(t *testing.T) {
	want := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor="}
	if !reflect.DeepEqual(BaseConfigArgs, want) {
		t.Fatalf("BaseConfigArgs = %v, want %v", BaseConfigArgs, want)
	}
}

func TestStatusPorcelainArgs(t *testing.T) {
	want := []string{"status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=dirty"}
	if !reflect.DeepEqual(StatusPorcelainArgs, want) {
		t.Fatalf("StatusPorcelainArgs = %v, want %v", StatusPorcelainArgs, want)
	}
}

func TestRefArgs(t *testing.T) {
	if got := CurrentBranchArgs(); !reflect.DeepEqual(got, []string{"symbolic-ref", "--short", "-q", "HEAD"}) {
		t.Fatalf("CurrentBranchArgs = %v", got)
	}
}

func TestSanitizedEnvHardeningDefaults(t *testing.T) {
	env := SanitizedEnv(envtrust.Env{}, nil, envtrust.Linux)
	checks := map[string]string{
		"GIT_ALLOW_PROTOCOL":     "/",
		"GIT_NO_LAZY_FETCH":      "1",
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_LFS_SKIP_SMUDGE":    "1",
		"GIT_SSH_COMMAND":        "false",
	}
	for k, want := range checks {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
	// GIT_CONFIG_PARAMETERS must carry the no-op remote aliases and autocorrect off.
	params := env["GIT_CONFIG_PARAMETERS"]
	for _, frag := range []string{"'alias.remote-none=version'", "'help.autocorrect=0'", "'fetch.bundleURI='"} {
		if !strings.Contains(params, frag) {
			t.Errorf("GIT_CONFIG_PARAMETERS missing %q: %q", frag, params)
		}
	}
}

func TestSanitizedEnvSSHAllowed(t *testing.T) {
	// When the caller explicitly allows ssh, the SSH-disabling override is not applied.
	env := SanitizedEnv(envtrust.Env{}, envtrust.Env{"GIT_ALLOW_PROTOCOL": "/:ssh"}, envtrust.Linux)
	if _, ok := env["GIT_SSH_COMMAND"]; ok {
		t.Fatalf("GIT_SSH_COMMAND should be absent when ssh is allowed, got %q", env["GIT_SSH_COMMAND"])
	}
	// ssh transport gets its own no-op aliases.
	if !strings.Contains(env["GIT_CONFIG_PARAMETERS"], "'alias.remote-ssh=version'") {
		t.Fatalf("expected ssh alias in params: %q", env["GIT_CONFIG_PARAMETERS"])
	}
}

func TestSanitizedEnvScrubsGitVars(t *testing.T) {
	// SanitizedEnv is git-specific: it clears the per-invocation git
	// variables and the indexed GIT_CONFIG_KEY/VALUE pairs, sanitizes PATH, and
	// leaves everything else (loader vars, HOME) to the separate wrappers.
	base := envtrust.Env{
		"PATH":               "/usr/bin:relative/bin:/bin",
		"GIT_DIR":            "/somewhere",    // per-invocation git var -> scrubbed
		"GIT_CONFIG_GLOBAL":  "/attacker/cfg", // config injection -> scrubbed
		"GIT_CONFIG_KEY_0":   "core.pager",    // indexed config pair -> scrubbed
		"GIT_CONFIG_VALUE_0": "sh -c evil",
		"GIT_WORK_TREE":      "/elsewhere", // scrubbed
		"HOME":               "/home/user", // left for the loader/home wrapper
		"KEEP_ME":            "yes",
	}
	env := SanitizedEnv(base, nil, envtrust.Linux)
	for _, gone := range []string{"GIT_DIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_WORK_TREE"} {
		if _, ok := env[gone]; ok {
			t.Errorf("expected %q to be scrubbed, present as %q", gone, env[gone])
		}
	}
	if env["KEEP_ME"] != "yes" {
		t.Errorf("unrelated var KEEP_ME dropped")
	}
	if env["HOME"] != "/home/user" {
		t.Errorf("HOME should be left for the wrapper, got %q", env["HOME"])
	}
	// PATH keeps only absolute entries.
	if env["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want %q", env["PATH"], "/usr/bin:/bin")
	}
}

func TestSanitizedEnvEmptyPathFallsBack(t *testing.T) {
	env := SanitizedEnv(envtrust.Env{"PATH": "relative:also/relative"}, nil, envtrust.Linux)
	if env["PATH"] != "/dev/null" {
		t.Fatalf("empty-of-absolutes PATH = %q, want /dev/null", env["PATH"])
	}
}

func TestHardenedEnv(t *testing.T) {
	// HardenedEnv composes the git-var layer (SanitizedEnv) with the envtrust
	// loader/trace/config hardening the worktree flows rely on.
	base := envtrust.Env{
		"PATH":              "/usr/bin",
		"HOME":              "/home/user",
		"LD_PRELOAD":        "/tmp/evil.so",    // dangerous loader var -> stripped
		"GIT_DIR":           "/somewhere",      // per-invocation git var -> cleared
		"GIT_CONFIG_GLOBAL": "/trusted/gitcfg", // re-introduced by passthrough
		"KEEP_ME":           "yes",
	}
	env := HardenedEnv(base, nil, envtrust.Linux)

	// 1. Dangerous loader variable is stripped.
	if _, ok := env["LD_PRELOAD"]; ok {
		t.Errorf("LD_PRELOAD should be stripped, present as %q", env["LD_PRELOAD"])
	}
	// 2. Git-trace-silencing variables are set to their silencing defaults.
	traceWant := map[string]string{
		"GIT_TRACE2":               "0",
		"GIT_TRACE2_EVENT":         "0",
		"GIT_TRACE2_PERF":          "0",
		"GIT_TRACE2_ENV_VARS":      "-",
		"GIT_TRACE2_CONFIG_PARAMS": "-",
		"GIT_TRACE2_MAX_FILES":     "0",
	}
	for k, want := range traceWant {
		if env[k] != want {
			t.Errorf("trace-silencing env[%q] = %q, want %q", k, env[k], want)
		}
	}
	// 3. Per-invocation GIT_* variables from the base are still cleared by the
	// git-var layer.
	if _, ok := env["GIT_DIR"]; ok {
		t.Errorf("GIT_DIR should be cleared, present as %q", env["GIT_DIR"])
	}
	// Config passthrough re-introduces the trusted global config the git-var
	// layer would otherwise scrub.
	if env["GIT_CONFIG_GLOBAL"] != "/trusted/gitcfg" {
		t.Errorf("GIT_CONFIG_GLOBAL = %q, want %q (passthrough)", env["GIT_CONFIG_GLOBAL"], "/trusted/gitcfg")
	}
	// The git-var hardening defaults still apply.
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", env["GIT_TERMINAL_PROMPT"])
	}
	// Unrelated vars survive.
	if env["KEEP_ME"] != "yes" || env["HOME"] != "/home/user" {
		t.Errorf("unrelated vars dropped: KEEP_ME=%q HOME=%q", env["KEEP_ME"], env["HOME"])
	}
}

func TestHardenedEnvCallerOverrideWins(t *testing.T) {
	// An explicit caller override beats the folded-in trace-silencing default.
	env := HardenedEnv(envtrust.Env{}, envtrust.Env{"GIT_TRACE2": "1"}, envtrust.Linux)
	if env["GIT_TRACE2"] != "1" {
		t.Fatalf("caller override GIT_TRACE2 = %q, want 1", env["GIT_TRACE2"])
	}
}

func TestEnvSliceSortedKeyValue(t *testing.T) {
	got := EnvSlice(envtrust.Env{"B": "2", "A": "1"})
	if !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("EnvSlice = %v", got)
	}
}

func TestSanitizedEnvInvalidAllowProtocolFailsClosed(t *testing.T) {
	// A transport name outside [A-Za-z0-9-] fails closed to the hardened
	// default so nothing unaliased is allowed.
	env := SanitizedEnv(envtrust.Env{}, envtrust.Env{"GIT_ALLOW_PROTOCOL": "https:ex t;rm"}, envtrust.Linux)
	if got := env["GIT_ALLOW_PROTOCOL"]; got != AllowProtocolDefault {
		t.Fatalf("GIT_ALLOW_PROTOCOL = %q, want %q", got, AllowProtocolDefault)
	}
	if strings.Contains(env["GIT_CONFIG_PARAMETERS"], "remote-https") || strings.Contains(env["GIT_CONFIG_PARAMETERS"], "ex t") {
		t.Fatalf("unexpected aliases: %q", env["GIT_CONFIG_PARAMETERS"])
	}
	// A valid list is kept and aliased.
	env = SanitizedEnv(envtrust.Env{}, envtrust.Env{"GIT_ALLOW_PROTOCOL": "https:file"}, envtrust.Linux)
	if env["GIT_ALLOW_PROTOCOL"] != "https:file" || !strings.Contains(env["GIT_CONFIG_PARAMETERS"], "'alias.remote-https=version'") {
		t.Fatalf("valid list: %q / %q", env["GIT_ALLOW_PROTOCOL"], env["GIT_CONFIG_PARAMETERS"])
	}
}
