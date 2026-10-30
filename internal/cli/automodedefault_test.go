package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// autoFixture is an isolated HOME + project with helpers for the settings
// files and the global state file.
type autoFixture struct {
	*subcmdFixture
	t *testing.T
}

func newAutoFixture(t *testing.T) *autoFixture {
	t.Helper()
	return &autoFixture{subcmdFixture: newSubcmdFixture(t), t: t}
}

func (f *autoFixture) userSettingsPath() string {
	return filepath.Join(f.home, ".config", "gocode", "settings.json")
}

func (f *autoFixture) localSettingsPath() string {
	return filepath.Join(f.cwd, ".gocode", "settings.local.json")
}

func (f *autoFixture) projectSettingsPath() string {
	return filepath.Join(f.cwd, ".gocode", "settings.json")
}

func (f *autoFixture) write(path, body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *autoFixture) readJSON(path string) map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		f.t.Fatalf("%s: %v", path, err)
	}
	return obj
}

func (f *autoFixture) statePath(env *Env) string {
	f.t.Helper()
	p := gocodeGlobalConfigPath(env)
	if p == "" || !strings.HasPrefix(p, f.home) {
		f.t.Fatalf("state path %q is not inside the test home %q", p, f.home)
	}
	return p
}

func userDefaultMode(obj map[string]any) string {
	perms, _ := obj["permissions"].(map[string]any)
	v, _ := perms["defaultMode"].(string)
	return v
}

func TestStartupModeInteractiveFallsBackToAuto(t *testing.T) {
	f := newAutoFixture(t)
	env, _, _ := f.env()

	s := newInteractiveSession(env, nil)
	if s.print.Mode != toolreg.ModeAuto || !s.print.modeFromFallback {
		t.Fatalf("interactive with nothing configured: mode=%q fromFallback=%v, want auto from fallback", s.print.Mode, s.print.modeFromFallback)
	}
	// Print mode keeps the default mode.
	if p := newPrintSession(env, nil); p.Mode != toolreg.ModeDefault || p.modeFromFallback {
		t.Fatalf("print mode with nothing configured: mode=%q", p.Mode)
	}
}

func TestStartupModeChosenModesWin(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		flag     string
		wantMode toolreg.Mode
	}{
		{name: "user default", user: `{"permissions":{"defaultMode":"default"}}`, wantMode: toolreg.ModeDefault},
		{name: "user manual alias", user: `{"permissions":{"defaultMode":"manual"}}`, wantMode: toolreg.ModeDefault},
		{name: "user plan", user: `{"permissions":{"defaultMode":"plan"}}`, wantMode: toolreg.ModePlan},
		{name: "user auto", user: `{"permissions":{"defaultMode":"auto"}}`, wantMode: toolreg.ModeAuto},
		{name: "flag auto", flag: "auto", wantMode: toolreg.ModeAuto},
		{name: "flag manual", flag: "manual", wantMode: toolreg.ModeDefault},
		{name: "flag default beats user auto", user: `{"permissions":{"defaultMode":"auto"}}`, flag: "default", wantMode: toolreg.ModeDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAutoFixture(t)
			if c.user != "" {
				f.write(f.userSettingsPath(), c.user)
			}
			env, _, _ := f.env()
			var d *DispatchDefaults
			if c.flag != "" {
				d = &DispatchDefaults{PermissionMode: c.flag}
			}
			s := newInteractiveSession(env, d)
			if s.print.Mode != c.wantMode || s.print.modeFromFallback {
				t.Fatalf("mode=%q fromFallback=%v, want %q chosen", s.print.Mode, s.print.modeFromFallback, c.wantMode)
			}
		})
	}
}

