package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTripAcrossChunkBoundaries(t *testing.T) {
	code := 3
	stream := append(EncodeDataFrame([]byte("hello")), EncodeCtrlFrame(Ctrl{T: CtrlExit, Code: &code, Signal: "SIGTERM"})...)
	stream = append(stream, EncodeDataFrame([]byte("world"))...)
	var data []string
	var ctrls []Ctrl
	d := &FrameDecoder{OnData: func(p []byte) { data = append(data, string(p)) }, OnCtrl: func(c Ctrl) { ctrls = append(ctrls, c) }}
	for _, b := range stream { // one byte at a time
		d.Feed([]byte{b})
	}
	if strings.Join(data, "|") != "hello|world" {
		t.Fatalf("data = %q", data)
	}
	if len(ctrls) != 1 || ctrls[0].T != CtrlExit || *ctrls[0].Code != 3 || ctrls[0].Signal != "SIGTERM" {
		t.Fatalf("ctrls = %+v", ctrls)
	}
}

func TestFrameDecoderRejectsOversizeAndUnknownKind(t *testing.T) {
	var errs []string
	d := &FrameDecoder{OnError: func(s string) { errs = append(errs, s) }}
	hdr := []byte{0x00, 0x20, 0x00, 0x00, 0} // 2 MiB
	d.Feed(hdr)
	if len(errs) != 1 || !strings.Contains(errs[0], "frame too large") {
		t.Fatalf("errs = %v", errs)
	}
	d.Feed(EncodeDataFrame([]byte("x"))) // poisoned: ignored
	if len(errs) != 1 {
		t.Fatalf("decoder not poisoned: %v", errs)
	}
	errs = nil
	d2 := &FrameDecoder{OnError: func(s string) { errs = append(errs, s) }}
	d2.Feed([]byte{0, 0, 0, 1, 7, 'x'})
	if len(errs) != 1 || errs[0] != "unknown frame kind 7" {
		t.Fatalf("errs = %v", errs)
	}
}

func TestByteRingTrimsWholeChunksAndContinuationBytes(t *testing.T) {
	r := NewByteRing(10)
	r.Push([]byte("aaaaaa"))
	r.Push([]byte("\x80\x80bbbb")) // continuation bytes at the new head after a drop
	r.Push([]byte("cc"))
	got := string(r.Bytes())
	if got != "bbbbcc" {
		t.Fatalf("ring = %q", got)
	}
	// The newest chunk is always kept, even when it alone exceeds the cap.
	r.Push([]byte("0123456789ABCDEF"))
	if string(r.Bytes()) != "0123456789ABCDEF" {
		t.Fatalf("ring = %q", r.Bytes())
	}
}

func TestJobStateRoundTripKeepsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	raw := `{"state":"working","detail":"x","template":"bg","intent":"do it","sessionId":"s","cwd":"/w",
	"createdAt":"2026-01-01T00:00:00.000Z","updatedAt":"2026-01-01T00:00:00.000Z","futureField":{"a":1},"pinned":true}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := ReadJobState(dir)
	if err != nil || st == nil {
		t.Fatalf("read: %v", err)
	}
	if st.Tempo != "idle" || st.Backend != "daemon" || st.RespawnFlags == nil {
		t.Fatalf("defaults not applied: %+v", st)
	}
	st.State = "done"
	if err := WriteJobState(dir, st); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["futureField"]; !ok {
		t.Fatalf("unknown field dropped: %s", b)
	}
	if _, ok := m["pinned"]; ok {
		t.Fatalf("pinned must not be written: %s", b)
	}
	if m["lastTerminalAt"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("lastTerminalAt not stamped for a settled state: %s", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.json")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
}

func TestReadJobStateSchemaAndAbsence(t *testing.T) {
	dir := t.TempDir()
	if st, err := ReadJobState(dir); st != nil || err != nil {
		t.Fatalf("absent: %v %v", st, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"state":"working"}`), 0o600)
	if _, err := ReadJobState(dir); err == nil || !strings.Contains(err.Error(), "schema validation failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestMarkStale(t *testing.T) {
	old := isoTime(time.Now().Add(-time.Minute))
	fresh := isoTime(time.Now())
	mk := func(state, created, detail string) *JobState {
		return &JobState{State: state, Tempo: "active", Template: "bg", RespawnFlags: []string{}, CreatedAt: created, Detail: detail}
	}
	in := []JobEntry{
		{ID: "aaaaaaaa", State: mk("working", old, "x; respawning")},
		{ID: "bbbbbbbb", State: mk("working", old, "")},
		{ID: "cccccccc", State: mk("working", fresh, "")},
	}
	out := MarkStale(in, map[string]bool{"bbbbbbbb": true}, time.Now())
	if out[0].State.State != "failed" || out[0].State.Detail != "x" || out[0].State.Tempo != "idle" {
		t.Fatalf("stale not failed: %+v", out[0].State)
	}
	if out[1].State.State != "working" || out[2].State.State != "working" {
		t.Fatalf("live/fresh changed: %+v %+v", out[1].State, out[2].State)
	}
	if in[0].State.State != "working" {
		t.Fatal("MarkStale mutated its input")
	}
}

