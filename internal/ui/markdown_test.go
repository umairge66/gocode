package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
)

// mdSample exercises headings (H2/H3), a paragraph, inline code, and a fenced
// code block — the shapes the renderer must get right.
const mdSample = "I'll show you.\n\n" +
	"## 1. Create Middleware\n\n" +
	"Use the `make:middleware` command:\n\n" +
	"```php\nphp artisan make:middleware X\n```\n\n" +
	"### In `bootstrap/app.php`:\n\n" +
	"Done.\n"

// TestMarkdownHeadingsHaveNoLiteralMarkers checks that rendered heading lines
// never contain a literal '#': the stock glamour H2–H6 styles inject "## " /
// "### " via StylePrimitive.Prefix, which our custom style config clears.
func TestMarkdownHeadingsHaveNoLiteralMarkers(t *testing.T) {
	mdRenderCacheReset()
	for _, dark := range []bool{true, false} {
		lines := renderMarkdown(mdSample, 80, dark)
		var gotCreate, gotBootstrap bool
		for _, ln := range lines {
			vis := xansi.Strip(ln)
			if strings.Contains(vis, "#") {
				t.Fatalf("dark=%v: rendered heading line still has a literal '#': %q", dark, vis)
			}
			if strings.Contains(vis, "1. Create Middleware") {
				gotCreate = true
			}
			if strings.Contains(vis, "In") && strings.Contains(vis, "bootstrap/app.php") {
				gotBootstrap = true
			}
		}
		if !gotCreate {
			t.Fatalf("dark=%v: expected a clean '1. Create Middleware' heading line", dark)
		}
		if !gotBootstrap {
			t.Fatalf("dark=%v: expected a clean 'In bootstrap/app.php' heading line", dark)
		}
	}
}

// TestMarkdownNoTrailingWhitespacePadding checks that no rendered line ends in
// whitespace once ANSI escapes are stripped: glamour pads every line to the
// wrap width with (color-coded) spaces, and the renderer must trim that in an
// ANSI-aware way.
func TestMarkdownNoTrailingWhitespacePadding(t *testing.T) {
	mdRenderCacheReset()
	for _, dark := range []bool{true, false} {
		lines := renderMarkdown(mdSample, 80, dark)
		for i, ln := range lines {
			vis := xansi.Strip(ln)
			if vis != strings.TrimRight(vis, " \t") {
				t.Fatalf("dark=%v line %d has trailing whitespace padding: %q (raw %q)", dark, i, vis, ln)
			}
			// The ANSI-stripped content must never run to the full wrap width —
			// that would mean the full-width padding survived.
			if len(vis) >= 80 {
				t.Fatalf("dark=%v line %d is padded to full width: %q", dark, i, vis)
			}
		}
	}
}

// TestMarkdownInlineCodeAndFenceRender checks that inline code and a fenced code
// block still render their content (distinguished/styled), so stripping markers
// and padding did not drop body text.
func TestMarkdownInlineCodeAndFenceRender(t *testing.T) {
	mdRenderCacheReset()
	for _, dark := range []bool{true, false} {
		lines := renderMarkdown(mdSample, 80, dark)
		joinedVis := strings.Join(mapStrip(lines), "\n")
		if !strings.Contains(joinedVis, "make:middleware") {
			t.Fatalf("dark=%v: inline code 'make:middleware' missing from render", dark)
		}
		// The fenced block's command line must be present (indented, highlighted).
		if !strings.Contains(joinedVis, "php artisan make:middleware X") {
			t.Fatalf("dark=%v: fenced code block content missing from render", dark)
		}
		// The inline-code span must carry a background SGR (code is distinguished
		// from plain prose), and the fenced block must carry styling too.
		var inlineStyled bool
		for _, ln := range lines {
			if strings.Contains(xansi.Strip(ln), "make:middleware") &&
				strings.Contains(xansi.Strip(ln), "command") &&
				strings.Contains(ln, "\x1b[") {
				inlineStyled = true
			}
		}
		if !inlineStyled {
			t.Fatalf("dark=%v: inline code line is not ANSI-styled", dark)
		}
	}
}

// TestMarkdownBlankRunsCollapsed checks that the renderer never emits two
// consecutive blank lines (heading/padding blanks are collapsed to at most one).
func TestMarkdownBlankRunsCollapsed(t *testing.T) {
	mdRenderCacheReset()
	lines := renderMarkdown(mdSample, 80, true)
	for i := 1; i < len(lines); i++ {
		if xansi.Strip(lines[i]) == "" && xansi.Strip(lines[i-1]) == "" {
			t.Fatalf("two consecutive blank lines at %d (run not collapsed)", i)
		}
	}
}

// TestMarkdownListsRender checks ordered/unordered lists still render their
// items and bullets.
func TestMarkdownListsRender(t *testing.T) {
	mdRenderCacheReset()
	src := "Items:\n\n- alpha\n- beta\n\n1. first\n2. second\n"
	lines := renderMarkdown(src, 80, true)
	vis := strings.Join(mapStrip(lines), "\n")
	for _, want := range []string{"alpha", "beta", "first", "second"} {
		if !strings.Contains(vis, want) {
			t.Fatalf("list item %q missing from render:\n%s", want, vis)
		}
	}
}

func mapStrip(lines []string) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = xansi.Strip(ln)
	}
	return out
}
