package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

func TestBackgroundVerbIndex(t *testing.T) {
	cases := []struct {
		args   []string
		behind bool
		want   int
	}{
		{[]string{"logs", "abc"}, true, 0},
		{[]string{"attach"}, true, 0},
		{[]string{"--settings", "s.json", "stop", "abcd1234"}, true, 2},
		{[]string{"--settings", "s.json", "stop", "name"}, true, -1}, // behind options needs an id
		{[]string{"--model", "m", "stop", "abcd1234"}, true, -1},     // not an honoured root option
		{[]string{"--settings", "s.json", "stop", "abcd1234"}, false, -1},
		{[]string{"respawn", "--all"}, true, 0},
		{[]string{"--verbose", "respawn", "--all"}, true, 1},
		{[]string{"fix the logs"}, true, -1},
		{[]string{"--", "logs"}, true, -1},
		{[]string{"-p", "logs"}, true, -1},
	}
	for _, c := range cases {
		if got := backgroundVerbIndex(c.args, c.behind); got != c.want {
			t.Errorf("backgroundVerbIndex(%q, %v) = %d, want %d", c.args, c.behind, got, c.want)
		}
	}
}

func TestHasBackgroundFlagAndStrip(t *testing.T) {
	if !hasBackgroundFlag([]string{"--bg", "x"}) || !hasBackgroundFlag([]string{"x", "--background"}) {
		t.Fatal("flag not found")
	}
	if hasBackgroundFlag([]string{"--", "--bg"}) {
		t.Fatal("flag after -- counted")
	}
	got := stripBgFlags([]string{"--bg", "-n", "x", "--", "--bg", "text"})
	if strings.Join(got, " ") != "-n x -- --bg text" {
		t.Fatalf("strip = %q", got)
	}
}

func TestParseBgArgs(t *testing.T) {
	p := parseBgArgs([]string{"--name", "fixer", "--model", "m1", "--session-id", "zzz", "fix", "the", "tests"})
	if p.name != "fixer" || p.prompt != "fix the tests" || !p.sessionID || strings.Join(p.flags, " ") != "--model m1" {
		t.Fatalf("%+v", p)
	}
	p = parseBgArgs([]string{"-nquick", "--", "a", "--b"})
	if p.name != "quick" || p.prompt != "a --b" {
		t.Fatalf("%+v", p)
	}
	p = parseBgArgs([]string{"--exec", "make", "test"})
	if !p.hasExec || p.exec != "make test" {
		t.Fatalf("%+v", p)
	}
	p = parseBgArgs([]string{"--exec=ls -la", "-n", "lister"})
	if p.exec != "ls -la" || p.name != "lister" {
		t.Fatalf("%+v", p)
	}
}

func TestFormatBgHints(t *testing.T) {
	got := formatBgHints("abcd1234", daemon.DetailIdleNoMsg, "nm", false)
	want := "backgrounded · abcd1234 · nm (idle — send a prompt to start)\n" +
		"  gocode agents             list sessions\n" +
		"  gocode attach abcd1234    open in this terminal\n" +
		"  gocode logs abcd1234      show recent output\n" +
		"  gocode stop abcd1234      stop this session"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(formatBgHints("abcd1234", "", "", true), "\x1b[36mabcd1234\x1b[39m") {
		t.Fatal("no colour on a TTY")
	}
}

func TestReplayFilter(t *testing.T) {
	in := "a\x1b[31mred\x1b[0m\x1b[?25l\x1b]0;title\x07b\x1b[5;3H\x07c\x1b7d\x1b[3J\x1b[2J"
	got, addressed := replayFilter(in)
	if got != "a\x1b[31mred\x1b[0mb\x1b[5;3Hc\x1b7d\x1b[2J" || !addressed {
		t.Fatalf("got %q addressed %v", got, addressed)
	}
	if _, addr := replayFilter("x\x1b[?1049hy"); !addr {
		t.Fatal("alt screen not detected")
	}
	if got, addr := replayFilter("plain\r\ntext\x1b[K"); got != "plain\r\ntext\x1b[K" || addr {
		t.Fatalf("%q %v", got, addr)
	}
}