func TestStartupModeRepoAutoIgnoredWithWarning(t *testing.T) {
	f := newAutoFixture(t)
	f.write(f.localSettingsPath(), `{"permissions":{"defaultMode":"auto"}}`)
	env, _, _ := f.env()

	p := newPrintSession(env, nil)
	if p.Mode != toolreg.ModeDefault || !strings.Contains(p.modeNotice, msgSettingsAutoUntrusted) {
		t.Fatalf("print: mode=%q notice=%q", p.Mode, p.modeNotice)
	}
	// Ignored means unset: an interactive session then starts in auto mode by
	// the fallback, not by the repository's grant.
	s := newInteractiveSession(env, nil)
	if s.print.Mode != toolreg.ModeAuto || !s.print.modeFromFallback {
		t.Fatalf("interactive: mode=%q fromFallback=%v", s.print.Mode, s.print.modeFromFallback)
	}

	// A repository bypass grant starts in default mode (a chosen mode: no
	// fallback) with its warning.
	f.write(f.localSettingsPath(), `{}`)
	f.write(f.projectSettingsPath(), `{"permissions":{"defaultMode":"bypassPermissions"}}`)
	s = newInteractiveSession(env, nil)
	if s.print.Mode != toolreg.ModeDefault || s.print.modeFromFallback || !strings.Contains(s.print.modeNotice, msgSettingsBypassUntrusted) {
		t.Fatalf("repo bypass: mode=%q fromFallback=%v notice=%q", s.print.Mode, s.print.modeFromFallback, s.print.modeNotice)
	}
}

func TestStartupModeAutoDisabledBySettings(t *testing.T) {
	for _, body := range []string{`{"disableAutoMode":"disable"}`, `{"permissions":{"disableAutoMode":"disable"}}`} {
		f := newAutoFixture(t)
		f.write(f.userSettingsPath(), body)
		env, _, _ := f.env()

		s := newInteractiveSession(env, nil)
		if s.print.Mode != toolreg.ModeDefault || s.print.modeFromFallback {
			t.Fatalf("%s: no fallback expected, mode=%q", body, s.print.Mode)
		}
		s = newInteractiveSession(env, &DispatchDefaults{PermissionMode: "auto"})
		if s.print.Mode != toolreg.ModeDefault || !strings.Contains(s.print.modeNotice, "auto mode disabled by settings") {
			t.Fatalf("%s: --permission-mode auto: mode=%q notice=%q", body, s.print.Mode, s.print.modeNotice)
		}
	}
}

func TestAutoDefaultNudgeConditions(t *testing.T) {
	cases := []struct {
		name  string
		user  string
		local string
		seen  bool
		want  string
	}{
		{name: "user default", user: `{"permissions":{"defaultMode":"default"}}`, want: "default"},
		{name: "user manual", user: `{"permissions":{"defaultMode":"manual"}}`, want: "manual"},
		{name: "user acceptEdits", user: `{"permissions":{"defaultMode":"acceptEdits"}}`, want: "acceptEdits"},
		{name: "user unset", user: `{}`, want: ""},
		{name: "user auto", user: `{"permissions":{"defaultMode":"auto"}}`, want: ""},
		{name: "local shadows", user: `{"permissions":{"defaultMode":"default"}}`, local: `{"permissions":{"defaultMode":"plan"}}`, want: ""},
		{name: "already answered", user: `{"permissions":{"defaultMode":"default"}}`, seen: true, want: ""},
		{name: "auto disabled", user: `{"permissions":{"defaultMode":"default","disableAutoMode":"disable"}}`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAutoFixture(t)
			f.write(f.userSettingsPath(), c.user)
			if c.local != "" {
				f.write(f.localSettingsPath(), c.local)
			}
			env, _, _ := f.env()
			if c.seen {
				f.write(f.statePath(env), `{"hasSeenAutoDefaultNudge":true}`)
			}
			if got := autoDefaultNudgeMode(env); got != c.want {
				t.Fatalf("autoDefaultNudgeMode = %q, want %q", got, c.want)
			}
		})
	}
}

// startAutoSession runs a fake-backed interactive session built the way the
// CLI builds one (newInteractiveSession).
func startAutoSession(t *testing.T, env *Env, d *DispatchDefaults) (*fakeProgram, chan int) {
	t.Helper()
	progCh := make(chan *fakeProgram, 1)
	s := newInteractiveSession(env, d)
	s.print.newCaller = func(*printSession) (agent.ModelCaller, error) { return fakeCaller{reply: "ok"}, nil }
	s.print.Model = "test-model"
	s.newProgram = func(m *ui.Model, _ *Env) teaProgram {
		p := newFakeProgram(m)
		progCh <- p
		return p
	}
	done := make(chan int, 1)
	go func() { done <- runInteractiveSession(env, s, "") }()
	prog := mustProgram(t, progCh)
	t.Cleanup(func() {
		prog.Quit()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("session did not end")
		}
	})
	return prog, done
}

