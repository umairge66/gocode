package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// settingsCmdSession is a fake-backed interactive session for the settings
// commands: an isolated HOME (also the working directory, trusted), the
// session runtime captured for live-state checks, and a recording editor.
type settingsCmdSession struct {
	prog *fakeProgram
	env  *Env
	home string

	mu        sync.Mutex
	rt        *sessionRuntime
	edited    []string
	editorErr error
}

// runtime returns the captured session runtime.
func (s *settingsCmdSession) runtime() *sessionRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rt
}

// startSettingsCmdSession boots the session after setup has written any
// settings files into home.
func startSettingsCmdSession(t *testing.T, setup func(home string), tweak func(*interactiveSession)) *settingsCmdSession {
	t.Helper()
	home := t.TempDir()
	if setup != nil {
		setup(home)
	}
	env, _, _ := dialogEnv(home, map[string]string{"OPENROUTER_API_KEY": "sk-or-env"})
	env.trustSession = true
	sc := &settingsCmdSession{env: env, home: home}
	progCh := make(chan *fakeProgram, 1)
	sess := newDialogSession(env, providerOpenRouter, "my-model", progCh)
	sess.newConversation = func(caller agent.ModelCaller, env *Env, s *interactiveSession) *sessionRuntime {
		rt := defaultNewConversation(caller, env, s)
		sc.mu.Lock()
		sc.rt = rt
		sc.mu.Unlock()
		return rt
	}
	sess.openEditor = func(path string) (editorChoice, error) {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.edited = append(sc.edited, path)
		return editorChoice{argv: []string{"fake-ed"}, source: "$EDITOR", value: "fake-ed"}, sc.editorErr
	}
	sess.now = func() time.Time { return time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local) }
	if tweak != nil {
		tweak(sess)
	}
	go func() { runInteractiveSession(env, sess, "") }()
	sc.prog = mustProgram(t, progCh)
	t.Cleanup(sc.prog.Quit)
	sc.prog.waitFor(func(m tea.Msg) bool { _, ok := m.(ui.SetBannerMsg); return ok }, 2*time.Second)
	return sc
}

// mark returns the number of messages sent so far.
func (p *fakeProgram) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// waitAfter waits for a message after index from matching pred and returns
// it with the index just past it.
func (p *fakeProgram) waitAfter(from int, pred func(tea.Msg) bool) (tea.Msg, int, bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for i := from; i < len(p.sent); i++ {
			if pred(p.sent[i]) {
				m := p.sent[i]
				p.mu.Unlock()
				return m, i + 1, true
			}
		}
		p.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	return nil, from, false
}

// dialogAfter waits for a dialog titled title opened after index from.
func (s *settingsCmdSession) dialogAfter(t *testing.T, from int, title string) (*ui.Dialog, int) {
	t.Helper()
	m, next, ok := s.prog.waitAfter(from, func(m tea.Msg) bool {
		d, ok := m.(ui.OpenDialogMsg)
		return ok && d.Dialog != nil && d.Dialog.Title == title
	})
	if !ok {
		t.Fatalf("dialog %q was not opened", title)
	}
	return m.(ui.OpenDialogMsg).Dialog, next
}