func TestWorkerArgs(t *testing.T) {
	d := &Dispatch{SessionID: "S", Launch: Launch{Mode: LaunchPrompt, Args: []string{"--session-id", "S", "hi"}}}
	if got := WorkerArgs(d, 1, false, "S", nil); strings.Join(got, " ") != "--session-id S hi" {
		t.Fatalf("prompt: %v", got)
	}
	if got := WorkerArgs(d, 2, true, "S", []string{"--model", "m"}); strings.Join(got, " ") != "--resume S --model m" {
		t.Fatalf("respawn with transcript: %v", got)
	}
	if got := WorkerArgs(d, 2, false, "T", nil); strings.Join(got, " ") != "--session-id T" {
		t.Fatalf("respawn new id: %v", got)
	}
	r := &Dispatch{SessionID: "N", Launch: Launch{Mode: LaunchResume, SessionID: "OLD", Fork: true, FlagArgs: []string{"-x"}}}
	if got := WorkerArgs(r, 1, false, "N", nil); strings.Join(got, " ") != "--session-id N --fork-session --resume OLD -x" {
		t.Fatalf("resume fork: %v", got)
	}
	e := &Dispatch{Launch: Launch{Mode: LaunchExec, Cmd: "/bin/sh", Args: []string{"-c", "ls"}}}
	if got := WorkerArgs(e, 3, true, "S", nil); strings.Join(got, " ") != "-c ls" {
		t.Fatalf("exec: %v", got)
	}
}

func TestWorkerEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/h", "GOCODE_SECRET_THING=1", "OTEL_X=1"}
	d := &Dispatch{Short: "abcd1234", Source: SourceShell, Seed: &Seed{Intent: "line1\nline2"},
		Env: map[string]string{"PATH": "/custom", "OPENROUTER_API_KEY": "k"}, Launch: Launch{Mode: LaunchPrompt}}
	env := strings.Join(WorkerEnv(base, d, WorkerEnvParams{JobDir: "/j", ConfigDir: "/c", PTYAuth: "tok"}), "\n")
	for _, want := range []string{"GOCODE_CODE_SESSION_KIND=bg", "GOCODE_BG_BACKEND=daemon", "GOCODE_BG_SOURCE=shell",
		"GOCODE_JOB_DIR=/j", "GOCODE_CONFIG_DIR=/c", "GOCODE_CODE_SESSION_NAME=line1 line2", "FORCE_COLOR=3",
		"COLORTERM=truecolor", "BROWSER=true", "PATH=/custom", "GOCODE_BG_PTY_AUTH=tok",
		"GOCODE_SECRET_THING=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in\n%s", want, env)
		}
	}
	// A key carried in the dispatch (an older client) is dropped.
	if strings.Contains(env, "OPENROUTER_API_KEY") {
		t.Errorf("dispatch key reached the worker env:\n%s", env)
	}
	d.Launch = Launch{Mode: LaunchExec, Cmd: "/bin/sh"}
	env = strings.Join(WorkerEnv(base, d, WorkerEnvParams{JobDir: "/j", ConfigDir: "/c", TokensPath: "/t"}), "\n")
	for _, gone := range []string{"GOCODE_SECRET_THING", "GOCODE_CODE_SESSION_KIND", "OTEL_X", "BROWSER", "GOCODE_BG_PTY_AUTH="} {
		if strings.Contains(env, gone) {
			t.Errorf("exec env kept %q:\n%s", gone, env)
		}
	}
	for _, want := range []string{"GOCODE_PTY_HOST_EXEC=1", "GOCODE_JOB_DIR=/j", "GOCODE_CONFIG_DIR=/c", "GOCODE_BG_SOCKET_TOKENS_PATH=/t"} {
		if !strings.Contains(env, want) {
			t.Errorf("exec env missing %q", want)
		}
	}
}

func TestDispatchValidate(t *testing.T) {
	ok := Dispatch{Short: "abcd1234", SessionID: "s", Cwd: string(filepath.Separator) + "w", Launch: Launch{Mode: LaunchPrompt}}
	if runtime.GOOS == "windows" {
		ok.Cwd = `C:\w`
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Dispatch){
		func(d *Dispatch) { d.Short = "ABCD1234" },
		func(d *Dispatch) { d.Cwd = "rel" },
		func(d *Dispatch) { d.Launch.Mode = "nope" },
		func(d *Dispatch) { d.Launch = Launch{Mode: LaunchExec} },
	} {
		d := ok
		bad(&d)
		if d.Validate() == nil {
			t.Errorf("accepted %+v", d)
		}
	}
}

