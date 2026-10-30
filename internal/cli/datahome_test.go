package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/planmode"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/toolreg"
)

// legacyFixture fills the former per-user locations of a temp HOME: a saved
// conversation for cwd that names a saved tool result, file history, a plan
// and a ~/.gocode.json trusting another folder.
type legacyFixture struct {
	home, cwd, trusted, root, legacy string
	sid, oldResult                   string
}

func newLegacyFixture(t *testing.T, home, cwd string) legacyFixture {
	t.Helper()
	l := legacyFixture{
		home:    home,
		cwd:     cwd,
		trusted: filepath.Join(filepath.Dir(cwd), "trusted-repo"),
		root:    filepath.Join(home, ".config", "gocode"),
		legacy:  filepath.Join(home, ".gocode"),
		sid:     "11111111-2222-4333-8444-555555555555",
	}
	for _, d := range []string{cwd, l.trusted} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st := sessionstore.New(l.legacy)
	l.oldResult = filepath.Join(st.ProjectDir(cwd), l.sid, "tool-results", "call-1.txt")
	subcmdWriteFile(t, l.oldResult, "the full saved output")
	sess, err := st.Create(cwd, l.sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessages([]agent.Message{
		agent.UserMessage(agent.TextBlock("earlier question")),
		agent.UserMessage(agent.ToolResultBlock("call-1", "Output too large. Full output saved to: "+l.oldResult, false)),
		agent.AssistantMessage(agent.TextBlock("earlier answer")),
	}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(sess.Path(), old, old)
	subcmdWriteFile(t, filepath.Join(l.legacy, "file-history", l.sid, "snapshots.json"), `{"snapshots":[]}`)
	subcmdWriteFile(t, filepath.Join(l.legacy, "plans", "calm-river.md"), "# Plan\n")
	state, _ := json.Marshal(map[string]any{
		"theme":    "dark",
		"projects": map[string]any{canonicalDir(l.trusted): map[string]any{"hasTrustDialogAccepted": true}},
	})
	subcmdWriteFile(t, filepath.Join(home, ".gocode.json"), string(state))
	return l
}

// TestRunMovesFormerDataIntoRootOnce: the first invocation moves ~/.gocode and
// ~/.gocode.json into the root and says so once on stderr; transcripts, file
// history, plans and trust decisions are then found under the root.
func TestRunMovesFormerDataIntoRootOnce(t *testing.T) {
	f := newSubcmdFixture(t)
	l := newLegacyFixture(t, f.home, f.cwd)

	code, out, errOut := f.run(t, "trust", f.cwd)
	if code != 0 {
		t.Fatalf("trust exit %d: %s", code, errOut)
	}
	notice := "gocode: moved data from " + filepath.Join("~", ".gocode") + " to " + filepath.Join("~", ".config", "gocode")
	if strings.Count(errOut, notice) != 1 {
		t.Fatalf("stderr = %q, want the notice once", errOut)
	}
	if !strings.Contains(out, filepath.Join(l.root, ".gocode.json")) {
		t.Fatalf("trust did not save into the root state file: %s", out)
	}
	for _, p := range []string{l.legacy, filepath.Join(f.home, ".gocode.json")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still exists", p)
		}
	}
	for _, rel := range []string{
		filepath.Join("file-history", l.sid, "snapshots.json"),
		filepath.Join("plans", "calm-river.md"),
	} {
		if _, err := os.Stat(filepath.Join(l.root, rel)); err != nil {
			t.Errorf("%s not in the root: %v", rel, err)
		}
	}

	// The migrated trust entry and the new one both count.
	env, _, _ := f.env()
	useRealTrust(env)
	if !folderTrustedInStore(env, l.trusted) || !folderTrustedInStore(env, f.cwd) {
		t.Fatal("trusted folders not honoured after the move")
	}

	// The resume picker and the welcome screen's recent sessions list the
	// moved transcript.
	paths := resolveSessionPaths(env)
	if base := sessionConfigHome(env, paths); base != l.root {
		t.Fatalf("session home = %q, want %q", base, l.root)
	}
	store := sessionstore.New(sessionConfigHome(env, paths))
	metas, err := store.List(f.cwd)
	if err != nil || len(metas) != 1 || metas[0].ID != l.sid {
		t.Fatalf("sessions = %+v %v", metas, err)
	}
	if recent := recentSessionsMsg(store, f.cwd, "other"); len(recent.Sessions) != 1 {
		t.Fatalf("recent sessions = %+v", recent)
	}

	// Tool results and plan files resolve under the root.
	if dir := toolResultsDir(env, paths, "s2"); !strings.HasPrefix(dir, l.root+string(filepath.Separator)) {
		t.Fatalf("tool-results dir %q outside the root", dir)
	}
	if p := planmode.NewPlans(sessionConfigHome(env, paths), f.cwd, "", "s2"); p == nil || p.Dir != filepath.Join(l.root, planmode.DirName) {
		t.Fatalf("plans = %+v", p)
	}

	// Second start: nothing to say.
	if code, _, errOut := f.run(t, "trust", f.cwd); code != 0 || strings.Contains(errOut, "moved data") {
		t.Fatalf("second run: exit %d stderr %q", code, errOut)
	}
}

// TestRunWithConfigDirOverrideLeavesFormerData: with GOCODE_CONFIG_DIR set the
// override is the root and the home-directory data stays where it is.
func TestRunWithConfigDirOverrideLeavesFormerData(t *testing.T) {
	f := newSubcmdFixture(t)
	l := newLegacyFixture(t, f.home, f.cwd)
	override := filepath.Join(t.TempDir(), "custom")
	f.vars["GOCODE_CONFIG_DIR"] = override

	code, out, errOut := f.run(t, "trust", f.cwd)
	if code != 0 || strings.Contains(errOut, "moved data") {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	if !strings.Contains(out, filepath.Join(override, ".gocode.json")) {
		t.Fatalf("state not saved under the override: %s", out)
	}
	if _, err := os.Stat(filepath.Join(l.legacy, "plans", "calm-river.md")); err != nil {
		t.Fatal("former data moved despite the override")
	}
}

// TestResumeAfterMoveReadsSavedToolResults: a resumed conversation's saved
// tool-result paths point into the root, and the read-only tools may open
// them without a prompt.
func TestResumeAfterMoveReadsSavedToolResults(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	l := newLegacyFixture(t, home, cwd)
	env, _, errb := newTestEnv()
	env.Getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env.Cwd = func() (string, error) { return cwd, nil }
	migrateDataHome(env)
	if !strings.Contains(errb.String(), "moved data") {
		t.Fatalf("no move: %q", errb.String())
	}
	_ = takeDataHomeNotice()

	caller := &resumeRecCaller{}
	var rt *sessionRuntime
	s := &printSession{
		Mode:      toolreg.ModeDefault,
		Provider:  providerOpenRouter,
		Model:     "m",
		newCaller: func(*printSession) (agent.ModelCaller, error) { return caller, nil },
		resume:    resumeIntent{resume: true, value: l.sid},
		onSession: func(r *sessionRuntime) { rt = r },
	}
	if code := runPrintSession(env, s, "follow-up"); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	newResult := filepath.Join(sessionstore.New(l.root).ProjectDir(cwd), l.sid, "tool-results", "call-1.txt")
	if b, err := os.ReadFile(newResult); err != nil || string(b) != "the full saved output" {
		t.Fatalf("moved tool result = %q %v", b, err)
	}
	var sawNew, sawOld bool
	for _, m := range caller.lastSeen() {
		for _, b := range m.Content {
			sawNew = sawNew || strings.Contains(b.Content, newResult)
			sawOld = sawOld || strings.Contains(b.Content, l.oldResult)
		}
	}
	if !sawNew || sawOld {
		t.Fatalf("resumed history paths: new=%v old=%v", sawNew, sawOld)
	}
	if rt == nil || rt.dispatcher == nil || rt.dispatcher.Decider == nil {
		t.Fatal("no runtime")
	}
	want := filepath.Dir(newResult)
	found := false
	for _, d := range rt.dispatcher.Decider.ReadableDirs {
		found = found || d == want
	}
	if !found {
		t.Fatalf("readable dirs %v lack %s", rt.dispatcher.Decider.ReadableDirs, want)
	}
}
