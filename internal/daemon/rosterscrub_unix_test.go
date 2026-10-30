//go:build linux || darwin

package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/auth"
)

const fakeRosterKey = "sk-or-v1-fakeRosterKeyForTests0123456789abcdef"

// dispatchWithEnv dispatches like bgFixture.dispatch but with a caller env.
func (f *bgFixture) dispatchWithEnv(short string, launch Launch, env map[string]string) *JobHandle {
	f.t.Helper()
	cwd := f.t.TempDir()
	if err := os.MkdirAll(JobDir(f.cfg, short), 0o700); err != nil {
		f.t.Fatal(err)
	}
	st := NewJobState(SeedParams{Template: "exec", SessionID: short + "-0000-4000-8000-000000000000", Cwd: cwd, Intent: "t"})
	if err := WriteJobState(JobDir(f.cfg, short), st); err != nil {
		f.t.Fatal(err)
	}
	d := Dispatch{Proto: 1, Short: short, SessionID: st.SessionID, CreatedAt: time.Now().UnixMilli(),
		Source: SourceShell, Cwd: cwd, Launch: launch, RespawnFlags: []string{}, Nonce: "n" + short,
		Cols: 80, Rows: 24, Env: env}
	res, h, refusal := f.m.Dispatch(d)
	if res != "spawned" || h == nil {
		f.t.Fatalf("dispatch = %s %q", res, refusal)
	}
	return h
}

func rosterBytes(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(RosterPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A bg job dispatched with a provider key in its env: the key never reaches
// roster.json (non-secret variables still do), and an exec job's command does
// not see it.
func TestRosterNeverStoresProviderKeys(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatchWithEnv("eeee0001", Launch{Mode: LaunchExec, Cmd: "/bin/sh",
		Args: []string{"-c", `echo "key=${OPENROUTER_API_KEY:-unset}"; sleep 30`}},
		map[string]string{"OPENROUTER_API_KEY": fakeRosterKey, "GOCODE_API_KEY": fakeRosterKey,
			"MY_SERVICE_TOKEN": fakeRosterKey, "GOCODE_MODEL": "some/model"})
	waitFor(t, "output", 10*time.Second, func() bool { return strings.Contains(ringText(h), "key=") })
	if got := ringText(h); !strings.Contains(got, "key=unset") {
		t.Fatalf("exec job saw the provider key: %q", got)
	}
	waitFor(t, "roster entry", 10*time.Second, func() bool { _, ok := ReadRoster(f.cfg)["eeee0001"]; return ok })
	raw := rosterBytes(t, f.cfg)
	if strings.Contains(raw, fakeRosterKey) || strings.Contains(raw, `"OPENROUTER_API_KEY":`) || strings.Contains(raw, `"MY_SERVICE_TOKEN":`) {
		t.Fatalf("roster.json holds a secret:\n%s", raw)
	}
	w := ReadRoster(f.cfg)["eeee0001"]
	if w.Dispatch == nil || w.Dispatch.Env["GOCODE_MODEL"] != "some/model" {
		t.Fatalf("non-secret env lost: %+v", w.Dispatch)
	}
}

// A roster written by an older build that still holds a key is scrubbed on
// the next write after adoption, the adopted job no longer carries the key,
// and a respawn of it resolves its key from auth.json.
func TestAdoptScrubsLegacyRosterKeys(t *testing.T) {
	f := newBGFixture(t)
	h := f.dispatch("eeee0002", Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "echo up; sleep 30"}})
	waitFor(t, "output", 10*time.Second, func() bool { return strings.Contains(ringText(h), "up") })
	waitFor(t, "worker pid", 10*time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.replPID > 1
	})
	if n := f.m.Close(false); n != 0 {
		t.Fatalf("Close(false) reaped %d", n)
	}
	// Simulate a legacy roster: inject a provider key into the dispatch env.
	var r Roster
	if err := json.Unmarshal([]byte(rosterBytes(t, f.cfg)), &r); err != nil {
		t.Fatal(err)
	}
	w := r.Workers["eeee0002"]
	w.Dispatch.Env = map[string]string{"OPENROUTER_API_KEY": fakeRosterKey, "GOCODE_MODEL": "m"}
	r.Workers["eeee0002"] = w
	writeTestRoster(t, f.cfg, r.Workers)
	if !strings.Contains(rosterBytes(t, f.cfg), fakeRosterKey) {
		t.Fatal("setup: legacy key not written")
	}

	m2 := newManagerFor(t, f)
	if a, d := m2.Adopt(); a != 1 || d != 0 {
		t.Fatalf("Adopt = %d/%d", a, d)
	}
	h2 := m2.get("eeee0002")
	if h2 == nil {
		t.Fatal("not registered")
	}
	m2.writeRoster()
	if raw := rosterBytes(t, f.cfg); strings.Contains(raw, fakeRosterKey) {
		t.Fatalf("legacy key survived the rewrite:\n%s", raw)
	}
	h2.mu.Lock()
	d := h2.dispatch
	h2.mu.Unlock()
	if _, ok := d.Env["OPENROUTER_API_KEY"]; ok || d.Env["GOCODE_MODEL"] != "m" {
		t.Fatalf("adopted dispatch env = %v", d.Env)
	}

	// Respawn: the worker env (daemon env without a key + the dispatch)
	// carries no key; gocode's resolver then finds it in auth.json.
	var base []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENROUTER_API_KEY=") {
			base = append(base, kv)
		}
	}
	d.Launch.Mode = LaunchPrompt
	env := WorkerEnv(base, &d, WorkerEnvParams{JobDir: JobDir(f.cfg, "eeee0002"), ConfigDir: f.cfg})
	lookup := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		lookup[k] = v
	}
	if _, ok := lookup["OPENROUTER_API_KEY"]; ok {
		t.Fatal("respawn env carries the key")
	}
	authPath := auth.DefaultAuthFilePath(lookup["GOCODE_CONFIG_DIR"])
	if err := auth.SaveProviderKey(authPath, auth.ProviderOpenRouter, fakeRosterKey); err != nil {
		t.Fatal(err)
	}
	res := auth.Resolver{Getenv: func(k string) string { return lookup[k] }, AuthFile: authPath}
	key, src, ok := res.ResolveProviderKey(auth.ProviderOpenRouter)
	if !ok || key != fakeRosterKey || src != string(auth.SourceAuthFile) {
		t.Fatalf("respawn key resolution = ok:%v src:%q", ok, src)
	}
	h2.kill("SIGTERM", OutcomeKilled)
	select {
	case <-h2.Settled():
	case <-time.After(15 * time.Second):
		t.Fatal("adopted job did not settle")
	}
}

func TestCappedDispatchDropsSecrets(t *testing.T) {
	d := Dispatch{Env: map[string]string{
		"OPENROUTER_API_KEY": "x", "ANTHROPIC_API_KEY": "x", "OPENAI_API_KEY": "x", "GOCODE_API_KEY": "x",
		"AWS_SECRET_ACCESS_KEY": "x", "PATH": "/bin", "GOCODE_MODEL": "m", "OPENROUTER_BASE_URL": "https://o.example",
		"SOMETHING": "ghp_" + strings.Repeat("a", 36),
	}}
	c := cappedDispatch(d)
	want := map[string]string{"GOCODE_MODEL": "m", "OPENROUTER_BASE_URL": "https://o.example"}
	if len(c.Env) != len(want) {
		t.Fatalf("env = %v", c.Env)
	}
	for k, v := range want {
		if c.Env[k] != v {
			t.Fatalf("env = %v", c.Env)
		}
	}
}
