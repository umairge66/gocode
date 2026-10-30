package toolrun

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutLongLinesLeavesNormalOutput(t *testing.T) {
	in := "a\nb\n" + strings.Repeat("x", longLineLimit) + "\n"
	if out, cut := cutLongLines(in); cut || out != in {
		t.Fatalf("cut=%v, output changed", cut)
	}
}

func TestCutLongLinesShortensOnlyLongLines(t *testing.T) {
	long := strings.Repeat("é", longLineLimit+100) // multi-byte: cut on characters, not bytes
	in := "first\n" + long + "\nlast"
	out, cut := cutLongLines(in)
	if !cut {
		t.Fatal("long line not cut")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[0] != "first" || lines[2] != "last" {
		t.Fatalf("lines = %q", lines)
	}
	want := strings.Repeat("é", longLineKeep) + " … [line cut: 1600 more characters]"
	if lines[1] != want {
		t.Fatalf("cut line = %q…", lines[1][:40])
	}
	if !utf8.ValidString(out) {
		t.Fatal("cut produced invalid UTF-8")
	}
}

func TestCapShellTextSavesFullOutputWhenCutting(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("m", 50_000)
	in := "match: " + long + "\nok\n"
	out, framed := capShellText(dir, "toolu_1", in, BashMaxResultSizeChars)
	if framed {
		t.Fatalf("short result should stay inline: %q", out[:80])
	}
	if !strings.Contains(out, "[line cut: 49507 more characters]") || !strings.Contains(out, "\nok\n") {
		t.Fatalf("out = %q", out)
	}
	i := strings.Index(out, "Full output saved to: ")
	if i < 0 {
		t.Fatalf("no saved path in %q", out)
	}
	path := strings.TrimSuffix(out[i+len("Full output saved to: "):], "]")
	saved, err := os.ReadFile(path)
	if err != nil || string(saved) != in {
		t.Fatalf("saved file: err=%v len=%d", err, len(saved))
	}
}

func TestCapShellTextManyLongLinesStillPersisted(t *testing.T) {
	dir := t.TempDir()
	in := strings.Repeat(strings.Repeat("z", 3000)+"\n", 100) // 100 cut lines ≈ 54k chars
	out, framed := capShellText(dir, "toolu_2", in, BashMaxResultSizeChars)
	if !framed || !strings.Contains(out, persistedOutputOpen) {
		t.Fatalf("expected a saved-output block, got framed=%v", framed)
	}
	if utf8.RuneCountInString(out) > BashMaxResultSizeChars {
		t.Fatalf("inline text exceeds the limit: %d", utf8.RuneCountInString(out))
	}
}

func TestCapShellTextUnchangedWithoutLongLines(t *testing.T) {
	in := "hello\nworld\n"
	if out, framed := capShellText(t.TempDir(), "toolu_3", in, BashMaxResultSizeChars); framed || out != in {
		t.Fatalf("out = %q framed=%v", out, framed)
	}
}
