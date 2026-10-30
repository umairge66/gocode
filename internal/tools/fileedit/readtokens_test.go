package fileedit

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// linesOf writes n lines of width characters each.
func linesOf(n, width int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		s := fmt.Sprintf("%d:", i)
		b.WriteString(s + strings.Repeat("a", max(0, width-len(s))) + "\n")
	}
	return b.String()
}

func TestReadTokenBudgetPagesWholeFileRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	// 1000 lines × 120 chars ≈ 30000 tokens at 4 chars/token.
	writeFile(t, p, linesOf(1000, 120))
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.NumLines >= 1000 || res.NumLines < 600 {
		t.Fatalf("expected a first page of 600-999 lines, got %d (truncated=%v)", res.NumLines, res.Truncated)
	}
	if got := estimateReadTokens(res.Content, "txt"); got > DefaultMaxReadTokens {
		t.Fatalf("page is %d tokens, over the %d budget", got, DefaultMaxReadTokens)
	}
	want := fmt.Sprintf("%s%s: showing lines 1-%d of 1001 total (", partialViewPrefix, p, res.NumLines)
	if !strings.HasPrefix(res.Text, want) {
		t.Fatalf("banner:\n%q\nwant prefix\n%q", res.Text[:300], want)
	}
	if !strings.Contains(res.Text, fmt.Sprintf("tokens, cap %d). Call Read with offset=%d limit=%d for the next page, or Grep to find a specific section. Do NOT answer from this page alone if the answer may be further in the file.]\n1\t0:", DefaultMaxReadTokens, res.NumLines+1, res.NumLines)) {
		t.Fatalf("banner tail: %q", res.Text[:400])
	}
}

func TestReadTokenBudgetRefusesNarrowedRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	writeFile(t, p, linesOf(1000, 120))
	for _, in := range []ReadInput{
		{FilePath: p, Offset: intp(2)},
		{FilePath: p, Limit: intp(1000)},
	} {
		_, err := ReadTool{}.Execute(ctx, in)
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed tokens (25000). Use offset and limit parameters") {
			t.Fatalf("%+v: got %v", in, err)
		}
	}
	// A narrow enough range is fine; offset 1 counts as a whole-file read.
	if _, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p, Offset: intp(10), Limit: intp(100)}); err != nil {
		t.Fatal(err)
	}
	if res, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p, Offset: intp(1)}); err != nil || !res.Truncated {
		t.Fatalf("offset=1: %v %+v", err, res.NumLines)
	}
}

func TestReadTokenBudgetJSONCountsDenser(t *testing.T) {
	dir := t.TempDir()
	body := linesOf(500, 120) // 60000 chars: 15000 tokens as text, 30000 as JSON
	txt, js := filepath.Join(dir, "a.txt"), filepath.Join(dir, "a.json")
	writeFile(t, txt, body)
	writeFile(t, js, body)
	if res, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: txt}); err != nil || res.Truncated {
		t.Fatalf("text read paged: %v %v", err, res.Truncated)
	}
	if res, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: js}); err != nil || !res.Truncated {
		t.Fatalf("json read not paged: %v %v", err, res.Truncated)
	}
}

func TestReadTokenBudgetLongLinesCutByCharacters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "one.txt")
	writeFile(t, p, strings.Repeat("q", 200000)) // one 50000-token line
	res, err := ReadTool{MaxLineChars: -1}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, partialViewPrefix+p+": showing the first ") ||
		!strings.Contains(res.Text, "of 200000 characters (50000 tokens, cap 25000); this file has very long lines and cannot be paginated by line.") {
		t.Fatalf("banner: %q", res.Text[:300])
	}
	if got := estimateReadTokens(res.Content, "txt"); got > DefaultMaxReadTokens || got < DefaultMaxReadTokens/2 {
		t.Fatalf("excerpt is %d tokens", got)
	}
}

func TestReadTokenBudgetOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.txt")
	writeFile(t, p, linesOf(400, 100)) // 10000 tokens
	if res, _ := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p}); res.Truncated {
		t.Fatal("under the default budget but paged")
	}
	if res, _ := (ReadTool{MaxTokens: 1000}).Execute(ctx, ReadInput{FilePath: p}); !res.Truncated {
		t.Fatal("MaxTokens not applied")
	}
	t.Setenv(maxReadTokensEnv, "1000")
	if res, _ := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p}); !res.Truncated {
		t.Fatal("environment budget not applied")
	}
	if res, err := (ReadTool{}).Execute(ctx, ReadInput{FilePath: p, AllowLarge: true}); err != nil || res.Truncated || res.NumLines != 401 {
		t.Fatalf("allow_large still paged: %v %d", err, res.NumLines)
	}
}
