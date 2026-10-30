package sessionstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
)

// rawLines decodes every line of a transcript as a generic map (so the test
// sees exactly what is on disk, including null parentUuid).
func rawLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func summaryMsg(text string) agent.Message {
	m := compaction.BuildSummaryMessage(text, false)
	return agent.Message{Role: m.Role, Content: m.Content}
}

func texts(ms []agent.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, messageText(m))
	}
	return out
}

// TestAppendCompactionKeepsOriginalLines: N messages then a compaction → the file
// still holds all N original lines, then the boundary, then

// the isCompactSummary summary and the kept tail chained after it; Load yields
// exactly the compacted history.
func TestAppendCompactionKeepsOriginalLines(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/compact"
	s, err := st.Create(cwd, "c1")
	if err != nil {
		t.Fatal(err)
	}
	orig := []agent.Message{userMsg("first prompt"), asstMsg("a1"), userMsg("q2"), asstMsg("a2")}
	if err := s.AppendMessages(orig); err != nil {
		t.Fatal(err)
	}
	summary := summaryMsg("Summary:\nearlier work")
	kept := asstMsg("kept tail")
	marker := compaction.BoundaryMarker{Content: "Conversation compacted", Trigger: compaction.TriggerManual, PreTokens: 1234, MessagesSummarized: 3}
	if err := s.AppendCompaction(marker, []agent.Message{summary, kept}); err != nil {
		t.Fatal(err)
	}

	lines := rawLines(t, s.Path())
	if len(lines) != len(orig)+3 {
		t.Fatalf("got %d lines, want %d (originals + boundary + summary + kept)", len(lines), len(orig)+3)
	}
	for i := range orig {
		msg, _ := lines[i]["message"].(map[string]any)
		if msg == nil {
			t.Fatalf("original line %d lost its message", i)
		}
	}
	if lines[0]["parentUuid"] != nil {
		t.Errorf("first line parentUuid = %v, want null", lines[0]["parentUuid"])
	}
	lastOrigUUID := lines[len(orig)-1]["uuid"]
	b := lines[len(orig)]
	if b["type"] != "system" || b["subtype"] != "compact_boundary" || b["content"] != "Conversation compacted" ||
		b["level"] != "info" || b["isMeta"] != false {
		t.Fatalf("boundary envelope = %+v", b)
	}
	if v, ok := b["parentUuid"]; !ok || v != nil {
		t.Errorf("boundary parentUuid = %v (present %v), want explicit null", v, ok)
	}
	if b["logicalParentUuid"] != lastOrigUUID {
		t.Errorf("boundary logicalParentUuid = %v, want %v", b["logicalParentUuid"], lastOrigUUID)
	}
	md, _ := b["compactMetadata"].(map[string]any)
	if md["trigger"] != "manual" || md["preTokens"] != float64(1234) || md["messagesSummarized"] != float64(3) {
		t.Errorf("compactMetadata = %+v", md)
	}
	sum := lines[len(orig)+1]
	if sum["isCompactSummary"] != true || sum["isVisibleInTranscriptOnly"] != true || sum["type"] != "user" {
		t.Errorf("summary line flags = %+v", sum)
	}
	if sum["parentUuid"] != b["uuid"] {
		t.Errorf("summary parentUuid = %v, want boundary uuid %v", sum["parentUuid"], b["uuid"])
	}
	if lines[len(orig)+2]["parentUuid"] != sum["uuid"] {
		t.Errorf("kept message not chained after the summary")
	}

	got, err := st.Load(cwd, "c1")
	if err != nil {
		t.Fatal(err)
	}
	want := texts([]agent.Message{summary, kept})
	if strings.Join(texts(got), "|") != strings.Join(want, "|") {
		t.Fatalf("Load = %q, want %q", texts(got), want)
	}
}

// TestLoadUsesLastBoundaryAndPostCompactionAppends: two compactions → Load uses
// the last; turns appended after it reload too; the title stays the first
// prompt and the count is what a resume loads.
func TestLoadUsesLastBoundaryAndPostCompactionAppends(t *testing.T) {
	st := New(t.TempDir())
	cwd := "/work/twice"
	s, _ := st.Create(cwd, "t1")
	_ = s.AppendMessages([]agent.Message{userMsg("original title prompt"), asstMsg("r1")})
	_ = s.AppendCompaction(compaction.BoundaryMarker{Trigger: compaction.TriggerAuto, PreTokens: 10}, []agent.Message{summaryMsg("one")})
	_ = s.AppendMessages([]agent.Message{userMsg("q2"), asstMsg("r2")})
	_ = s.AppendCompaction(compaction.BoundaryMarker{Trigger: compaction.TriggerAuto, PreTokens: 20}, []agent.Message{summaryMsg("two")})
	_ = s.AppendMessages([]agent.Message{userMsg("q3"), asstMsg("r3")})

	got, err := st.Load(cwd, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !strings.Contains(messageText(got[0]), "two") || messageText(got[1]) != "q3" || messageText(got[2]) != "r3" {
		t.Fatalf("Load = %q, want [summary two, q3, r3]", texts(got))
	}
	// All 2+1+2+1+2 message lines + 2 boundaries are still on disk.
	if n := len(rawLines(t, s.Path())); n != 10 {
		t.Fatalf("file has %d lines, want 10", n)
	}

	metas, _ := st.List(cwd)
	if len(metas) != 1 || metas[0].Title != "original title prompt" || metas[0].MessageCount != 3 {
		t.Fatalf("meta = %+v", metas)
	}

	// Reopening continues the chain from the last line on disk.
	re, _ := st.Open(cwd, "t1", len(got))
	_ = re.AppendMessages([]agent.Message{userMsg("q4")})
	lines := rawLines(t, s.Path())
	if lines[len(lines)-1]["parentUuid"] != lines[len(lines)-2]["uuid"] {
		t.Errorf("reopened append not chained to the last line")
	}
	got, _ = st.Load(cwd, "t1")
	if len(got) != 4 || messageText(got[3]) != "q4" {
		t.Fatalf("after reopen Load = %q", texts(got))
	}
}

// TestLoadOldFormatAndTruncatedLine: a file written before boundaries existed
// (no parentUuid key, no boundary) loads in full; a truncated last line (crash
// mid-write) is skipped.
func TestLoadOldFormatAndTruncatedLine(t *testing.T) {
	base := t.TempDir()
	st := New(base)
	cwd := "/work/old"
	dir := st.ProjectDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"type":"user","uuid":"u1","timestamp":"2025-01-01T00:00:00Z","sessionId":"o1","message":{"role":"user","content":[{"type":"text","text":"old hello"}]},"gocodeSchema":1}
{"type":"assistant","uuid":"u2","parentUuid":"u1","timestamp":"2025-01-01T00:00:01Z","sessionId":"o1","message":{"role":"assistant","content":[{"type":"text","text":"old hi"}]},"gocodeSchema":1}
{"type":"user","uuid":"u3","parentUuid":"u2","message":{"role":"user","content":[{"type":"te`
	if err := os.WriteFile(filepath.Join(dir, "o1.jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(cwd, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || messageText(got[0]) != "old hello" || messageText(got[1]) != "old hi" {
		t.Fatalf("Load = %q, want [old hello, old hi]", texts(got))
	}
	m, ok := st.Find(cwd, "o1")
	if !ok || m.Title != "old hello" || m.MessageCount != 2 {
		t.Fatalf("meta = %+v ok=%v", m, ok)
	}
}
