package sessionstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/cost"
)

// plainFS hides osFS's ReadAt capability, forcing the ReadFile fallback.
type plainFS struct {
	FS
}

func TestScanLinesBackward(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	var lines []string
	for i := 0; i < 2000; i++ {
		// Lines of varying length so they straddle the block boundaries.
		lines = append(lines, fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("p", (i*37)%900)))
	}
	big := `{"n":"big","pad":"` + strings.Repeat("b", 3*tailBlock) + `"}`
	for _, c := range []struct {
		name, content string
		want          []string
	}{
		{"many lines, trailing newline", strings.Join(lines, "\n") + "\n", lines},
		{"no trailing newline, CRLF, blanks", "a\r\n\r\nb\n\n c \n", []string{"a", "b", "c"}},
		{"one line longer than a block", "first\n" + big + "\nlast\n", []string{"first", big, "last"}},
		{"single line, no newline", "only", []string{"only"}},
		{"empty", "", nil},
	} {
		if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, st := range []*Store{{base: dir, fs: osFS{}}, {base: dir, fs: plainFS{osFS{}}}} {
			var got []string
			st.scanLinesBackward(path, func(l []byte) bool { got = append(got, string(l)); return false })
			if len(got) != len(c.want) {
				t.Fatalf("%s (%T): %d lines, want %d", c.name, st.fs, len(got), len(c.want))
			}
			for i := range got {
				if got[i] != c.want[len(c.want)-1-i] {
					t.Fatalf("%s (%T): line %d = %.40q, want %.40q", c.name, st.fs, i, got[i], c.want[len(c.want)-1-i])
				}
			}
		}
	}
	// Stopping early returns true and reads no further.
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	n := 0
	if !(&Store{fs: osFS{}}).scanLinesBackward(path, func([]byte) bool { n++; return n == 3 }) || n != 3 {
		t.Fatalf("early stop: n=%d", n)
	}
}

// A transcript line over the old 16 MiB scanner cap no longer hides the lines
// after it.
func TestForEachEntryHasNoLineLimit(t *testing.T) {
	huge := `{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("x", 17<<20) + `"}]}}`
	data := []byte(huge + "\n" + `{"type":"assistant","uuid":"u2"}` + "\n\n" + `not json` + "\n" + `{"type":"user","uuid":"u3"}`)
	var uuids []string
	forEachEntry(data, func(e entry) { uuids = append(uuids, e.UUID) })
	if strings.Join(uuids, ",") != "u1,u2,u3" {
		t.Fatalf("uuids = %v", uuids)
	}
}

// Resume-time reads: the last chain uuid and the latest cost totals come from
// the end of the file; an exact id is found without listing the project.
func TestResumeReadsFromTheEnd(t *testing.T) {
	st := New(t.TempDir())
	const cwd = "/proj"
	sess, err := st.Create(cwd, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := sess.AppendMessages([]agent.Message{userMsg(fmt.Sprintf("q%d", i)), asstMsg("a")}); err != nil {
			t.Fatal(err)
		}
		if i == 10 || i == 40 {
			if err := sess.AppendCostState(cost.State{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, _ := os.ReadFile(st.sessionPath(cwd, "s-1"))
	var want string
	forEachEntry(data, func(e entry) {
		if e.UUID != "" {
			want = e.UUID
		}
	})
	if got := st.lastChainUUIDAt(st.sessionPath(cwd, "s-1")); got == "" || got != want {
		t.Fatalf("last uuid = %q, want %q", got, want)
	}
	if _, ok := st.LoadCostState(cwd, "s-1"); !ok {
		t.Fatal("cost state not found")
	}
	if _, ok := st.LoadCostState(cwd, "missing"); ok {
		t.Fatal("cost state for a missing session")
	}

	// Find by exact id: a second, unreadable session must not matter.
	other := st.sessionPath(cwd, "s-2")
	_ = os.WriteFile(other, []byte("{}\n"), 0o000)
	if m, ok := st.Find(cwd, "s-1"); !ok || m.ID != "s-1" || m.MessageCount == 0 {
		t.Fatalf("Find exact = %+v %v", m, ok)
	}
	for _, bad := range []string{"../s-1", "s-1/..", ".."} {
		if m, ok := st.Find(cwd, bad); ok && m.ID != "s-1" {
			t.Fatalf("Find(%q) = %+v", bad, m)
		}
	}
	// The fast path keeps prefix lookup working.
	if m, ok := st.Find(cwd, "s-"); ok && m.ID != "s-1" && m.ID != "s-2" {
		t.Fatalf("prefix = %+v", m)
	}
}

// The lazy parse yields the same active chain and messages as the full one.
func TestParseEntriesLazyMatchesFull(t *testing.T) {
	st := New(t.TempDir())
	sess, err := st.Create("/p", "s")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := sess.AppendMessages([]agent.Message{userMsg(fmt.Sprintf("q%d", i)), asstMsg(fmt.Sprintf("a%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(st.sessionPath("/p", "s"))
	full, _ := activeEntries(parseEntries(data), "")
	lazy, _ := activeEntries(parseEntriesLazy(data), "")
	fm, lm := chainMessages(full), chainMessages(lazy)
	if len(fm) != 60 || len(fm) != len(lm) {
		t.Fatalf("full %d, lazy %d messages", len(fm), len(lm))
	}
	for i := range fm {
		if messageText(fm[i]) != messageText(lm[i]) || fm[i].Role != lm[i].Role {
			t.Fatalf("message %d differs: %q vs %q", i, messageText(fm[i]), messageText(lm[i]))
		}
	}
	if lazy[0].line == nil && lazy[0].Message == nil {
		t.Fatal("lazy entry kept neither line nor message")
	}
}
