package sessionstore

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
)

// TestRenameLatestWins: custom-title lines are appended, the latest titles
// the session, and the picker title prefers it over the first prompt.
func TestRenameLatestWins(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/p"
	sess, _ := st.Create(cwd, "s1")
	if err := sess.AppendMessages([]agent.Message{userMsg("fix the parser"), asstMsg("ok")}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rename("first name"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rename("  parser   work "); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rename(" "); err == nil {
		t.Error("empty title accepted")
	}
	m, ok := st.Find(cwd, "s1")
	if !ok {
		t.Fatal("not found")
	}
	if m.CustomTitle != "parser work" || m.Title != "parser work" || m.FirstPrompt != "fix the parser" {
		t.Errorf("meta = %+v", m)
	}
	// Meta lines never enter the history or the chain.
	msgs, _ := st.Load(cwd, "s1")
	if len(msgs) != 2 || m.MessageCount != 2 {
		t.Errorf("msgs=%d count=%d", len(msgs), m.MessageCount)
	}
	data, _ := os.ReadFile(sess.Path())
	if !strings.Contains(string(data), `{"type":"custom-title","customTitle":"parser work","sessionId":"s1"`) {
		t.Errorf("custom-title line not in the expected shape:\n%s", data)
	}
	// Appending after a rename continues the turn chain past the meta line.
	if err := sess.AppendMessages([]agent.Message{userMsg("more")}); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := st.Load(cwd, "s1"); len(msgs) != 3 {
		t.Errorf("after append: %d msgs", len(msgs))
	}
}

// TestFindPrefersCustomTitle: a custom title wins over another session whose
// derived title merely contains the value.
func TestFindPrefersCustomTitle(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/w"
	a, _ := st.Create(cwd, "aaaa")
	_ = a.AppendMessages([]agent.Message{userMsg("deploy notes")})
	b, _ := st.Create(cwd, "bbbb")
	_ = b.AppendMessages([]agent.Message{userMsg("something else")})
	_ = b.Rename("deploy")
	// Make a the most recent so a plain title match would pick it.
	_ = a.AppendMessages([]agent.Message{asstMsg("later")})
	if m, ok := st.Find(cwd, "DEPLOY"); !ok || m.ID != "bbbb" {
		t.Errorf("Find(deploy) = %+v %v, want bbbb", m, ok)
	}
	if m, ok := st.Find(cwd, "notes"); !ok || m.ID != "aaaa" {
		t.Errorf("Find(notes) = %+v %v, want aaaa", m, ok)
	}
}

// TestTags: tag lines carry the whole set; the latest wins; an empty set
// clears.
func TestTags(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/w"
	sess, _ := st.Create(cwd, "s")
	_ = sess.AppendMessages([]agent.Message{userMsg("hi")})
	if err := sess.SetTags([]string{"wip", "bug", "wip", " "}, "bug"); err != nil {
		t.Fatal(err)
	}
	if got := sess.Tags(); !reflect.DeepEqual(got, []string{"bug", "wip"}) {
		t.Errorf("tags = %v", got)
	}
	m, _ := st.Find(cwd, "s")
	if !reflect.DeepEqual(m.Tags, []string{"bug", "wip"}) {
		t.Errorf("meta tags = %v", m.Tags)
	}
	data, _ := os.ReadFile(sess.Path())
	if !strings.Contains(string(data), `{"type":"tag","tag":"bug","tags":["bug","wip"]`) {
		t.Errorf("tag line shape:\n%s", data)
	}
	if err := st.SetTags(m, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := sess.Tags(); len(got) != 0 {
		t.Errorf("after clear: %v", got)
	}
	// A single-tag line (no "tags" field) is read as a one-tag set.
	f, _ := os.OpenFile(sess.Path(), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"tag","tag":"solo","sessionId":"s"}` + "\n")
	f.Close()
	if got := sess.Tags(); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Errorf("single-tag line: %v", got)
	}
}

// recordedUUIDs returns the uuids of the chain lines in file order.
func recordedUUIDs(t *testing.T, st *Store, m Meta) []string {
	t.Helper()
	recs, _, err := st.Records(m, true)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.UUID)
	}
	return out
}

// TestResumeAtBranches: LoadAt truncates at a message, OpenAt branches the
// transcript there, and Load then follows the new branch while the old lines
// stay on disk.
func TestResumeAtBranches(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/w"
	sess, _ := st.Create(cwd, "s")
	_ = sess.AppendMessages([]agent.Message{userMsg("one"), asstMsg("r1"), userMsg("two"), asstMsg("r2")})
	m, _ := st.Find(cwd, "s")
	uuids := recordedUUIDs(t, st, m)
	if len(uuids) != 4 {
		t.Fatalf("uuids = %v", uuids)
	}
	at := uuids[1] // the first reply

	msgs, err := st.LoadAt(cwd, "s", at)
	if err != nil || len(msgs) != 2 || messageText(msgs[1]) != "r1" {
		t.Fatalf("LoadAt = %v, %v", msgs, err)
	}
	if _, err := st.LoadAt(cwd, "s", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadAt(unknown) err = %v", err)
	}

	re, err := st.OpenAt(cwd, "s", at, len(msgs))
	if err != nil {
		t.Fatal(err)
	}
	_ = re.AppendMessages([]agent.Message{userMsg("two again"), asstMsg("r2b")})

	got, _ := st.Load(cwd, "s")
	var texts []string
	for _, g := range got {
		texts = append(texts, messageText(g))
	}
	if strings.Join(texts, ",") != "one,r1,two again,r2b" {
		t.Errorf("branch load = %v", texts)
	}
	m, _ = st.Find(cwd, "s")
	if m.MessageCount != 4 {
		t.Errorf("count = %d", m.MessageCount)
	}
	// The abandoned branch is still on disk and can itself be resumed at.
	if old, err := st.LoadAt(cwd, "s", uuids[3]); err != nil || len(old) != 4 || messageText(old[3]) != "r2" {
		t.Errorf("old branch = %v %v", old, err)
	}
	// A further plain resume continues the active branch.
	cont, _ := st.Open(cwd, "s", 4)
	_ = cont.AppendMessages([]agent.Message{userMsg("three")})
	if got, _ := st.Load(cwd, "s"); len(got) != 5 || messageText(got[4]) != "three" || messageText(got[2]) != "two again" {
		t.Errorf("continue on branch = %d msgs", len(got))
	}
}

// TestBranchAfterCompaction: a branch walk stops at the compaction boundary.
func TestBranchAfterCompaction(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/w"
	sess, _ := st.Create(cwd, "s")
	_ = sess.AppendMessages([]agent.Message{userMsg("old"), asstMsg("old r")})
	summary := summaryMsg("the old part")
	_ = sess.AppendCompaction(compaction.BoundaryMarker{Trigger: compaction.TriggerManual}, []agent.Message{summary, userMsg("a")})
	_ = sess.AppendMessages([]agent.Message{asstMsg("ra"), userMsg("b"), asstMsg("rb")})
	m, _ := st.Find(cwd, "s")
	recs, compactions, _ := st.Records(m, false)
	if compactions != 1 || len(recs) != 5 {
		t.Fatalf("active records = %d, compactions = %d", len(recs), compactions)
	}
	at := recs[2].UUID // "ra"
	re, _ := st.OpenAt(cwd, "s", at, 3)
	_ = re.AppendMessages([]agent.Message{userMsg("c")})
	got, _ := st.Load(cwd, "s")
	var texts []string
	for _, g := range got[1:] {
		texts = append(texts, messageText(g))
	}
	if len(got) != 4 || strings.Join(texts, ",") != "a,ra,c" {
		t.Errorf("load = %d %v", len(got), texts)
	}
}

// TestListAllAndFindAll spans project directories and fills Cwd from the
// transcript.
func TestListAllAndFindAll(t *testing.T) {
	st := New(t.TempDir())
	a, _ := st.Create("/proj/a", "id-a")
	_ = a.AppendMessages([]agent.Message{userMsg("in a")})
	b, _ := st.Create("/proj/b", "id-b")
	_ = b.AppendMessages([]agent.Message{userMsg("in b")})
	all, err := st.ListAll()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListAll = %v %v", all, err)
	}
	m, ok := st.FindAll("id-a")
	if !ok || m.Cwd != "/proj/a" {
		t.Errorf("FindAll = %+v %v", m, ok)
	}
	if _, ok := st.Find("/proj/b", "id-a"); ok {
		t.Error("Find crossed projects")
	}
	if err := st.Delete(m); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.FindAll("id-a"); ok {
		t.Error("deleted session still listed")
	}
}
