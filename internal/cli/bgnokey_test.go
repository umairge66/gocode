package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/daemon"
)

// A fake provider key; never a real one.
const fakeBgKey = "sk-or-v1-FAKE-test-key-0000000000"

// bgKeyEnv is an isolated Env: HOME, XDG_CONFIG_HOME, GOCODE_CONFIG_DIR (the
// daemon dir) and the cwd are temp dirs. vars may add/override variables.
func bgKeyEnv(t *testing.T, vars map[string]string) (*Env, map[string]string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	all := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"PATH":            "/usr/bin:/bin",
	}
	for k, v := range vars {
		all[k] = v
	}
	var out, errb bytes.Buffer
	env := &Env{
		Getenv:             func(k string) string { return all[k] },
		Setenv:             func(string, string) {},
		Stdout:             &out,
		Stderr:             &errb,
		Cwd:                func() (string, error) { return cwd, nil },
		ManagedSettingsDir: filepath.Join(home, "managed"),
		trustCheck:         func(string) bool { return true },
	}
	return env, all, &out, &errb
}

// saveAuthKey writes key to the env's auth.json (as /login does).
func saveAuthKey(t *testing.T, env *Env, key string) {
	t.Helper()
	if err := auth.SaveProviderKey(authFilePath(env), auth.ProviderOpenRouter, key); err != nil {
		t.Fatal(err)
	}
}

