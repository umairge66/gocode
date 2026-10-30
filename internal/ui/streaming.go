package ui

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// Live Markdown for a streaming assistant reply.
//
// The reply is split into top-level Markdown BLOCKS (paragraphs, headings,
// lists, fenced code, tables, …) at blank lines that lie outside any fenced
// code block. Every block but the last is complete: it is rendered once with
// glamour and cached. The last block (the TAIL) is still growing; it is
// re-rendered on each delta — but only it, and at most once per
// tailThrottle — after a few cheap fix-ups that make a half-written construct
// render the way it will look once finished:
//
//   - an open code fence (``` or ~~~, any length ≥ 3, so a ````md fence that
//     wraps ``` lines nests correctly) is closed temporarily, so the code shows
//     highlighted at once instead of as a literal "```php …" line;
//   - a dangling inline backtick or "**" is closed;
//   - a lone "-"/"=" under a paragraph (a list marker or rule in progress, which
//     CommonMark would read as a setext heading underline) and a partial fence
//     opener ("`", "``") are held back until they mean something;
//   - a table whose header separator row has not arrived yet is shown as plain
//     text rows instead of being folded into a paragraph.
//
// The finalized message renders through exactly the same block pipeline
// (renderMarkdown → renderMarkdownBlocks), so the frame after Done equals the
// last streamed frame: nothing jumps when the stream ends.

// tailThrottle bounds how often the in-progress tail block is re-rendered. A
// delta that arrives sooner reuses the previous tail render (it is at most one
// throttle interval stale; the spinner tick redraws soon after). Tests set it to
// 0 for deterministic output.
var tailThrottle = 50 * time.Millisecond

// mdFence is an open fenced code block: its fence character and run length.
type mdFence struct {
	ch byte
	n  int
}

// fenceOpenRE matches a fenced-code opener: optional indentation, then a run of
// ≥3 backticks or tildes, then the info string.
var fenceOpenRE = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})(.*)$")

// parseFenceOpen reports whether line opens a fenced code block. A backtick
// fence's info string may not contain a backtick (CommonMark), which keeps a
// line like "```inline``` code" from opening a fence.
func parseFenceOpen(line string) (mdFence, bool) {
	m := fenceOpenRE.FindStringSubmatch(line)
	if m == nil {
		return mdFence{}, false
	}
	run := m[1]
	if run[0] == '`' && strings.Contains(m[2], "`") {
		return mdFence{}, false
	}
	return mdFence{ch: run[0], n: len(run)}, true
}

// isFenceClose reports whether line closes the open fence f: a run of the same
// character at least as long as the opener, followed only by whitespace.
func isFenceClose(line string, f mdFence) bool {
	t := strings.TrimLeft(line, " \t")
	i := 0
	for i < len(t) && t[i] == f.ch {
		i++
	}
	return i >= f.n && strings.TrimSpace(t[i:]) == ""
}

// listItemRE matches a list item marker at the start of a line (bullet or
// ordered), with at most three columns of indentation.
var listItemRE = regexp.MustCompile(`^ {0,3}([-*+]|\d{1,9}[.)])([ \t]|$)`)

// isListItem reports whether line starts a top-level list item.
func isListItem(line string) bool { return listItemRE.MatchString(line) }

// startsIndented reports whether line begins with whitespace, i.e. it continues
// a list item or is indented code — it never starts an independent block.
func startsIndented(line string) bool {
	return line != "" && (line[0] == ' ' || line[0] == '\t')
}

// mdBlocks is a streaming buffer split into blocks: the complete blocks, the
// trailing in-progress block, and the fence still open at the end of the tail
// (nil when none).
type mdBlocks struct {
	done []string
	tail string
	open *mdFence
}

