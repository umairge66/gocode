package ui

import (
	"strings"
	"sync"

	"charm.land/glamour/v2"
	glansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
)

// Markdown rendering for assistant prose. Assistant messages render as
// Markdown (headings, emphasis, lists, blockquotes, and fenced code
// blocks with syntax highlighting) themed for the terminal via glamour,
// which parses Markdown with goldmark and emits ANSI, highlighting fenced
// code via chroma. We theme it for a dark or light terminal to track the UI
// palette and word-wrap it to the current width.

// mdCacheKey keys a cached glamour renderer by the parameters that change its
// output: the wrap width and whether the terminal is dark.
type mdCacheKey struct {
	width int
	dark  bool
}

var (
	mdMu    sync.Mutex
	mdCache = map[mdCacheKey]*glamour.TermRenderer{}
)

// markdownRenderer returns a glamour renderer for the given wrap width and
// dark/light theme, building and caching it on first use. Rebuilding a glamour
// renderer compiles a style, so it is cached; a nil result (construction error)
// signals the caller to fall back to raw text.
func markdownRenderer(width int, dark bool) *glamour.TermRenderer {
	if width < minMarkdownWidth {
		width = minMarkdownWidth
	}
	key := mdCacheKey{width: width, dark: dark}
	mdMu.Lock()
	defer mdMu.Unlock()
	if r, ok := mdCache[key]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(markdownStyleConfig(dark)),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		mdCache[key] = nil
		return nil
	}
	mdCache[key] = r
	return r
}

// boolPtr is a tiny helper for the bool pointers glamour's StyleConfig uses.
func boolPtr(b bool) *bool { return &b }

// markdownStyleConfig builds the glamour style we render with. It starts from
// glamour's built-in dark/light config and overrides two things that the stock
// styles get wrong for our TUI:
//
//  1. Headings. The stock H2–H6 styles inject a literal Markdown marker via
//     their StylePrimitive.Prefix ("## ", "### ", …), so a "## Heading" renders
//     with the raw "## " still visible to the user. We clear every per-level
//     Prefix (and drop H1's full-width background bar) so headings render as
//     clean bold, heading-colored text with NO "#" characters. The base
//     Heading style (bold + an accent color) still cascades into each level.
//  2. H6 stays visually subordinate (not bold) but, like the rest, carries no
//     marker.
//
// The remaining style (paragraphs, emphasis, inline code, fenced code blocks
// with chroma highlighting, lists, blockquotes, links, the 2-space document
// margin that aligns wrapped lines under the assistant gutter) is inherited
// unchanged. The full-width trailing-space padding glamour's margin writer adds
// is stripped in post-processing (see trimTrailingSpaceANSI) rather than here,
// because that padding is inherent to glamour's wrap and not a style field we
// can switch off.
func markdownStyleConfig(dark bool) glansi.StyleConfig {
	var cfg glansi.StyleConfig
	if dark {
		cfg = styles.DarkStyleConfig
	} else {
		cfg = styles.LightStyleConfig
	}
	// Clean headings: no literal "#"/"##"/… markers, no H1 background bar. Each
	// level inherits the base Heading style (bold + accent color) via glamour's
	// cascade; we only strip the per-level Prefix/Suffix/background.
	cfg.H1 = glansi.StyleBlock{}
	cfg.H2 = glansi.StyleBlock{}
	cfg.H3 = glansi.StyleBlock{}
	cfg.H4 = glansi.StyleBlock{}
	cfg.H5 = glansi.StyleBlock{}
	cfg.H6 = glansi.StyleBlock{StylePrimitive: glansi.StylePrimitive{Bold: boolPtr(false)}}
	return cfg
}

// minMarkdownWidth is the floor glamour is given so narrow terminals still wrap
// sanely instead of collapsing code blocks.
const minMarkdownWidth = 20

// --- render memoization -----------------------------------------------------
//
// renderMarkdown is the expensive step: glamour parses the whole source through
// goldmark and highlights fenced code with chroma on every call. View() now
// fires on every spinner tick, token-count update and stream delta, so without a
// cache the same (unchanged) prose would be re-highlighted many times a second —
// and, for a growing streaming buffer, re-highlighted from the top on each delta
// (O(n²)). The *renderer* was already cached by (width,dark); here we also
// memoize the render RESULT keyed by (src,width,dark) so an identical call is a
// map lookup. A spinner/token tick with no new delta therefore re-runs no
// glamour at all, and a streaming delta that does not complete a new block
// re-uses the cached completed render (see renderCompletedStreaming).
//
// The cache is bounded by an LRU so memory never grows unbounded. The
// full-screen transcript pane re-renders the WHOLE history every frame (it needs
// the full flat line list to window it), so the cap is sized to comfortably hold
// a long session's rendered items: a no-change tick is then an all-cache-hit and
// re-runs no glamour. A width or theme change produces new keys; old-width
// entries are evicted by the LRU rather than kept forever.

// mdRenderKey keys a cached render result by everything that changes its output.
type mdRenderKey struct {
	src   string
	width int
	dark  bool
}

const mdRenderCacheMax = 512

var (
	mdRenderMu    sync.Mutex
	mdRenderVals  = map[mdRenderKey][]string{}
	mdRenderOrder []mdRenderKey // LRU recency; last element = most recently used
	// mdRenderComputeCount counts ACTUAL glamour renders (cache misses). It is a
	// test seam: a memoized second call must not increment it.
	mdRenderComputeCount int
)

// mdRenderCacheReset clears the render cache. Test-only seam.
func mdRenderCacheReset() {
	mdRenderMu.Lock()
	defer mdRenderMu.Unlock()
	mdRenderVals = map[mdRenderKey][]string{}
	mdRenderOrder = nil
	mdRenderComputeCount = 0
	blockCacheReset()
}

