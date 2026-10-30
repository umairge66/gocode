package ui

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/textutil"
)

// Renderer turns a ViewState into output lines.
// PlainRenderer is a dependency-free text renderer (the non-TTY fallback);
// LipglossRenderer (render_lipgloss.go) is the styled implementation the
// Bubble Tea Model (model.go) uses.
type Renderer interface {
	// Render returns the whole screen as a slice of display lines (the welcome
	// banner, the transcript, then the pinned bottom region). It is the sequential
	// render used by the non-TTY / PlainRenderer path; the full-screen Model.View
	// composes RenderTranscript + RenderBottom itself so it can WINDOW the
	// transcript into a scrollable pane above a pinned bottom. Render is kept as
	// RenderTranscript(vs) ++ RenderBottom(vs) so both paths agree.
	Render(vs ViewState) []string

	// RenderTranscript renders the SCROLLABLE region as a flat slice of display
	// lines: the welcome banner (the first transcript item) followed by every
	// transcript message, in order, with the renderer's inter-message spacing. The
	// full-screen Model.View windows this slice to the pane height at the current
	// scroll offset. The whole history is rendered every frame (reusing the
	// Markdown memoization + streaming completed-block cache, so an unchanged item
	// is a cache hit and a no-change tick re-renders no Markdown).
	RenderTranscript(vs ViewState) []string

	// RenderBottom renders the PINNED bottom region as display lines: an open
	// modal dialog/picker/secret input if one is present (it takes over the
	// region), otherwise the composer overlays (the "/" command menu and "@" file
	// menu), the composer input box, the active-agents panel, the spinner/status
	// line, the footer token readout, and the host status line. These are the
	// lines the full-screen layout keeps fixed at the bottom of the terminal.
	RenderBottom(vs ViewState) []string
}

// PlainRenderer is a minimal, ANSI-free Renderer: transcript on top, a
// separator, the composer, the status/spinner line, then the footer. It is
// enough to exercise and snapshot the view-state model in tests.
type PlainRenderer struct {
	// Prompt is the glyph drawn before the composer buffer. Defaults to ">".
	Prompt string
}

// Render implements Renderer: the full sequential screen (transcript then the
// pinned bottom region). Kept equal to RenderTranscript ++ RenderBottom.
func (pr PlainRenderer) Render(vs ViewState) []string {
	out := pr.RenderTranscript(vs)
	return append(out, pr.RenderBottom(vs)...)
}

// RenderTranscript implements Renderer: the welcome banner followed by every
// transcript message, as ANSI-free display lines.
func (pr PlainRenderer) RenderTranscript(vs ViewState) []string {
	width := vs.Width
	if width <= 0 {
		width = 80
	}
	var out []string

	// Welcome banner: the first item of the scrollable transcript (not gated
	// on an empty transcript), framed in a plain ASCII box.
	if vs.Banner != nil {
		out = append(out, plainBanner(vs.Banner, width)...)
	}

	// Transcript.
	for _, m := range vs.Transcript {
		out = append(out, plainMessage(m, width)...)
	}
	return out
}

