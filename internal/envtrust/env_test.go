package envtrust

import "testing"

func TestLookupEnv(t *testing.T) {
	env := Env{"PATH": "/bin", "Path": "/other"}
	// Exact match wins regardless of platform.
	if v, ok := LookupEnv(env, "PATH", Linux); !ok || v != "/bin" {
		t.Errorf("exact = (%q,%v)", v, ok)
	}
	// Linux: no case-insensitive fallback.
	if _, ok := LookupEnv(Env{"PATH": "/bin"}, "path", Linux); ok {
		t.Error("linux should not match case-insensitively")
	}
	// Windows: case-insensitive fallback.
	if v, ok := LookupEnv(Env{"PATH": "/bin"}, "path", Windows); !ok || v != "/bin" {
		t.Errorf("windows ci = (%q,%v)", v, ok)
	}
	if _, ok := LookupEnv(env, "MISSING", Windows); ok {
		t.Error("missing should report false")
	}
}

func TestGitTraceSilencingEnv(t *testing.T) {
	got := GitTraceSilencingEnv(Env{"GIT_TRACE2_EVENT": "1"}, Linux)
	if got["GIT_TRACE2"] != "0" {
		t.Errorf("default silencing missing: %q", got["GIT_TRACE2"])
	}
	if got["GIT_TRACE2_EVENT"] != "1" {
		t.Errorf("existing non-empty value should be preserved, got %q", got["GIT_TRACE2_EVENT"])
	}
	if got["GIT_TRACE2_ENV_VARS"] != "-" {
		t.Errorf("env-vars default should be '-', got %q", got["GIT_TRACE2_ENV_VARS"])
	}
	// Empty value falls back to the default.
	got2 := GitTraceSilencingEnv(Env{"GIT_TRACE2": ""}, Linux)
	if got2["GIT_TRACE2"] != "0" {
		t.Errorf("empty value should fall back, got %q", got2["GIT_TRACE2"])
	}
}

func TestGitConfigPassthroughEnv(t *testing.T) {
	got := GitConfigPassthroughEnv(Env{"GIT_CONFIG_GLOBAL": "/g"}, Linux)
	if got["GIT_CONFIG_GLOBAL"] != "/g" {
		t.Errorf("global passthrough = %q", got["GIT_CONFIG_GLOBAL"])
	}
	if _, present := got["GIT_CONFIG_SYSTEM"]; present {
		t.Error("absent variable should be omitted, not set empty")
	}
}