// splitMarkdownBlocks splits Markdown into top-level blocks at blank lines
// outside fenced code. A boundary is taken only where the next block can stand
// on its own: not before an indented line (a list item's continuation, indented
// code) and not between two items of the same list (which would restart its
// numbering). A block is complete once the first line of the following block
// has been seen, so the last block is always the tail.
func splitMarkdownBlocks(text string) mdBlocks {
	var (
		out     mdBlocks
		cur     []string
		blanks  int
		inList  bool
		fence   *mdFence
		started bool
	)
	for _, ln := range strings.Split(text, "\n") {
		if fence != nil {
			for ; blanks > 0; blanks-- {
				cur = append(cur, "")
			}
			cur = append(cur, ln)
			if isFenceClose(ln, *fence) {
				fence = nil
			}
			continue
		}
		if strings.TrimSpace(ln) == "" {
			if started {
				blanks++
			}
			continue
		}
		if blanks > 0 {
			split := !startsIndented(ln) && !(inList && isListItem(ln))
			if split {
				out.done = append(out.done, strings.Join(cur, "\n"))
				cur, inList = nil, false
			} else {
				for ; blanks > 0; blanks-- {
					cur = append(cur, "")
				}
			}
			blanks = 0
		}
		started = true
		cur = append(cur, ln)
		if f, ok := parseFenceOpen(ln); ok {
			fence = &f
		}
		if isListItem(ln) {
			inList = true
		}
	}
	out.tail = strings.Join(cur, "\n")
	out.open = fence
	return out
}

// renderMarkdownBlocks renders src block by block (each block through the
// block cache) and joins the blocks with one blank line, which is how glamour
// spaces top-level blocks itself.
func renderMarkdownBlocks(src string, width int, dark bool) []string {
	sp := splitMarkdownBlocks(src)
	blocks := sp.done
	if strings.TrimSpace(sp.tail) != "" {
		blocks = append(blocks, sp.tail)
	}
	return joinBlockRenders(blocks, width, dark, nil)
}

// joinBlockRenders renders each block (cached) and joins them with a blank
// separator line, appending tail (already-rendered lines) last.
func joinBlockRenders(blocks []string, width int, dark bool, tail []string) []string {
	var out []string
	for _, b := range blocks {
		lines := renderBlock(b, width, dark)
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	if len(tail) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, tail...)
	}
	return out
}

// --- block cache -------------------------------------------------------------
//
// A complete block's render never changes, so it is cached by (src, width,
// dark). Streaming fills it block by block; the finalized message then reuses
// those very renders. It is bounded and simply reset when full.

const blockCacheMax = 4096

var (
	blockMu    sync.Mutex
	blockCache = map[mdRenderKey][]string{}
)

// renderBlock renders one complete block, memoized. The result is shared with
// the cache and must be treated as read-only.
func renderBlock(src string, width int, dark bool) []string {
	src = strings.TrimRight(src, "\n")
	if strings.TrimSpace(src) == "" {
		return nil
	}
	key := mdRenderKey{src: src, width: width, dark: dark}
	blockMu.Lock()
	if v, ok := blockCache[key]; ok {
		blockMu.Unlock()
		return v
	}
	blockMu.Unlock()

	mdRenderMu.Lock()
	mdRenderComputeCount++
	mdRenderMu.Unlock()
	lines := glamourRender(src, width, dark)

	blockMu.Lock()
	if len(blockCache) >= blockCacheMax {
		blockCache = map[mdRenderKey][]string{}
	}
	blockCache[key] = lines
	blockMu.Unlock()
	return lines
}

// blockCacheReset clears the block cache (used by the render-cache reset seam).
func blockCacheReset() {
	blockMu.Lock()
	blockCache = map[mdRenderKey][]string{}
	blockMu.Unlock()
}

// --- the streaming render ----------------------------------------------------

// streamState is the single in-flight stream's render state: the complete
// blocks rendered so far and the last tail render (for the throttle). Only one
// assistant message streams at a time, so one entry suffices; any width,
// theme or non-append change simply rebuilds it.
type streamState struct {
	width int
	dark  bool

	blocks []string // complete blocks seen so far
	lines  []string // their joined render

	tailSrc   string
	tailLines []string
	tailAt    time.Time
}