// RenderBottom implements Renderer: the pinned bottom region (dialog, or the
// composer + agents + status + footer), ANSI-free.
func (pr PlainRenderer) RenderBottom(vs ViewState) []string {
	width := vs.Width
	if width <= 0 {
		width = 80
	}
	var out []string

	// Active modal dialog takes over the lower region.
	if d := vs.Dialog; d != nil {
		out = append(out, "")
		out = append(out, truncate(d.Title, width))
		for _, b := range d.Body {
			out = append(out, truncate(b, width))
		}
		switch d.Kind {
		case DialogList:
			// A visible search field (the ANSI-free form of the picker search box),
			// then the windowed item rows.
			out = append(out, plainSearchField(d, width)...)
			items, start := d.VisibleItems()
			if start > 0 {
				out = append(out, truncate("  ↑ more", width))
			}
			for i, it := range items {
				idx := start + i
				marker := "  "
				if idx == d.Selected {
					marker = "> "
				}
				line := marker + it.Label
				if it.Detail != "" {
					line += "  " + it.Detail
				}
				out = append(out, truncate(line, width))
			}
			if start+len(items) < len(d.FilteredItems()) {
				out = append(out, truncate("  ↓ more", width))
			}
		case DialogSecret:
			// Masked entry: never the raw secret, one glyph per rune.
			out = append(out, truncate("  "+d.Masked()+"_", width))
		default:
			for i, opt := range d.Options {
				marker := "  "
				if i == d.Selected {
					marker = "> "
				}
				box := ""
				if d.MultiSelect {
					if d.IsMarked(i) {
						box = "[x] "
					} else {
						box = "[ ] "
					}
				}
				out = append(out, truncate(marker+box+opt.Label, width))
			}
			if d.AwaitingFeedback {
				out = append(out, truncate("  Tell gocode what to do differently: "+d.Feedback+"_", width))
			}
		}
		return out
	}

	// Status / spinner line, drawn ABOVE the input (thinking/elapsed/tokens sit
	// just over the composer while a turn runs).
	if vs.Status.IsActive() {
		line := strings.TrimSpace(vs.Status.Glyph() + " " + vs.Status.StatusLine())
		out = append(out, truncate(line, width))
	} else if vs.Status.Done != "" {
		// Finished-turn summary ("2m 14s · done · 14:32") until the next turn.
		out = append(out, truncate("✻ "+vs.Status.Done, width))
	}

	// Composer line. The PlainRenderer intentionally does NOT draw either composer
	// autocomplete overlay — neither the slash-command menu (vs.Commands /
	// CommandMenuActive) nor the "@" file-mention menu (vs.Files /
	// FileMenuActive): it is the non-TTY fallback, where a live popup has no
	// meaning. Both are LipglossRenderer-only overlays; the pure ViewState still
	// tracks them so the model and its tests behave identically under either
	// renderer.
	prompt := pr.Prompt
	if prompt == "" {
		prompt = ">"
	}
	if vs.Composer.Mode == ModeBash {
		prompt = "!"
	}
	// Command-history label on the composer's top rule while browsing history
	// (ANSI-free form: "History N/M" between dashes). Absent at the
	// live draft (HistoryLabel returns "").
	if label := vs.HistoryLabel(); label != "" {
		out = append(out, truncate("── "+label+" "+strings.Repeat("─", max0(width-len(label)-4)), width))
	}
	// A simple "_" caret marks the cursor column (block/underscore for the ANSI-
	// free fallback), visible even on an empty composer: the plain-text form of
	// the reverse-video caret.
	out = append(out, truncate(prompt+" "+plainCaret(vs.Composer.Value, vs.Composer.Cursor), width))

	// Permission-mode indicator (ANSI-free): a compact mode line just below the
	// input, e.g. "⏵⏵ accept edits on". Empty for the default (manual) mode.
	if line := vs.Footer.Mode.ModeLine(); line != "" {
		out = append(out, truncate(line, width))
	}

	// Running-agents panel: in-flight Agent/Task subagents, listed below the
	// composer (absent when none are running).
	for _, a := range vs.ActiveAgents {
		name := a.Name
		if name == "" {
			name = "agent"
		}
		out = append(out, truncate(fmt.Sprintf("%s %s working… (%ds)", "●", name, a.Elapsed), width))
	}

	// Footer: token readout.
	if footer := renderFooter(vs.Footer, width); footer != "" {
		out = append(out, footer)
	}

	// Transient keyboard-shortcut hint (double-press exit/clear), plain text.
	if vs.Footer.ExitMessage != "" {
		out = append(out, truncate(vs.Footer.ExitMessage, width))
	}

	return out
}

// max0 returns n when positive, else 0.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// plainSearchField renders the DialogList search input for the ANSI-free
// PlainRenderer: a clearly-delimited "⌕ Search: <query>" line with a "_" caret
// (a dim "Type to search…" placeholder when empty) and an "N/M" match count.
func plainSearchField(d *Dialog, width int) []string {
	n := len(d.FilteredItems())
	m := len(d.Items)
	count := fmt.Sprintf("%d/%d", n, m)

	var body string
	if d.Filter == "" {
		body = SearchPlaceholder
	} else {
		body = d.Filter + "_"
	}
	left := SearchPromptGlyph + " Search: " + body

	// Right-align the count when it fits, otherwise just show the (truncated) line.
	countCells := textutil.CountCodePoints(count) + 1
	if textutil.CountCodePoints(left)+countCells <= width {
		pad := width - textutil.CountCodePoints(left) - countCells
		return []string{left + strings.Repeat(" ", pad) + " " + count}
	}
	return []string{truncate(left, width)}
}