func isNudge(m tea.Msg) bool {
	o, ok := m.(ui.OpenDialogMsg)
	return ok && o.Dialog != nil && o.Dialog.Title == autoDefaultNudgeTitle
}

func TestAutoDefaultNudgeAccept(t *testing.T) {
	f := newAutoFixture(t)
	f.write(f.userSettingsPath(), `{"permissions":{"defaultMode":"acceptEdits"}}`)
	env, _, _ := f.env()

	prog, _ := startAutoSession(t, env, nil)
	if !prog.waitFor(isNudge, 3*time.Second) {
		t.Fatal("the default question was not asked")
	}
	var dlg *ui.Dialog
	prog.mu.Lock()
	for _, m := range prog.sent {
		if isNudge(m) {
			dlg = m.(ui.OpenDialogMsg).Dialog
		}
	}
	prog.mu.Unlock()
	if len(dlg.Options) != 2 || dlg.Options[0].Label != autoDefaultNudgeAccept || dlg.Options[1].Label != "No, keep accept edits" {
		t.Fatalf("options = %+v", dlg.Options)
	}
	if len(dlg.Body) != 1 || dlg.Body[0] != autoModeSummary {
		t.Fatalf("body = %q", dlg.Body)
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetPermissionMode)
		return ok && s.Mode == ui.ModeAuto
	}, 3*time.Second) {
		t.Fatal("the session did not switch to auto mode")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := f.readJSON(f.statePath(env))
		if userDefaultMode(f.readJSON(f.userSettingsPath())) == "auto" && state["hasSeenAutoDefaultNudge"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("user settings %v / state %v not updated", f.readJSON(f.userSettingsPath()), state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The entry description shows (the mode was chosen, not a fallback).
	if !prog.waitFor(func(m tea.Msg) bool { s, ok := m.(ui.AppendSystemMsg); return ok && s.Text == autoModeDescription }, 3*time.Second) {
		t.Fatal("auto mode description not shown on entering auto mode")
	}

	// The next session starts in auto mode and is not asked again.
	next := newInteractiveSession(env, nil)
	if next.print.Mode != toolreg.ModeAuto || next.print.modeFromFallback {
		t.Fatalf("next session mode=%q fromFallback=%v", next.print.Mode, next.print.modeFromFallback)
	}
	if got := autoDefaultNudgeMode(env); got != "" {
		t.Fatalf("asked again: %q", got)
	}
}

func TestAutoDefaultNudgeDecline(t *testing.T) {
	for _, how := range []string{"choose no", "esc"} {
		t.Run(how, func(t *testing.T) {
			f := newAutoFixture(t)
			f.write(f.userSettingsPath(), `{"permissions":{"defaultMode":"default"}}`)
			env, _, _ := f.env()

			prog, _ := startAutoSession(t, env, nil)
			if !prog.waitFor(isNudge, 3*time.Second) {
				t.Fatal("the default question was not asked")
			}
			if how == "esc" {
				prog.press(tea.KeyPressMsg{Code: tea.KeyEscape})
			} else {
				prog.press(tea.KeyPressMsg{Code: tea.KeyDown})
				prog.press(tea.KeyPressMsg{Code: tea.KeyEnter})
			}
			deadline := time.Now().Add(3 * time.Second)
			for f.readJSON(f.statePath(env))["hasSeenAutoDefaultNudge"] != true {
				if time.Now().After(deadline) {
					t.Fatal("decline was not remembered")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if m := userDefaultMode(f.readJSON(f.userSettingsPath())); m != "default" {
				t.Fatalf("user defaultMode = %q after decline", m)
			}
			if got := autoDefaultNudgeMode(env); got != "" {
				t.Fatalf("asked again after decline: %q", got)
			}
			if s := newInteractiveSession(env, nil); s.print.Mode != toolreg.ModeDefault {
				t.Fatalf("next session mode = %q", s.print.Mode)
			}
		})
	}
}

func TestAutoDefaultNudgeNotAskedWhenNoDefault(t *testing.T) {
	f := newAutoFixture(t)
	env, _, _ := f.env()
	prog, _ := startAutoSession(t, env, nil)
	// Startup done once the fallback notice is shown.
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.AppendSystemMsg)
		return ok && strings.HasPrefix(s.Text, autoDefaultNoticeTitle)
	}, 3*time.Second) {
		t.Fatal("the auto default notice was not shown")
	}
	time.Sleep(100 * time.Millisecond)
	if prog.waitFor(isNudge, 10*time.Millisecond) {
		t.Fatal("asked although no default mode is configured")
	}
	if prog.waitFor(func(m tea.Msg) bool { s, ok := m.(ui.AppendSystemMsg); return ok && s.Text == autoModeDescription }, 10*time.Millisecond) {
		t.Fatal("entry description shown in a fallback session")
	}
	if f.readJSON(f.statePath(env))["hasSeenAutoDefaultNotice"] != true {
		t.Fatal("notice not remembered")
	}
}

// collectSend records the messages a notices helper posts.
type collectSend struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (c *collectSend) send(m tea.Msg) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *collectSend) texts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		switch t := m.(type) {
		case ui.AppendSystemMsg:
			out = append(out, t.Text)
		case ui.ToastMsg:
			out = append(out, "toast:"+t.Text)
		}
	}
	return out
}