var (
	streamMu sync.Mutex
	stream   streamState
)

// streamCacheReset clears the streaming render state so a test starts clean.
func streamCacheReset() {
	streamMu.Lock()
	stream = streamState{}
	streamMu.Unlock()
}

// renderStreamingMarkdown renders an in-progress reply: the complete blocks
// from cache (rendering only newly completed ones) and the tail live, after the
// partial-construct fix-ups. width is the Markdown wrap width.
func renderStreamingMarkdown(text string, width int, dark bool) []string {
	if width < minMarkdownWidth {
		width = minMarkdownWidth
	}
	sp := splitMarkdownBlocks(text)

	streamMu.Lock()
	defer streamMu.Unlock()
	st := &stream
	if st.width != width || st.dark != dark || !blocksExtend(sp.done, st.blocks) {
		*st = streamState{width: width, dark: dark}
	}
	if len(sp.done) > len(st.blocks) {
		next := make([]string, len(st.lines), len(st.lines)+16)
		copy(next, st.lines)
		for _, b := range sp.done[len(st.blocks):] {
			lines := renderBlock(b, width, dark)
			if len(lines) == 0 {
				continue
			}
			if len(next) > 0 {
				next = append(next, "")
			}
			next = append(next, lines...)
		}
		st.blocks = append([]string(nil), sp.done...)
		st.lines = next
	}

	tail := st.renderTail(sp.tail, sp.open, width, dark)
	if len(tail) == 0 {
		return st.lines
	}
	out := make([]string, 0, len(st.lines)+len(tail)+1)
	out = append(out, st.lines...)
	if len(out) > 0 {
		out = append(out, "")
	}
	return append(out, tail...)
}

// blocksExtend reports whether next starts with every block of prev (the
// stream only appended).
func blocksExtend(next, prev []string) bool {
	if len(next) < len(prev) {
		return false
	}
	for i := range prev {
		if next[i] != prev[i] {
			return false
		}
	}
	return true
}

// renderTail renders the in-progress block, reusing the previous render when a
// new delta arrives within tailThrottle of it.
func (st *streamState) renderTail(tail string, open *mdFence, width int, dark bool) []string {
	if strings.TrimSpace(tail) == "" {
		st.tailSrc, st.tailLines = "", nil
		return nil
	}
	if tail == st.tailSrc {
		return st.tailLines
	}
	if st.tailSrc != "" && tailThrottle > 0 && strings.HasPrefix(tail, st.tailSrc) &&
		time.Since(st.tailAt) < tailThrottle {
		return st.tailLines
	}
	st.tailSrc = tail
	st.tailLines = renderTailBlock(tail, open, width, dark)
	st.tailAt = time.Now()
	return st.tailLines
}

// renderTailBlock renders the trailing in-progress block after the fix-ups
// described at the top of this file. A complete block needs no fix-up and
// renders exactly like renderBlock (so the end of a stream matches the
// finalized render), but the tail is never cached: it changes every delta.
func renderTailBlock(tail string, open *mdFence, width int, dark bool) []string {
	if out, ok := renderOpenFence(tail, open, width, dark); ok {
		return out
	}
	md, rawRows := prepareTail(tail, open)
	if strings.TrimSpace(md) == "" && len(rawRows) == 0 {
		return nil
	}
	var out []string
	if strings.TrimSpace(md) != "" {
		key := mdRenderKey{src: strings.TrimRight(md, "\n"), width: width, dark: dark}
		blockMu.Lock()
		cached, ok := blockCache[key]
		blockMu.Unlock()
		if ok {
			out = append(out, cached...)
		} else {
			out = append(out, glamourRender(md, width, dark)...)
		}
	}
	if len(rawRows) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		for _, r := range rawRows {
			for _, w := range wrapCells(r, max(width-2, 1)) {
				out = append(out, "  "+w)
			}
		}
	}
	return out
}