// noticeAfter waits for a transcript notice, toast or error containing every
// substring after index from.
func (s *settingsCmdSession) noticeAfter(t *testing.T, from int, substrs ...string) (string, int) {
	t.Helper()
	m, next, ok := s.prog.waitAfter(from, func(m tea.Msg) bool {
		var text string
		switch v := m.(type) {
		case ui.AppendSystemMsg:
			text = v.Text
		case ui.AppendErrorMsg:
			text = v.Text
		case ui.ShowInfoPanelMsg:
			text = v.Text
		case ui.ToastMsg:
			text = v.Text
		default:
			return false
		}
		for _, sub := range substrs {
			if !strings.Contains(text, sub) {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("no notice containing %q", substrs)
	}
	return msgText(m) + panelText(m), next
}

// panelText returns an info panel's text.
func panelText(m tea.Msg) string {
	if p, ok := m.(ui.ShowInfoPanelMsg); ok {
		return p.Text
	}
	return ""
}

func (s *settingsCmdSession) enter()            { s.prog.press(tea.KeyPressMsg{Code: tea.KeyEnter}) }
func (s *settingsCmdSession) esc()              { s.prog.press(tea.KeyPressMsg{Code: tea.KeyEscape}) }
func (s *settingsCmdSession) down()             { s.prog.press(tea.KeyPressMsg{Code: tea.KeyDown}) }
func (s *settingsCmdSession) typeText(x string) { typeInto(s.prog, x) }

// readJSON reads a settings file into an object.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return obj
}

// itemLabels lists a list dialog's labels.
func itemLabels(d *ui.Dialog) []string {
	var out []string
	for _, it := range d.Items {
		out = append(out, it.Label)
	}
	return out
}

// findItem returns the list item labelled label.
func findItem(t *testing.T, d *ui.Dialog, label string) ui.ListItem {
	t.Helper()
	for _, it := range d.Items {
		if it.Label == label {
			return it
		}
	}
	t.Fatalf("dialog %q has no item %q (items: %q)", d.Title, label, itemLabels(d))
	return ui.ListItem{}
}

// TestSettingsCommandsListed asserts the commands appear in /help and in the
// command list behind the "/" menu and the ctrl+p palette.
func TestSettingsCommandsListed(t *testing.T) {
	s := startSettingsCmdSession(t, nil, nil)
	cmds := []string{"/memory", "/permissions", "/hooks", "/export", "/config", "/theme", "/usage"}
	if _, _, ok := s.prog.waitAfter(0, func(m tea.Msg) bool {
		sc, ok := m.(ui.SetCommandsMsg)
		if !ok {
			return false
		}
		have := map[string]bool{}
		for _, c := range sc.Commands {
			have[c.Value] = true
		}
		for _, c := range cmds {
			if !have[c] {
				return false
			}
		}
		return true
	}); !ok {
		t.Fatal("the command list sent to the UI lacks the settings commands")
	}
	mark := s.prog.mark()
	submitLine(s.prog, "/help")
	s.noticeAfter(t, mark, cmds...)

	// /allowed-tools resolves to /permissions, /cost to /usage.
	mark = s.prog.mark()
	submitLine(s.prog, "/allowed-tools")
	s.dialogAfter(t, mark, "Permissions")
	s.esc()
	mark = s.prog.mark()
	submitLine(s.prog, "/cost")
	s.noticeAfter(t, mark, "Session usage")
}

// TestMemoryCommand drives /memory: the picker lists the user and project
// files plus the other loaded ones, choosing one creates it and opens the
// editor, Esc cancels, and an editor failure says how to configure one.
func TestMemoryCommand(t *testing.T) {
	s := startSettingsCmdSession(t, func(home string) {
		writeSettingsFile(t, filepath.Join(home, "gocode.local.md"), "local notes\n")
	}, nil)
	userMem := filepath.Join(s.home, ".config", "gocode", "gocode.md")

	mark := s.prog.mark()
	submitLine(s.prog, "/memory")
	dlg, next := s.dialogAfter(t, mark, "Memory")
	user := findItem(t, dlg, "User memory")
	if user.Value != userMem || !strings.Contains(user.Detail, "~/.config/gocode/gocode.md") {
		t.Fatalf("user memory row = %+v", user)
	}
	project := findItem(t, dlg, "Project memory")
	if project.Value != filepath.Join(s.home, "gocode.md") {
		t.Fatalf("project memory row = %+v", project)
	}
	local := findItem(t, dlg, "~/gocode.local.md")
	if !strings.Contains(local.Detail, "Local") {
		t.Fatalf("local memory row = %+v", local)
	}

	s.enter() // the highlighted row: User memory
	msg, _ := s.noticeAfter(t, next, "Opened ~/.config/gocode/gocode.md")
	if !strings.Contains(msg, `Using $EDITOR="fake-ed"`) {
		t.Fatalf("open message lacks the editor hint: %q", msg)
	}
	if _, err := os.Stat(userMem); err != nil {
		t.Fatalf("the user memory file was not created: %v", err)
	}
	s.mu.Lock()
	edited := append([]string(nil), s.edited...)
	s.mu.Unlock()
	if len(edited) != 1 || edited[0] != userMem {
		t.Fatalf("editor opened %q, want %q", edited, userMem)
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/memory")
	s.dialogAfter(t, mark, "Memory")
	s.esc()
	s.noticeAfter(t, mark, "Cancelled memory editing")

	s.mu.Lock()
	s.editorErr = errors.New("no editor")
	s.mu.Unlock()
	mark = s.prog.mark()
	submitLine(s.prog, "/memory")
	_, next = s.dialogAfter(t, mark, "Memory")
	s.typeText("Project memory")
	s.enter()
	s.noticeAfter(t, next, "Couldn't open ~/gocode.md in an editor", "set $EDITOR or $VISUAL")
}

// TestPermissionsCommand drives /permissions: rules by behavior and source,
// adding a rule to the local project settings (file and live session),
// deleting a user rule, rejecting an invalid rule, and a read-only rule.
func TestPermissionsCommand(t *testing.T) {
	s := startSettingsCmdSession(t, func(home string) {
		writeSettingsFile(t, userSettingsPath(home), `{"theme":"dark","permissions":{"allow":["Bash(ls:*)"],"deny":["WebFetch"]}}`)
	}, func(sess *interactiveSession) {
		sess.print.AllowedTools = []string{"Read"}
	})
	localPath := filepath.Join(s.home, ".gocode", "settings.local.json")

	mark := s.prog.mark()
	submitLine(s.prog, "/permissions")
	top, next := s.dialogAfter(t, mark, "Permissions")
	if got := findItem(t, top, "Allow").Detail; !strings.HasPrefix(got, "2 rules") {
		t.Fatalf("Allow detail = %q, want 2 rules (settings + command line)", got)
	}
	if got := findItem(t, top, "Deny").Detail; !strings.HasPrefix(got, "1 rule ") {
		t.Fatalf("Deny detail = %q", got)
	}
	findItem(t, top, "Workspace")

	s.enter() // Allow
	allow, next := s.dialogAfter(t, next, "Allow rules")
	if got := findItem(t, allow, "Bash(ls:*)").Detail; got != "from user settings" {
		t.Fatalf("user rule detail = %q", got)
	}
	if got := findItem(t, allow, "Read").Detail; !strings.Contains(got, "command line") {
		t.Fatalf("cli rule detail = %q", got)
	}

	// Add a rule to the local project settings.
	s.enter() // Add a new rule…
	in, next := s.dialogAfter(t, next, "Add allow permission rule")
	if in.Kind != ui.DialogSecret || !in.Reveal {
		t.Fatalf("rule entry is not a plain text field: %+v", in)
	}
	s.typeText("Bash(npm test:*)")
	s.enter()
	where, next := s.dialogAfter(t, next, "Where should this rule be saved?")
	if labels := itemLabels(where); strings.Join(labels, "|") != "Project settings (local)|Project settings|User settings" {
		t.Fatalf("save targets = %q", labels)
	}
	s.enter() // Project settings (local)
	_, next = s.noticeAfter(t, next, "Added allow rule Bash(npm test:*)")
	local := readJSON(t, localPath)
	if allowList, _ := local["permissions"].(map[string]any)["allow"].([]any); len(allowList) != 1 || allowList[0] != "Bash(npm test:*)" {
		t.Fatalf("local settings = %v", local)
	}
	if !liveRule(s.runtime(), toolreg.RuleAllow, "Bash", "npm test:*", string(settingspolicy.SourceLocal)) {
		t.Fatal("the added rule is not live in the session")
	}

	// The list now shows it; delete the user rule.
	allow, next = s.dialogAfter(t, next, "Allow rules")
	if got := findItem(t, allow, "Bash(npm test:*)").Detail; got != "from project local settings" {
		t.Fatalf("added rule detail = %q", got)
	}
	s.typeText("ls:*")
	s.enter()
	del, next := s.dialogAfter(t, next, "Delete allow rule?")
	if !strings.Contains(strings.Join(del.Body, "\n"), "From user settings") {
		t.Fatalf("delete body = %q", del.Body)
	}
	s.enter() // Yes, delete it
	_, next = s.noticeAfter(t, next, "Deleted allow rule Bash(ls:*)")
	user := readJSON(t, userSettingsPath(s.home))
	perms, _ := user["permissions"].(map[string]any)
	if _, has := perms["allow"]; has || perms["deny"] == nil || user["theme"] != "dark" {
		t.Fatalf("user settings after delete = %v", user)
	}
	if liveRule(s.runtime(), toolreg.RuleAllow, "Bash", "ls:*", string(settingspolicy.SourceUser)) {
		t.Fatal("the deleted rule is still live")
	}

	// A command-line rule is read-only.
	_, next = s.dialogAfter(t, next, "Allow rules")
	s.typeText("Read")
	s.enter()
	ro, next := s.dialogAfter(t, next, "Rule details")
	if len(ro.Options) != 1 || ro.Options[0].Label != "Back" || !strings.Contains(strings.Join(ro.Body, "\n"), "cannot be modified") {
		t.Fatalf("read-only rule dialog = %+v", ro)
	}
	s.enter()

	// An invalid rule is refused.
	_, next = s.dialogAfter(t, next, "Allow rules")
	s.enter() // Add a new rule…
	_, next = s.dialogAfter(t, next, "Add allow permission rule")
	s.typeText("Bash()")
	s.enter()
	s.noticeAfter(t, next, "Invalid permission rule Bash()")
	_, next = s.dialogAfter(t, next, "Allow rules")
	s.esc()
	_, next = s.dialogAfter(t, next, "Permissions")
	s.esc()
	if _, _, ok := s.prog.waitAfter(next, func(m tea.Msg) bool {
		v, ok := m.(ui.AppendSystemMsg)
		return ok && v.Text == "Permissions dialog dismissed"
	}); ok {
		t.Fatal("a session with changes reported a plain dismissal")
	}
}

// liveRule reports whether the session's shared rule set holds the rule.
func liveRule(rt *sessionRuntime, b toolreg.RuleBehavior, tool, content, source string) bool {
	if rt == nil || rt.ruleStore == nil {
		return false
	}
	for _, r := range rt.ruleStore.Snapshot() {
		if r.Behavior == b && r.ToolName == tool && r.Content == content && r.Source == source {
			return true
		}
	}
	return false
}

// TestHooksCommand drives the read-only /hooks view: events with counts, the
// matchers of an event, its hooks, one hook's details, and an event with no
// hooks.
func TestHooksCommand(t *testing.T) {
	s := startSettingsCmdSession(t, func(home string) {
		writeSettingsFile(t, userSettingsPath(home), `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo pre","timeout":5}]}]}}`)
	}, nil)

	mark := s.prog.mark()
	submitLine(s.prog, "/hooks")
	top, next := s.dialogAfter(t, mark, "Hooks")
	if !strings.Contains(strings.Join(top.Body, "\n"), "read-only") || !strings.Contains(strings.Join(top.Body, "\n"), "1 hook on 1 event") {
		t.Fatalf("hooks body = %q", top.Body)
	}
	if got := findItem(t, top, "PreToolUse").Detail; got != "1 hook · Before tool execution" {
		t.Fatalf("PreToolUse detail = %q", got)
	}
	s.enter() // PreToolUse
	matchers, next := s.dialogAfter(t, next, "PreToolUse - Matchers")
	if got := findItem(t, matchers, "Bash").Detail; got != "1 hook" {
		t.Fatalf("matcher detail = %q", got)
	}
	s.enter()
	list, next := s.dialogAfter(t, next, "PreToolUse - Matcher: Bash")
	if got := findItem(t, list, "echo pre").Detail; got != "User settings" {
		t.Fatalf("hook detail = %q", got)
	}
	s.enter()
	detail, next := s.dialogAfter(t, next, "PreToolUse · Bash")
	body := strings.Join(detail.Body, "\n")
	for _, want := range []string{"Command hook", "Command: echo pre", "Timeout: 5s", "Source: User settings", "edit ~/.config/gocode/settings.json or ask gocode"} {
		if !strings.Contains(body, want) {
			t.Fatalf("hook details lack %q:\n%s", want, body)
		}
	}
	s.enter() // Back
	_, next = s.dialogAfter(t, next, "PreToolUse - Matcher: Bash")
	s.esc()
	_, next = s.dialogAfter(t, next, "PreToolUse - Matchers")
	s.esc()
	_, next = s.dialogAfter(t, next, "Hooks")
	s.typeText("Stop")
	s.enter()
	empty, next := s.dialogAfter(t, next, "Stop")
	if !strings.Contains(strings.Join(empty.Body, "\n"), "No hooks configured for this event") {
		t.Fatalf("empty event body = %q", empty.Body)
	}
	s.esc()
	_, next = s.dialogAfter(t, next, "Hooks")
	s.esc()
	s.noticeAfter(t, next, "Hooks dialog dismissed")
}

// TestExportCommand drives /export: a named file at once, the save-to-file
// flow with its default name, and the clipboard.
func TestExportCommand(t *testing.T) {
	s := startSettingsCmdSession(t, nil, nil)

	mark := s.prog.mark()
	submitLine(s.prog, "/export")
	s.noticeAfter(t, mark, "Nothing to export")

	mark = s.prog.mark()
	submitLine(s.prog, "Hello world, export me!")
	if _, _, ok := s.prog.waitAfter(mark, func(m tea.Msg) bool {
		v, ok := m.(ui.StreamAssistantMsg)
		return ok && v.Done
	}); !ok {
		t.Fatal("the prompt was not answered")
	}
	s.runtime() // the turn has run; let it settle before reading the history
	time.Sleep(50 * time.Millisecond)

	mark = s.prog.mark()
	submitLine(s.prog, "/export notes")
	s.noticeAfter(t, mark, "Conversation exported to: "+filepath.Join(s.home, "notes.txt"))
	data, err := os.ReadFile(filepath.Join(s.home, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "> Hello world, export me!") || !strings.Contains(got, "⏺ ok") {
		t.Fatalf("export content = %q", got)
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/export")
	method, next := s.dialogAfter(t, mark, "Export conversation")
	if labels := itemLabels(method); strings.Join(labels, "|") != "Copy to clipboard|Save to file" {
		t.Fatalf("export methods = %q", labels)
	}
	s.down()
	s.enter()
	name, next := s.dialogAfter(t, next, "Export conversation")
	if name.Text() != "2026-01-02-150405-hello-world-export-me.txt" {
		t.Fatalf("default file name = %q", name.Text())
	}
	s.enter()
	want := filepath.Join(s.home, "2026-01-02-150405-hello-world-export-me.txt")
	s.noticeAfter(t, next, "Conversation exported to: "+want)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("export file missing: %v", err)
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/export")
	_, next = s.dialogAfter(t, mark, "Export conversation")
	s.enter() // Copy to clipboard
	if _, _, ok := s.prog.waitAfter(next, func(m tea.Msg) bool {
		c, ok := m.(ui.CopyToClipboardMsg)
		return ok && strings.Contains(c.Text, "> Hello world, export me!")
	}); !ok {
		t.Fatal("the conversation was not put on the clipboard")
	}
	s.noticeAfter(t, next, "Conversation copied to clipboard")
}

// TestConfigCommand drives the settings screen and "/config key=value": a
// toggle saved to the user settings and applied live, a list setting, the
// summary on close, the precedence note, and an unknown key.
func TestConfigCommand(t *testing.T) {
	s := startSettingsCmdSession(t, func(home string) {
		writeSettingsFile(t, userSettingsPath(home), `{"model":"keep-me"}`)
		writeSettingsFile(t, filepath.Join(home, ".gocode", "settings.local.json"), `{"permissions":{"defaultMode":"plan"}}`)
	}, nil)

	mark := s.prog.mark()
	submitLine(s.prog, "/config")
	screen, next := s.dialogAfter(t, mark, "Settings")
	for _, label := range []string{"Theme", "Model", "Provider", "Default permission mode", "Auto-compact", "Rewind code (checkpoints)", "Respect .gitignore in file search", "UI style", "Icons", "Animations", "Sidebar"} {
		findItem(t, screen, label)
	}
	if got := findItem(t, screen, "Auto-compact").Detail; got != "true" {
		t.Fatalf("Auto-compact = %q", got)
	}
	if got := findItem(t, screen, "Default permission mode").Detail; got != "plan" {
		t.Fatalf("Default permission mode = %q", got)
	}

	s.typeText("Auto-compact")
	s.enter()
	screen, next = s.dialogAfter(t, next, "Settings")
	if got := findItem(t, screen, "Auto-compact").Detail; got != "false" {
		t.Fatalf("Auto-compact after toggle = %q", got)
	}
	if rt := s.runtime(); rt == nil || !rt.autoCompactOff.Load() {
		t.Fatal("auto-compact was not turned off for the live session")
	}

	s.typeText("Theme")
	s.enter()
	_, next = s.dialogAfter(t, next, "Theme")
	s.typeText("daylight")
	s.enter()
	if _, _, ok := s.prog.waitAfter(next, func(m tea.Msg) bool {
		v, ok := m.(ui.SetThemeMsg)
		return ok && v.Name == "daylight"
	}); !ok {
		t.Fatal("the theme was not applied live")
	}
	_, next = s.dialogAfter(t, next, "Settings")
	s.esc()
	summary, _ := s.noticeAfter(t, next, "Auto-compact set to false", "Theme set to daylight")
	if strings.Contains(summary, "unchanged") {
		t.Fatalf("summary = %q", summary)
	}
	user := readJSON(t, userSettingsPath(s.home))
	if user["autoCompactEnabled"] != false || user["model"] != "keep-me" {
		t.Fatalf("user settings = %v", user)
	}
	if uiObj, _ := user["ui"].(map[string]any); uiObj["theme"] != "daylight" {
		t.Fatalf("ui.theme not saved: %v", user)
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/config autocompact=on")
	s.noticeAfter(t, mark, "Auto-compact set to true")
	if s.runtime().autoCompactOff.Load() {
		t.Fatal("/config autocompact=on did not turn auto-compact back on")
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/config defaultMode=acceptEdits")
	s.noticeAfter(t, mark, "Default permission mode set to acceptEdits (applies next session)", "project local settings also sets permissions.defaultMode")
	if perms, _ := readJSON(t, userSettingsPath(s.home))["permissions"].(map[string]any); perms["defaultMode"] != "acceptEdits" {
		t.Fatalf("defaultMode not saved to user settings")
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/config ui.sidebar=off")
	s.noticeAfter(t, mark, "Sidebar set to off")
	if _, _, ok := s.prog.waitAfter(mark, func(m tea.Msg) bool {
		v, ok := m.(ui.ApplyUISettingMsg)
		return ok && v.Key == "ui.sidebar" && v.Value == "off"
	}); !ok {
		t.Fatal("ui.sidebar was not applied live")
	}

	mark = s.prog.mark()
	submitLine(s.prog, "/config bogus=1")
	s.noticeAfter(t, mark, `Unknown setting "bogus"`)

	mark = s.prog.mark()
	submitLine(s.prog, "/config autocompact=maybe")
	s.noticeAfter(t, mark, "autoCompactEnabled takes true or false")

	mark = s.prog.mark()
	submitLine(s.prog, "/theme light")
	s.noticeAfter(t, mark, "Theme set to light")

	mark = s.prog.mark()
	submitLine(s.prog, "/config")
	_, next = s.dialogAfter(t, mark, "Settings")
	s.esc()
	s.noticeAfter(t, next, "Settings unchanged")
}

// TestTextInputDialog covers the plain text field: initial value, typing,
// backspace, and the entered text as the confirmed value.
func TestTextInputDialog(t *testing.T) {
	d := ui.TextInputDialog("Name", "abc", "Enter filename:")
	if d.Masked() != "abc" || d.Text() != "abc" {
		t.Fatalf("initial = %q / %q", d.Masked(), d.Text())
	}
	d.AppendSecret("d")
	d.BackspaceSecret()
	d.BackspaceSecret()
	d.AppendSecret("x")
	if got := d.ChosenValues(); len(got) != 1 || got[0] != "abx" {
		t.Fatalf("value = %q", got)
	}
	if s := ui.SecretDialog("Key"); s.Text() != "" {
		t.Fatal("a secret dialog hands out its value through Text")
	}
}

// TestExportHelpers covers the default file name, the path resolution and
// the transcript rendering.
func TestExportHelpers(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local)
	hist := []agent.Message{
		{Role: agent.RoleUser, Content: []agent.ContentBlock{agent.TextBlock("<system-reminder>x</system-reminder>"), agent.TextBlock("Fix the BUG in main.go, please!")}},
		{Role: agent.RoleAssistant, Content: []agent.ContentBlock{agent.TextBlock("Looking."), agent.ToolUseBlock("t1", "Bash", json.RawMessage(`{"command":"go test ./..."}`))}},
		{Role: agent.RoleUser, Content: []agent.ContentBlock{agent.ToolResultBlock("t1", "ok\nPASS", false)}},
	}
	if got := exportFileName(hist, now); got != "2026-03-04-050607-fix-the-bug-in-maingo-please.txt" {
		t.Fatalf("file name = %q", got)
	}
	if got := exportFileName(nil, now); got != "conversation-2026-03-04-050607.txt" {
		t.Fatalf("empty file name = %q", got)
	}
	if got := exportPath("/w", "out"); got != filepath.Join("/w", "out.txt") {
		t.Fatalf("path = %q", got)
	}
	if got := exportPath("/w", "a/b.md"); got != filepath.Join("/w", "a", "b.md") {
		t.Fatalf("path = %q", got)
	}
	text := exportTranscript(hist)
	for _, want := range []string{"> Fix the BUG in main.go, please!", "⏺ Looking.", "⏺ Bash(go test ./...)", "  ⎿  ok\n     PASS"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "system-reminder") {
		t.Fatalf("transcript shows a system reminder:\n%s", text)
	}
}

// TestResolveEditor covers the editor choice: $VISUAL, then $EDITOR, then a
// default on PATH, else an error; and the hint line.
func TestResolveEditor(t *testing.T) {
	env := map[string]string{"VISUAL": "code --wait", "EDITOR": "vim"}
	get := func(k string) string { return env[k] }
	none := func(string) (string, error) { return "", errors.New("missing") }
	c, err := resolveEditor(get, none)
	if err != nil || c.source != "$VISUAL" || strings.Join(c.argv, " ") != "code --wait" || !isGUIEditor(c.argv) {
		t.Fatalf("VISUAL choice = %+v, %v", c, err)
	}
	delete(env, "VISUAL")
	c, _ = resolveEditor(get, none)
	if c.source != "$EDITOR" || isGUIEditor(c.argv) || !strings.Contains(editorHint(c), `$EDITOR="vim"`) {
		t.Fatalf("EDITOR choice = %+v", c)
	}
	delete(env, "EDITOR")
	if _, err := resolveEditor(get, none); !errors.Is(err, errNoEditor) {
		t.Fatalf("no editor: err = %v", err)
	}
	found := func(name string) (string, error) {
		if name == defaultEditors()[len(defaultEditors())-1] {
			return "/bin/" + name, nil
		}
		return "", errors.New("missing")
	}
	c, err = resolveEditor(get, found)
	if err != nil || c.source != "default" || !strings.Contains(editorHint(c), "set the $EDITOR or $VISUAL") {
		t.Fatalf("default choice = %+v, %v", c, err)
	}
}
