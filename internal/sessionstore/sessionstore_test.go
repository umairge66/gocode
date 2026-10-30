package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

func userMsg(text string) agent.Message { return agent.UserMessage(agent.TextBlock(text)) }
func asstMsg(text string) agent.Message { return agent.AssistantMessage(agent.TextBlock(text)) }

// TestCreateAppendLoad is the core round-trip: create a session, append turns as
// a conversation would, and reload the exact message list back.
func TestCreateAppendLoad(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/project"

	sess, err := st.Create(cwd, "sess-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Nothing written yet → file must not exist (lazy creation).
	if _, err := os.Stat(sess.Path()); !os.IsNotExist(err) {
		t.Fatalf("session file created before first write: %v", err)
	}

	if err := sess.AppendMessages([]agent.Message{userMsg("hello"), asstMsg("hi there")}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	// A second append (next turn) extends the same file.
	if err := sess.AppendMessages([]agent.Message{userMsg("bye")}); err != nil {
		t.Fatalf("AppendMessages 2: %v", err)
	}

	msgs, err := st.Load(cwd, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("loaded %d messages, want 3", len(msgs))
	}
	if msgs[0].Role != agent.RoleUser || messageText(msgs[0]) != "hello" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != agent.RoleAssistant || messageText(msgs[1]) != "hi there" {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Role != agent.RoleUser || messageText(msgs[2]) != "bye" {
		t.Errorf("msg2 = %+v", msgs[2])
	}

	// The transcript is valid JSONL under the expected path layout.
	want := filepath.Join(base, "projects", EscapeCwd(cwd), "sess-1.jsonl")
	if sess.Path() != want {
		t.Errorf("path = %q, want %q", sess.Path(), want)
	}
	data, _ := os.ReadFile(want)
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if e.Type == "" || e.Message == nil || e.SessionID != "sess-1" || e.GocodeSchema != SchemaVersion {
			t.Errorf("line %d malformed envelope: %+v", i, e)
		}
	}
}

// TestEscapeCwd checks the replace(/[^a-zA-Z0-9]/g,"-") rule.

func TestEscapeCwd(t *testing.T) {
	cases := map[string]string{
		"/home/user/foo": "-home-user-foo",
		"/a.b/c":         "-a-b-c",
		"C:\\win\\dir":   "C--win-dir",
		"simple":         "simple",
		"with space/x":   "with-space-x",
	}
	for in, want := range cases {
		if got := EscapeCwd(in); got != want {
			t.Errorf("EscapeCwd(%q) = %q, want %q", in, got, want)
		}
	}
	// Overflow: >200 chars is capped and hash-suffixed.
	long := "/" + strings.Repeat("a", 300)
	got := EscapeCwd(long)
	if len(got) <= cwdNameCap || len(got) > cwdNameCap+16 {
		t.Errorf("overflow escape length = %d, want cap(%d)+hash", len(got), cwdNameCap)
	}
}

// TestListMostRecentFirst verifies List returns sessions newest-first with
// titles derived from the first user message.
func TestListMostRecentFirst(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/proj"

	// Three sessions written with increasing mtimes.
	mk := func(id, firstPrompt string, mod time.Time) {
		s, err := st.Create(cwd, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessages([]agent.Message{userMsg(firstPrompt), asstMsg("ok")}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(s.Path(), mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	mk("old", "first old prompt", now.Add(-3*time.Hour))
	mk("mid", "middle prompt", now.Add(-2*time.Hour))
	mk("new", "newest prompt", now.Add(-1*time.Hour))

	metas, err := st.List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("got %d sessions, want 3", len(metas))
	}
	if metas[0].ID != "new" || metas[1].ID != "mid" || metas[2].ID != "old" {
		t.Errorf("order = %s,%s,%s; want new,mid,old", metas[0].ID, metas[1].ID, metas[2].ID)
	}
	if metas[0].Title != "newest prompt" {
		t.Errorf("title = %q, want first user message", metas[0].Title)
	}
	if metas[0].MessageCount != 2 {
		t.Errorf("message count = %d, want 2", metas[0].MessageCount)
	}

	// Latest resolves to the newest.
	latest, ok := st.Latest(cwd)
	if !ok || latest.ID != "new" {
		t.Errorf("Latest = %v,%v; want new", latest.ID, ok)
	}

	// Find resolves by exact id, by prefix, and by title substring.
	if m, ok := st.Find(cwd, "mid"); !ok || m.ID != "mid" {
		t.Errorf("Find exact id failed: %v %v", m.ID, ok)
	}
	if m, ok := st.Find(cwd, "o"); !ok || m.ID != "old" {
		t.Errorf("Find unique prefix failed: %v %v", m.ID, ok)
	}
	if m, ok := st.Find(cwd, "middle"); !ok || m.ID != "mid" {
		t.Errorf("Find by title substring failed: %v %v", m.ID, ok)
	}
}

// TestSummaryTitle: a summary line supplies the picker title over the first
// user message.
func TestSummaryTitle(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/s"
	dir := st.ProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"summary","summary":"Refactor the parser","leafUuid":"x"}`,
		`{"type":"user","sessionId":"s1","message":{"role":"user","content":[{"type":"text","text":"please refactor"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metas, _ := st.List(cwd)
	if len(metas) != 1 {
		t.Fatalf("got %d, want 1", len(metas))
	}
	if metas[0].Title != "Refactor the parser" {
		t.Errorf("title = %q, want summary", metas[0].Title)
	}
	msgs, err := st.Load(cwd, "s1")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Load: %v msgs=%d (summary line must be skipped)", err, len(msgs))
	}
}

// TestMalformedAndMissingTolerated: a corrupt line is skipped, and a missing
// session/project is handled cleanly.
func TestMalformedAndMissingTolerated(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/m"
	dir := st.ProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"good"}]}}`,
		`this is not json`,
		``,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"also good"}]}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.Load(cwd, "a")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (corrupt+blank skipped)", len(msgs))
	}

	// Missing session file → error (not a panic).
	if _, err := st.Load(cwd, "nope"); err == nil {
		t.Errorf("Load of missing session should error")
	}
	// Missing project dir → empty list, no error.
	if metas, err := st.List("/never/seen"); err != nil || len(metas) != 0 {
		t.Errorf("List of missing project = %v,%v; want nil,nil", metas, err)
	}
	if _, ok := st.Latest("/never/seen"); ok {
		t.Errorf("Latest of missing project should be !ok")
	}
}

// TestReplaceHistory rewrites the transcript (compaction path).
func TestReplaceHistory(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/r"
	s, err := st.Create(cwd, "r1")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendMessages([]agent.Message{userMsg("a"), asstMsg("b"), userMsg("c")})
	// Compaction rebuilds history to a single summary turn.
	if err := s.ReplaceHistory([]agent.Message{userMsg("[summary] earlier conversation")}); err != nil {
		t.Fatalf("ReplaceHistory: %v", err)
	}
	msgs, _ := st.Load(cwd, "r1")
	if len(msgs) != 1 || !strings.Contains(messageText(msgs[0]), "summary") {
		t.Fatalf("after rewrite got %d msgs: %+v", len(msgs), msgs)
	}
}

// TestNoConfigHome: a Store without a base degrades to clean errors, never a
// panic or a stray write.
func TestNoConfigHome(t *testing.T) {
	st := New("")
	if _, err := st.Create("/x", "id"); err == nil {
		t.Error("Create with no base should error")
	}
	if _, err := st.Load("/x", "id"); err == nil {
		t.Error("Load with no base should error")
	}
	if metas, err := st.List("/x"); err == nil || metas != nil {
		t.Error("List with no base should error")
	}
}

// TestCleanup prunes transcripts older than the cutoff (age-based retention).
func TestCleanup(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/c"
	mk := func(id string, mod time.Time) {
		s, _ := st.Create(cwd, id)
		_ = s.AppendMessages([]agent.Message{userMsg("x")})
		_ = os.Chtimes(s.Path(), mod, mod)
	}
	now := time.Now()
	mk("fresh", now.Add(-1*time.Hour))
	mk("stale", now.Add(-40*24*time.Hour))

	cutoff := now.Add(-30 * 24 * time.Hour)
	if n := st.Cleanup(cwd, cutoff); n != 1 {
		t.Fatalf("Cleanup removed %d, want 1", n)
	}
	metas, _ := st.List(cwd)
	if len(metas) != 1 || metas[0].ID != "fresh" {
		t.Fatalf("after cleanup sessions = %+v, want [fresh]", metas)
	}
	// A zero cutoff disables cleanup.
	if n := st.Cleanup(cwd, time.Time{}); n != 0 {
		t.Errorf("zero-cutoff cleanup removed %d, want 0", n)
	}
}