func TestControlKey(t *testing.T) {
	dir := t.TempDir()
	if ReadControlKey(dir) != "" {
		t.Fatal("key before creation")
	}
	k1, err := EnsureControlKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatalf("key %q err %v", k1, err)
	}
	k2, _ := EnsureControlKey(dir)
	if k1 != k2 || ReadControlKey(dir) != k1 {
		t.Fatal("key not stable")
	}
	if !KeysMatch(k1, k1) || KeysMatch("", "") || KeysMatch(k1, k1[:31]+"x") {
		t.Fatal("KeysMatch")
	}
}

func TestDaemonLogFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.log")
	l, err := OpenDaemonLog(p)
	if err != nil {
		t.Fatal(err)
	}
	var echoed bytes.Buffer
	l.Echo = func(s string) { echoed.WriteString(s) }
	l.Write("bg", "bg spawned abcd1234 (shell)")
	_ = l.Close()
	b, _ := os.ReadFile(p)
	line := string(b)
	if !strings.HasPrefix(line, "[") || !strings.Contains(line, "Z] [bg] bg spawned abcd1234 (shell)\n") || echoed.String() != line {
		t.Fatalf("line = %q echo = %q", line, echoed.String())
	}
}

func TestBreadcrumbPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		got := PTYErrPath(`C:\cfg`, `\\.\pipe\cc-daemon-0123456789abcdef-pty-abcd1234`)
		if got != filepath.Join(`C:\cfg`, "daemon", "pty-pids", "cc-daemon-0123456789abcdef-pty-abcd1234.err") {
			t.Fatal(got)
		}
		return
	}
	if PTYErrPath("/c", "/r/pty/x.sock") != "/r/pty/x.sock.err" || PTYLatePath("/c", "/s") != "/s.late" ||
		PTYExecExitPath("/c", "/s") != "/s.exec-exit" {
		t.Fatal("breadcrumbs")
	}
	ep, err := PTYEndpoint("/cfg", "abcd1234")
	if err != nil || ep.Network != "unix" || !strings.HasSuffix(ep.Address, "/pty/abcd1234.sock") {
		t.Fatalf("%+v %v", ep, err)
	}
}

// The daemon's own provider key still reaches the worker; a dispatched one
// never does.
func TestWorkerEnvKeyOnlyFromDaemon(t *testing.T) {
	base := []string{"PATH=/usr/bin", "OPENROUTER_API_KEY=daemon-key"}
	d := &Dispatch{Short: "abcd1234", Source: SourceShell, Launch: Launch{Mode: LaunchPrompt},
		Env: map[string]string{"OPENROUTER_API_KEY": "caller-key", "ANTHROPIC_API_KEY": "x", "GOCODE_PROVIDER": "openrouter"}}
	env := strings.Join(WorkerEnv(base, d, WorkerEnvParams{JobDir: "/j"}), "\n")
	if !strings.Contains(env, "OPENROUTER_API_KEY=daemon-key") || strings.Contains(env, "caller-key") || strings.Contains(env, "ANTHROPIC_API_KEY") {
		t.Errorf("worker env keys wrong:\n%s", env)
	}
	if !strings.Contains(env, "GOCODE_PROVIDER=openrouter") {
		t.Errorf("non-secret dispatch env dropped:\n%s", env)
	}
}

// GOCODE_CONFIG_DIR is exported to a worker only when it was set explicitly
// or the daemon serves a non-default dir, so a default worker keeps reading
// the user's settings and auth.json.
func TestExportConfigDir(t *testing.T) {
	def := DefaultConfigHome(nil)
	if exportConfigDir(def, false) {
		t.Errorf("default config dir must not be exported")
	}
	if !exportConfigDir(def, true) {
		t.Errorf("an inherited GOCODE_CONFIG_DIR must be kept")
	}
	if !exportConfigDir("/c", false) || exportConfigDir("", true) {
		t.Errorf("non-default dir must be exported; empty never")
	}
	env := strings.Join(WorkerEnv([]string{"PATH=/b"}, &Dispatch{Launch: Launch{Mode: LaunchPrompt}}, WorkerEnvParams{JobDir: "/j", ConfigDir: def}), "\n")
	if strings.Contains(env, "GOCODE_CONFIG_DIR") {
		t.Errorf("default config dir exported:\n%s", env)
	}
}