// fenceChunk is how many lines of a growing code block are highlighted
// together. A long block being streamed is rendered in chunks: each full chunk
// once (cached), only the last, growing chunk per delta — so the cost of a
// delta stays flat however long the block gets. Highlighting restarts at each
// chunk boundary while the block is open; once its closing fence arrives it
// renders whole.
const fenceChunk = 24

// renderOpenFence renders a tail that is an open code block (the fence on its
// first line) in chunks. ok is false for any other tail.
func renderOpenFence(tail string, open *mdFence, width int, dark bool) ([]string, bool) {
	if open == nil {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if f, isOpener := parseFenceOpen(lines[0]); !isOpener || f != *open || len(lines) <= fenceChunk+1 {
		return nil, false
	}
	opener, code := lines[0], lines[1:]
	if last := strings.TrimSpace(code[len(code)-1]); last != "" && strings.Trim(last, string(open.ch)) == "" {
		code = code[:len(code)-1] // a closing run still being typed
	}
	closer := strings.Repeat(string(open.ch), open.n)
	full := (len(code) - 1) / fenceChunk // keep at least one line in the live chunk
	var out []string
	for c := 0; c < full; c++ {
		src := opener + "\n" + strings.Join(code[c*fenceChunk:(c+1)*fenceChunk], "\n") + "\n" + closer
		out = append(out, renderBlock(src, width, dark)...)
	}
	live := opener + "\n" + strings.Join(code[full*fenceChunk:], "\n") + "\n" + closer
	return append(out, glamourRender(live, width, dark)...), true
}

// tableDelimRE matches a table's header separator row (| --- | :-: |).
var tableDelimRE = regexp.MustCompile(`^[ \t]*\|?[ \t]*:?-{1,}:?[ \t]*(\|[ \t]*:?-{1,}:?[ \t]*)*\|?[ \t]*$`)

// tableCells counts the cells of a table row ("| a | b |" has two).
func tableCells(row string) int {
	t := strings.TrimSpace(row)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	return strings.Count(t, "|") + 1
}

// partialFenceRE matches a line that may still grow into a fence opener.
var partialFenceRE = regexp.MustCompile("^[ \t]*(`{1,2}|~{1,2})$")

// prepareTail applies the partial-construct fix-ups to the tail block. It
// returns the Markdown to render and any rows to show as plain text (a table
// still waiting for its separator row).
func prepareTail(tail string, open *mdFence) (md string, raw []string) {
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if open != nil {
		// Inside an open fence: drop a closing run still being typed, then close
		// the fence so the code renders highlighted right away.
		if n := len(lines); n > 1 {
			last := strings.TrimSpace(lines[n-1])
			if last != "" && strings.Trim(last, string(open.ch)) == "" {
				lines = lines[:n-1]
			}
		}
		return strings.Join(lines, "\n") + "\n" + strings.Repeat(string(open.ch), open.n), nil
	}

	// Hold back a last line that is still ambiguous.
	if n := len(lines); n > 0 {
		last := strings.TrimSpace(lines[n-1])
		switch {
		case partialFenceRE.MatchString(lines[n-1]):
			lines = lines[:n-1]
		case n > 1 && (last == "-" || last == "=") && strings.TrimSpace(lines[n-2]) != "":
			lines = lines[:n-1]
		}
	}

	// A table whose separator row has not arrived (or is still being typed):
	// show its rows as text rather than folding them into one paragraph.
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "|") {
			continue
		}
		if i+1 < len(lines) && tableDelimRE.MatchString(lines[i+1]) &&
			tableCells(lines[i+1]) == tableCells(ln) {
			break // a real table: glamour renders it (partial rows included)
		}
		raw = append(raw, lines[i:]...)
		lines = lines[:i]
		break
	}

	md = strings.Join(lines, "\n")
	if strings.TrimSpace(md) == "" {
		return "", raw
	}
	// Close a dangling inline code span or strong emphasis.
	if strings.Count(md, "`")%2 == 1 {
		md += "`"
	}
	if strings.Count(md, "**")%2 == 1 {
		md += "**"
	}
	return md, raw
}