func TestAutoModeEntryWarning(t *testing.T) {
	f := newAutoFixture(t)
	env, _, _ := f.env()

	c := &collectSend{}
	n := &autoModeNotices{env: env, send: c.send}
	n.startup(toolreg.ModeDefault)
	n.modeChanged(toolreg.ModePlan, toolreg.ModeAuto)
	n.modeChanged(toolreg.ModeAuto, toolreg.ModeDefault)
	n.modeChanged(toolreg.ModePlan, toolreg.ModeAuto)
	if got := c.texts(); len(got) != 1 || got[0] != autoModeDescription {
		t.Fatalf("first session notices = %q", got)
	}
	// Shown once ever.
	c2 := &collectSend{}
	(&autoModeNotices{env: env, send: c2.send}).startup(toolreg.ModeAuto)
	if got := c2.texts(); len(got) != 0 {
		t.Fatalf("shown again: %q", got)
	}

	// skipAutoPermissionPrompt in the user settings suppresses it.
	f2 := newAutoFixture(t)
	f2.write(f2.userSettingsPath(), `{"skipAutoPermissionPrompt":true}`)
	env2, _, _ := f2.env()
	c3 := &collectSend{}
	(&autoModeNotices{env: env2, send: c3.send}).startup(toolreg.ModeAuto)
	if got := c3.texts(); len(got) != 0 {
		t.Fatalf("shown despite skipAutoPermissionPrompt: %q", got)
	}
	// ...but not from a repository-controlled file.
	f3 := newAutoFixture(t)
	f3.write(f3.localSettingsPath(), `{"skipAutoPermissionPrompt":true}`)
	env3, _, _ := f3.env()
	c4 := &collectSend{}
	(&autoModeNotices{env: env3, send: c4.send}).startup(toolreg.ModeAuto)
	if got := c4.texts(); len(got) != 1 {
		t.Fatalf("local skipAutoPermissionPrompt honored: %q", got)
	}
}

func TestAutoDefaultNoticeShownOnce(t *testing.T) {
	f := newAutoFixture(t)
	env, _, _ := f.env()
	c := &collectSend{}
	n := &autoModeNotices{env: env, send: c.send, fromFallback: true}
	n.startup(toolreg.ModeAuto)
	n.modeChanged(toolreg.ModeAuto, toolreg.ModeDefault)
	n.modeChanged(toolreg.ModePlan, toolreg.ModeAuto)
	got := c.texts()
	if len(got) != 1 || got[0] != autoDefaultNoticeTitle+"\n"+autoModeSummary {
		t.Fatalf("fallback session notices = %q", got)
	}
	c2 := &collectSend{}
	(&autoModeNotices{env: env, send: c2.send, fromFallback: true}).startup(toolreg.ModeAuto)
	if got := c2.texts(); len(got) != 0 {
		t.Fatalf("notice shown again: %q", got)
	}
}