func TestDetachScanner(t *testing.T) {
	s := &detachScanner{}
	if fwd, det := s.scan([]byte("ab\x1acd")); string(fwd) != "ab" || !det {
		t.Fatalf("ctrl-z: %q %v", fwd, det)
	}
	s = &detachScanner{}
	if fwd, det := s.scan([]byte("x\x02")); string(fwd) != "x" || det {
		t.Fatalf("prefix: %q %v", fwd, det)
	}
	if fwd, det := s.scan([]byte("d")); len(fwd) != 0 || !det {
		t.Fatalf("prefix d: %q %v", fwd, det)
	}
	s = &detachScanner{}
	if fwd, det := s.scan([]byte("\x02q")); string(fwd) != "\x02q" || det {
		t.Fatalf("prefix other: %q %v", fwd, det)
	}
	s = &detachScanner{}
	if _, det := s.scan([]byte("\x1b[122;5u")); !det {
		t.Fatal("kitty ctrl+z")
	}
}

// bgTestEnv is an Env over a temp config dir with no daemon reachable.
func bgTestEnv(t *testing.T) (*Env, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	cfg := t.TempDir()
	var out, errb bytes.Buffer
	env := &Env{
		Getenv: func(k string) string {
			if k == "GOCODE_CONFIG_DIR" {
				return cfg
			}
			return ""
		},
		Setenv: func(string, string) {},
		Stdout: &out, Stderr: &errb,
		Cwd: func() (string, error) { return cfg, nil },
	}
	return env, &out, &errb, cfg
}

func writeJob(t *testing.T, cfg, id string, mut func(*daemon.JobState)) {
	t.Helper()
	st := daemon.NewJobState(daemon.SeedParams{Template: "bg", SessionID: id + "-0000-4000-8000-000000000000", Cwd: cfg, Intent: "fix the flaky test"})
	if mut != nil {
		mut(st)
	}
	dir := daemon.JobDir(cfg, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteJobState(dir, st); err != nil {
		t.Fatal(err)
	}
}

func TestResolveJobMessages(t *testing.T) {
	env, _, errb, cfg := bgTestEnv(t)
	writeJob(t, cfg, "abcd1234", func(s *daemon.JobState) { s.Name = "frontend" })
	writeJob(t, cfg, "abcd9999", nil)
	if r := resolveJob(env, cfg, "abcd1", nil, "u", "d", true); r.short != "abcd1234" {
		t.Fatalf("prefix: %+v", r)
	}
	r := resolveJob(env, cfg, "abcd", nil, "gocode logs <id|name>", "d", true)
	if r.code != 1 || !strings.Contains(errb.String(), "Ambiguous prefix 'abcd', matches: abcd1234, abcd9999") {
		t.Fatalf("ambiguous: %+v %q", r, errb.String())
	}
	errb.Reset()
	if r := resolveJob(env, cfg, "front", nil, "u", "d", true); r.short != "abcd1234" || !r.byName {
		t.Fatalf("name: %+v", r)
	}
	if !strings.Contains(errb.String(), "Session abcd1234 · frontend") {
		t.Fatalf("name lines: %q", errb.String())
	}
	errb.Reset()
	if r := resolveJob(env, cfg, "front", nil, "u", "d", false); r.code != 1 ||
		!strings.Contains(errb.String(), "No job matching 'front'. Run 'gocode agents' to list running sessions.") {
		t.Fatalf("names disabled: %+v %q", r, errb.String())
	}
	errb.Reset()
	if r := resolveJob(env, cfg, "-x", nil, "gocode stop <id>", "d", false); r.code != 1 ||
		errb.String() != "unknown option '-x'\nUsage: gocode stop <id>\n" {
		t.Fatalf("%q", errb.String())
	}
}

func TestStopWithoutDaemonSettledJob(t *testing.T) {
	env, out, errb, cfg := bgTestEnv(t)
	writeJob(t, cfg, "deadbeef", func(s *daemon.JobState) { s.State, s.Tempo = "failed", "idle" })
	code := runBackgroundVerb(env, []string{"stop", "deadbeef"}, 0)
	if code != 0 || out.String() != "stopped deadbeef\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestStopWithoutDaemonRunningJobIsUnconfirmed(t *testing.T) {
	env, _, errb, cfg := bgTestEnv(t)
	writeJob(t, cfg, "deadbee1", nil)
	code := runBackgroundVerb(env, []string{"kill", "deadbee1"}, 0)
	if code != 1 || !strings.Contains(errb.String(), "couldn't confirm deadbee1 was stopped") {
		t.Fatalf("code %d err %q", code, errb.String())
	}
}

func TestLogsWithoutDaemon(t *testing.T) {
	env, _, errb, cfg := bgTestEnv(t)
	writeJob(t, cfg, "cafe0001", nil)
	code := runBackgroundVerb(env, []string{"logs", "cafe"}, 0)
	if code != 1 || !strings.HasPrefix(errb.String(), "Couldn't read logs for cafe0001 — ") {
		t.Fatalf("code %d err %q", code, errb.String())
	}
}

func TestRmSettledJobWithoutDaemon(t *testing.T) {
	env, out, errb, cfg := bgTestEnv(t)
	writeJob(t, cfg, "cafe0002", func(s *daemon.JobState) { s.State, s.Tempo = "done", "idle" })
	if code := runBackgroundVerb(env, []string{"rm", "cafe0002"}, 0); code != 0 || out.String() != "removed cafe0002\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errb.String())
	}
	if _, err := os.Stat(daemon.JobDir(cfg, "cafe0002")); !os.IsNotExist(err) {
		t.Fatalf("job dir still there: %v", err)
	}
	errb.Reset()
	if code := runBackgroundVerb(env, []string{"rm", "x", "--force-remove-worktree", "NOPE"}, 0); code != 1 ||
		!strings.Contains(errb.String(), "option '--force-remove-worktree' takes the <worktree-id> value") {
		t.Fatalf("code %d err %q", code, errb.String())
	}
}