// plainMessage renders one transcript message as ANSI-free lines: a tool-call
// block (header + truncated result), a diff block (+/- signed lines), or
// role-prefixed prose.
func plainMessage(m Message, width int) []string {
	switch {
	case m.Tool != nil:
		t := m.Tool
		out := []string{truncate(RoleToolUse.Prefix()+t.Header(), width)}
		if t.Collapsed {
			return out
		}
		lines, overflow := t.VisibleResultLines()
		for _, ln := range lines {
			// The plain renderer is ANSI-free: kept tool-output colours go.
			out = append(out, truncate("    "+SanitizeText(ln), width))
		}
		if overflow > 0 {
			out = append(out, truncate(fmt.Sprintf("    … +%d %s", overflow, linesWord(overflow)), width))
		}
		return out
	case m.Diff != nil:
		d := m.Diff
		out := []string{truncate("  diff "+d.Path, width)}
		for _, h := range d.Hunks {
			if h.Header != "" {
				out = append(out, truncate("  "+h.Header, width))
			}
			for _, ln := range h.Lines {
				sign := " "
				switch ln.Kind {
				case DiffAdd:
					sign = "+"
				case DiffRemove:
					sign = "-"
				}
				out = append(out, truncate("  "+sign+" "+ln.Text, width))
			}
		}
		return out
	default:
		prefix := m.Role.Prefix()
		var out []string
		for i, ln := range strings.Split(m.Text, "\n") {
			p := prefix
			if i > 0 {
				p = strings.Repeat(" ", textutil.CountCodePoints(prefix))
			}
			out = append(out, truncate(p+ln, width))
		}
		return out
	}
}

// renderFooter composes the always-on footer. It now carries ONLY the
// cumulative session token readout, right-aligned. The model name, the
// permission-mode pill and the persistent key-shortcut hints were removed from
// the footer per the product direction that the footer show just the token
// usage (the active model is still shown in the welcome banner, and the
// SetFooterModel/SetFooterTokens host messages are kept for API stability — the
// model segment is simply no longer rendered here). Nothing renders at 0/0.
func renderFooter(f Footer, width int) string {
	readout := f.statusReadout()
	if readout == "" {
		return ""
	}
	rw := textutil.CountCodePoints(readout)
	if rw < width {
		return strings.Repeat(" ", width-rw) + readout
	}
	return truncate(readout, width)
}

// plainCaret renders value with a simple "_" caret inserted at the cursor rune
// offset (so an empty composer shows "_" and a mid-line cursor shows the caret
// between runes), for the ANSI-free PlainRenderer.
func plainCaret(value string, cursor int) string {
	r := []rune(value)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(r) {
		cursor = len(r)
	}
	return string(r[:cursor]) + "_" + string(r[cursor:])
}

// plainBanner frames a welcome banner in a plain ASCII box, preceded by the
// user's custom "terminal buddy" mascot (welcomeMascot) rendered without color.
// The mascot is dropped when the terminal is too narrow to hold it, so the
// banner never overflows. PlainRenderer draws no background and no accent color.
func plainBanner(b *Banner, width int) []string {
	lines := b.Lines()
	if len(lines) == 0 {
		return nil
	}
	var out []string
	// Mascot column (plain ASCII, no color), stacked above the boxed text. Only
	// shown when every mascot row fits the terminal width.
	if width >= welcomeMascotWidth {
		for _, ln := range welcomeMascot {
			out = append(out, ln)
		}
	}
	out = append(out, plainBannerBox(b, width)...)
	return out
}

// plainBannerBox frames the banner text lines in a plain ASCII box.
func plainBannerBox(b *Banner, width int) []string {
	lines := b.Lines()
	if len(lines) == 0 {
		return nil
	}
	// Box width: the widest content line (capped to the terminal), with a
	// 1-column pad each side inside the vertical bars.
	inner := 0
	for _, ln := range lines {
		if n := textutil.CountCodePoints(ln); n > inner {
			inner = n
		}
	}
	if max := width - 4; max > 0 && inner > max {
		inner = max
	}
	top := "+" + strings.Repeat("-", inner+2) + "+"
	out := []string{top}
	for _, ln := range lines {
		ln = truncate(ln, inner)
		pad := inner - textutil.CountCodePoints(ln)
		out = append(out, "| "+ln+strings.Repeat(" ", pad)+" |")
	}
	out = append(out, top)
	return out
}

// truncate limits s to width code points, appending an ellipsis when cut.
func truncate(s string, width int) string {
	if width <= 0 || textutil.CountCodePoints(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return textutil.FirstCodePoints(s, width-1) + "…"
}