// fakeDaemon serves the control socket of configDir, answers every request
// OK and records the raw dispatch payloads.
func fakeDaemon(t *testing.T, configDir string) func() [][]byte {
	t.Helper()
	l, err := daemon.ListenControl(configDir)
	if err != nil {
		t.Skipf("control socket unavailable: %v", err)
	}
	var mu sync.Mutex
	var got [][]byte
	ctx, cancel := context.WithCancel(context.Background())
	srv := &daemon.Server{Handler: daemon.HandlerFunc(func(_ context.Context, req daemon.Request, rw daemon.ReplyWriter) {
		if req.Op == daemon.OpDispatch {
			mu.Lock()
			got = append(got, append([]byte(nil), req.Dispatch...))
			mu.Unlock()
		}
		_ = rw.Send(daemon.Reply{OK: true, Op: req.Op})
	})}
	go func() { _ = srv.Serve(ctx, l) }()
	t.Cleanup(cancel)
	return func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

// The dispatch request sent to the daemon carries no provider key — neither
// the value nor any key variable name — and the caller is warned up front that
// its key exists only in its shell. The warning never shows the value.
func TestBgDispatchSendsNoKey(t *testing.T) {
	cfg := t.TempDir()
	env, _, _, errb := bgKeyEnv(t, map[string]string{
		"GOCODE_CONFIG_DIR":  cfg,
		"OPENROUTER_API_KEY": fakeBgKey,
		"ANTHROPIC_API_KEY":  fakeBgKey + "-a",
		"GOCODE_API_KEY":     fakeBgKey + "-g",
		"GOCODE_PROVIDER":    "openrouter",
	})
	// GOCODE_CONFIG_DIR also moves the user config: no auth.json there.
	payloads := fakeDaemon(t, cfg)

	if code := handleBgFlag(env, []string{"--bg", "hello"}); code != 0 {
		t.Fatalf("handleBgFlag = %d; stderr=%q", code, errb.String())
	}
	got := payloads()
	if len(got) != 1 {
		t.Fatalf("want 1 dispatch, got %d", len(got))
	}
	body := string(got[0])
	for _, bad := range []string{fakeBgKey, "API_KEY"} {
		if strings.Contains(body, bad) {
			t.Errorf("dispatch request contains %q: %s", bad, body)
		}
	}
	if !strings.Contains(body, `"GOCODE_PROVIDER":"openrouter"`) {
		t.Errorf("non-secret provider selection should still travel: %s", body)
	}
	warn := errb.String()
	if !strings.Contains(warn, "OPENROUTER_API_KEY is set only in this shell") || !strings.Contains(warn, "/login") {
		t.Errorf("missing up-front env-only warning: %q", warn)
	}
	if strings.Contains(warn, fakeBgKey) {
		t.Errorf("warning printed the key value")
	}
}

func TestBgDispatchEnvDropsSecrets(t *testing.T) {
	get := func(k string) string { return "v-" + k }
	m := bgDispatchEnv(get)
	for k := range m {
		if strings.Contains(k, "KEY") || strings.Contains(k, "TOKEN") {
			t.Errorf("secret name %q in dispatch env", k)
		}
	}
	if m["PATH"] == "" || m["GOCODE_PROVIDER"] == "" {
		t.Errorf("non-secret names missing: %v", m)
	}
}

// No warning when auth.json holds a key the background session can use, nor
// when the caller has no key in its environment at all.
func TestBgEnvOnlyKeyWarning(t *testing.T) {
	env, _, _, _ := bgKeyEnv(t, map[string]string{"OPENROUTER_API_KEY": fakeBgKey})
	if w := bgEnvOnlyKeyWarning(env, nil); w == "" {
		t.Fatalf("env-only key: want a warning")
	} else if strings.Contains(w, fakeBgKey) {
		t.Fatalf("warning leaks the key")
	}
	saveAuthKey(t, env, "sk-or-v1-FAKE-auth-json-key")
	if w := bgEnvOnlyKeyWarning(env, nil); w != "" {
		t.Errorf("auth.json has a key: want no warning, got %q", w)
	}
	env2, _, _, _ := bgKeyEnv(t, nil)
	if w := bgEnvOnlyKeyWarning(env2, nil); w != "" {
		t.Errorf("no key anywhere: the job fails with its own message; got %q", w)
	}
	// --provider in the forwarded flags selects the provider checked.
	env3, _, _, _ := bgKeyEnv(t, map[string]string{"OPENROUTER_API_KEY": fakeBgKey})
	if w := bgEnvOnlyKeyWarning(env3, []string{"--provider", "anthropic"}); w != "" {
		t.Errorf("anthropic session with only an OpenRouter key: no env-only warning expected, got %q", w)
	}
}

// A background job finds its key in auth.json (written by /login) — in the
// user config dir, not the daemon's — and starts.
func TestBgWorkerUsesAuthJSONKey(t *testing.T) {
	env, _, _, _ := bgKeyEnv(t, map[string]string{"GOCODE_CODE_SESSION_KIND": "bg"})
	saveAuthKey(t, env, "sk-or-v1-FAKE-auth-json-key")
	s := newPrintSession(env, nil)
	if s.APIKey == "" || s.KeySource != string(auth.SourceAuthFile) {
		t.Fatalf("job did not resolve the auth.json key (source %q)", s.KeySource)
	}
	if msg := bgWorkerMissingKey(env, s); msg != "" {
		t.Errorf("job with an auth.json key must start, got %q", msg)
	}
}

// A background job with no key anywhere fails fast: the clear message on
// stderr, exit 1, and its job record settled "failed" with that detail.
func TestBgWorkerNoKeyFailsFast(t *testing.T) {
	jobDir := t.TempDir()
	env, _, _, errb := bgKeyEnv(t, map[string]string{"GOCODE_CODE_SESSION_KIND": "bg", "GOCODE_JOB_DIR": jobDir})
	st := daemon.NewJobState(daemon.SeedParams{Template: "bg", SessionID: "s", Cwd: "/", Intent: "x"})
	if err := daemon.WriteJobState(jobDir, st); err != nil {
		t.Fatal(err)
	}
	s := newInteractiveSession(env, nil)
	s.newProgram = nil
	if code := runInteractiveSession(env, s, "hello"); code != 1 {
		t.Fatalf("runInteractiveSession = %d, want 1", code)
	}
	want := "No API key for openrouter in the background session — run /login (saves to auth.json) or set it in the environment that starts the daemon"
	if !strings.Contains(errb.String(), want) {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
	got, _ := daemon.ReadJobState(jobDir)
	if got == nil || got.State != "failed" || !strings.Contains(got.Detail, "No API key for openrouter") || !got.IsSettled() {
		t.Errorf("job state = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(jobDir, "timeline.jsonl")); !strings.Contains(string(b), `"failed"`) {
		t.Errorf("timeline missing the failure: %q", b)
	}
}

// Outside a background session, a missing key still starts the interactive
// session on the placeholder caller (unchanged).
func TestBgWorkerMissingKeyOnlyForBg(t *testing.T) {
	env, _, _, _ := bgKeyEnv(t, nil)
	if msg := bgWorkerMissingKey(env, newPrintSession(env, nil)); msg != "" {
		t.Errorf("non-bg session: want no fail-fast, got %q", msg)
	}
}