func TestAutoModeUnavailableNotice(t *testing.T) {
	f := newAutoFixture(t)
	f.write(f.userSettingsPath(), `{"disableAutoMode":"disable"}`)
	env, _, _ := f.env()
	c := &collectSend{}
	n := &autoModeNotices{env: env, send: c.send}
	n.modeChanged(toolreg.ModeAcceptEdits, toolreg.ModePlan)
	n.modeChanged(toolreg.ModePlan, toolreg.ModeDefault)
	n.modeChanged(toolreg.ModeDefault, toolreg.ModeAcceptEdits)
	n.modeChanged(toolreg.ModePlan, toolreg.ModeDefault)
	if got := c.texts(); len(got) != 1 || got[0] != "toast:auto mode disabled by settings" {
		t.Fatalf("notices = %q", got)
	}
	// Available auto mode: nothing to say.
	f2 := newAutoFixture(t)
	env2, _, _ := f2.env()
	c2 := &collectSend{}
	(&autoModeNotices{env: env2, send: c2.send}).modeChanged(toolreg.ModePlan, toolreg.ModeDefault)
	if got := c2.texts(); len(got) != 0 {
		t.Fatalf("notices = %q", got)
	}
}

func lastModeCycle(p *fakeProgram) (ui.ModeCycle, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.sent) - 1; i >= 0; i-- {
		if c, ok := p.sent[i].(ui.SetModeCycle); ok {
			return c.Cycle, true
		}
	}
	return ui.ModeCycle{}, false
}

func TestShiftTabCycleConditions(t *testing.T) {
	cases := []struct {
		name string
		user string
		d    *DispatchDefaults
		want ui.ModeCycle
	}{
		{name: "plain", d: &DispatchDefaults{PermissionMode: "default"}, want: ui.ModeCycle{}},
		{name: "auto disabled", user: `{"disableAutoMode":"disable"}`, want: ui.ModeCycle{NoAuto: true}},
		{name: "bypass allowed", d: &DispatchDefaults{PermissionMode: "default", AllowBypass: true}, want: ui.ModeCycle{Bypass: true}},
		{name: "bypass allowed but disabled", user: `{"permissions":{"disableBypassPermissionsMode":"disable"}}`, d: &DispatchDefaults{AllowBypass: true}, want: ui.ModeCycle{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAutoFixture(t)
			if c.user != "" {
				f.write(f.userSettingsPath(), c.user)
			}
			env, _, _ := f.env()
			prog, _ := startAutoSession(t, env, c.d)
			if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetModeCycle); return ok }, 3*time.Second) {
				t.Fatal("mode cycle not sent")
			}
			if got, _ := lastModeCycle(prog); got != c.want {
				t.Fatalf("cycle = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Shift+tab changes the mode for this session only: nothing is written to the
// settings files.
func TestShiftTabDoesNotPersist(t *testing.T) {
	f := newAutoFixture(t)
	env, _, _ := f.env()
	prog, _ := startAutoSession(t, env, &DispatchDefaults{PermissionMode: "default"})
	if !prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetModeCycle); return ok }, 3*time.Second) {
		t.Fatal("startup not finished")
	}
	prog.press(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if !prog.waitFor(func(m tea.Msg) bool {
		s, ok := m.(ui.SetPermissionMode)
		return ok && s.Mode == ui.ModeAcceptEdits
	}, 3*time.Second) {
		t.Fatal("shift+tab did not switch the session mode")
	}
	for _, p := range []string{f.localSettingsPath(), f.userSettingsPath(), f.projectSettingsPath()} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s written by shift+tab", p)
		}
	}
}

func TestPermissionModeFlagManualAlias(t *testing.T) {
	if m := modeFromDefaults(&DispatchDefaults{PermissionMode: "manual"}); m != toolreg.ModeDefault {
		t.Fatalf("manual = %q", m)
	}
	if m := modeFromDefaults(&DispatchDefaults{PermissionMode: "auto"}); m != toolreg.ModeAuto {
		t.Fatalf("auto = %q", m)
	}
}