func TestHandleBgFlagAmbiguous(t *testing.T) {
	env, _, errb, _ := bgTestEnv(t)
	if code := handleBgFlag(env, []string{"--", "--bg"}); code != 1 ||
		!strings.Contains(errb.String(), "Couldn't tell whether --bg is a flag or text here, so nothing was started.") {
		t.Fatalf("code %d err %q", code, errb.String())
	}
	errb.Reset()
	if code := handleBgFlag(env, []string{"--bg", "--exec="}); code != 1 || errb.String() != "--exec requires a command.\n" {
		t.Fatalf("code %d err %q", code, errb.String())
	}
}

func TestBuildAgentsJSON(t *testing.T) {
	_, _, _, cfg := bgTestEnv(t)
	old := daemon.ISOTime(time.Now().Add(-time.Hour))
	writeJob(t, cfg, "aaaa0001", func(s *daemon.JobState) { s.Name = "live one"; s.CreatedAt = old })
	writeJob(t, cfg, "aaaa0002", func(s *daemon.JobState) { s.State, s.Tempo = "done", "idle"; s.CreatedAt = old })
	writeJob(t, cfg, "aaaa0003", func(s *daemon.JobState) { s.CreatedAt = old }) // stale -> failed
	live := map[string]daemon.JobRecord{"aaaa0001": {Short: "aaaa0001", PID: 4242, StartedAt: 99, Cwd: cfg}}
	got := buildAgentsJSON(cfg, "", false, live, time.Now())
	if len(got) != 1 || got[0].ID != "aaaa0001" || got[0].PID != 4242 || got[0].State != "working" || got[0].Kind != "background" || got[0].Name != "live one" {
		t.Fatalf("%+v", got)
	}
	all := buildAgentsJSON(cfg, "", true, live, time.Now())
	states := map[string]string{}
	for _, e := range all {
		states[e.ID] = e.State
	}
	if states["aaaa0002"] != "done" || states["aaaa0003"] != "failed" || len(all) != 3 {
		t.Fatalf("%+v", all)
	}
	if none := buildAgentsJSON(cfg, filepath.Join(cfg, "elsewhere"), true, live, time.Now()); len(none) != 0 {
		t.Fatalf("cwd filter: %+v", none)
	}
}