// renderMarkdown renders src as themed, width-wrapped ANSI, memoized by
// (src,width,dark). On any failure it returns the source unchanged so prose is
// never lost. The result has its outer blank lines trimmed (glamour frames a
// document in blank lines) and is returned as a slice of display lines. The
// returned slice is shared with the cache and MUST be treated as read-only by
// callers (they copy/wrap it rather than mutating in place).
func renderMarkdown(src string, width int, dark bool) []string {
	src = strings.TrimRight(src, "\n")
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if width < minMarkdownWidth {
		width = minMarkdownWidth
	}
	key := mdRenderKey{src: src, width: width, dark: dark}

	mdRenderMu.Lock()
	if cached, ok := mdRenderVals[key]; ok {
		mdRenderTouch(key)
		mdRenderMu.Unlock()
		return cached
	}
	mdRenderMu.Unlock()

	// Render block by block (the streaming pipeline), so a finalized message
	// looks exactly like its last streamed frame and reuses its block renders.
	lines := renderMarkdownBlocks(src, width, dark)

	mdRenderMu.Lock()
	mdRenderVals[key] = lines
	mdRenderOrder = append(mdRenderOrder, key)
	mdRenderEvict()
	mdRenderMu.Unlock()
	return lines
}

// mdRenderTouch moves key to the most-recently-used end of the LRU order.
// Caller holds mdRenderMu.
func mdRenderTouch(key mdRenderKey) {
	for i, k := range mdRenderOrder {
		if k == key {
			mdRenderOrder = append(mdRenderOrder[:i], mdRenderOrder[i+1:]...)
			break
		}
	}
	mdRenderOrder = append(mdRenderOrder, key)
}

// mdRenderEvict drops least-recently-used entries past the cap. Caller holds
// mdRenderMu.
func mdRenderEvict() {
	for len(mdRenderOrder) > mdRenderCacheMax {
		old := mdRenderOrder[0]
		mdRenderOrder = mdRenderOrder[1:]
		delete(mdRenderVals, old)
	}
}

// glamourRender performs the actual glamour render of one Markdown source
// (normally one block), cleaned of glamour's padding.
func glamourRender(src string, width int, dark bool) []string {
	r := markdownRenderer(width, dark)
	if r == nil {
		return strings.Split(src, "\n")
	}
	out, err := r.Render(src)
	if err != nil {
		return strings.Split(src, "\n")
	}
	out = strings.Trim(out, "\n")
	if out == "" {
		return strings.Split(src, "\n")
	}
	lines := cleanRenderedLines(strings.Split(out, "\n"))
	// Drop the blank frame lines glamour puts around a document, so blocks
	// rendered on their own join with exactly one blank line between them.
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return strings.Split(src, "\n")
	}
	return lines
}

// cleanRenderedLines post-processes glamour's output: it trims the full-width
// trailing whitespace padding glamour's margin writer adds to every line (even
// when that padding is a run of foreground-colored SGR spaces), and collapses
// runs of now-blank padding lines down to at most one blank line. See
// trimTrailingSpaceANSI for why the trim must be ANSI-aware.
func cleanRenderedLines(lines []string) []string {
	res := make([]string, 0, len(lines))
	prevBlank := false
	for _, ln := range lines {
		ln = trimTrailingSpaceANSI(ln)
		if ln == "" {
			if prevBlank {
				continue // collapse a run of blank lines to one
			}
			prevBlank = true
		} else {
			prevBlank = false
		}
		res = append(res, ln)
	}
	return res
}

// trimTrailingSpaceANSI removes trailing whitespace from a rendered line in an
// ANSI-aware way. glamour pads every line out to the wrap width with spaces that
// carry the block's foreground SGR (e.g. "\x1b[38;5;252m \x1b[0m" repeated),
// so a plain strings.TrimRight would leave the color-wrapped spaces behind.
//
// We first check the escape-stripped text (via xansi.Strip) for trailing
// whitespace; lines with none are returned untouched. Otherwise we walk the raw
// bytes, skipping over ANSI escape sequences, and remember the offset just past
// the last visible non-space glyph. Everything after that offset — the trailing
// spaces AND the SGR runs that only ever colored them — is dropped, and a single
// reset is appended so the retained styling stays terminated. This leaves no
// trailing color-space run and no bloat of empty SGR pairs. A whitespace-only
// line collapses to "".
func trimTrailingSpaceANSI(line string) string {
	visible := xansi.Strip(line)
	trimmed := strings.TrimRight(visible, " \t")
	if trimmed == visible {
		return line // no trailing whitespace to remove
	}
	if trimmed == "" {
		return ""
	}

	const esc = 0x1b
	b := []byte(line)
	i := 0
	lastContent := 0 // byte offset just past the last visible non-space glyph
	sawStyle := false
	for i < len(b) {
		if b[i] == esc {
			sawStyle = true
			j := i + 1
			if j < len(b) && b[j] == '[' { // CSI sequence: ESC [ ... final
				j++
				for j < len(b) && !(b[j] >= 0x40 && b[j] <= 0x7e) {
					j++
				}
				if j < len(b) {
					j++ // include the final byte
				}
			} else if j < len(b) {
				j++ // other escape: consume the following byte
			}
			i = j
			continue
		}
		// A non-escape byte. Spaces/tabs are single-byte ASCII; any other byte
		// (including a UTF-8 continuation byte) counts as visible content, so a
		// multi-byte glyph correctly advances lastContent to its final byte.
		if b[i] != ' ' && b[i] != '\t' {
			lastContent = i + 1
		}
		i++
	}
	out := string(b[:lastContent])
	if sawStyle {
		out += "\x1b[0m"
	}
	return out
}
