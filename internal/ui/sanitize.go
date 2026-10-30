package ui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the single terminal-safety choke point for everything the ui
// draws. Text that reaches the screen comes from untrusted places — file
// contents shown by Read, Bash output, fetched web pages, MCP tool results and
// server-supplied names, model output, filenames, git branch names, plugin
// names, peer messages, error bodies — and any of it can carry raw terminal
// control sequences: OSC 52 (write the clipboard), OSC 0/2 (window title), OSC
// 8 (spoofed hyperlinks), DCS/APC/PM strings, CSI cursor movement or screen
// clears (overdraw the permission prompt), the bracketed-paste end marker,
// terminal QUERIES whose answer the terminal types back into stdin (DECRQSS,
// CSI 21 t, OSC 10 ;?), C1 controls (U+009B is a one-character CSI), carriage
// return / backspace overwrite tricks, and Unicode bidi overrides that reorder
// what the user reads (Trojan Source).
//
// Three sanitisers cover the cases:
//
//   - SanitizeText: untrusted multi-line display text. Every escape sequence
//     and control character is removed (\n and \t survive; \r and the Unicode
//     line/paragraph separators become \n), invalid UTF-8 becomes U+FFFD, and
//     the bidi embedding/override/isolate controls and marks are dropped.
//   - SanitizeLine: SanitizeText folded to one line (newlines/tabs -> space),
//     for headers, labels, names and other single-line slots.
//   - SanitizeStyled: like SanitizeText but SGR colour sequences are kept, for
//     host text that is legitimately coloured (the user's statusline command).
//   - SanitizeToolOutput: like SanitizeText but SGR sequences made only of
//     known-safe colours/attributes are kept, for tool results (Bash output).
//     Renderers close each displayed result line with a reset.
//   - VisibleControls: the exact text with every control, format and invisible
//     character made VISIBLE as an escape (\x1b, \r, ‮ …) instead of
//     being dropped — for permission prompts, where the user must see exactly
//     what will run.
//
// On top of the ingress sanitisation (sanitizeEvent, applied by Reduce and the
// Model's direct setters), the full-screen frame is passed through filterFrame,
// which lets only SGR sequences through: whatever text slips past ingress can
// still never move the cursor, clear the screen, set the title or reach the
// clipboard.

type sanitizeMode int

const (
	modeText sanitizeMode = iota
	modeLine
	modeStyled
	modeToolOutput
)

// SanitizeText strips terminal control sequences and control characters from
// untrusted multi-line display text (see the file comment).
func SanitizeText(s string) string { return sanitize(s, modeText) }

// SanitizeLine is SanitizeText folded to a single line: newlines, carriage
// returns and tabs become a single space each.
func SanitizeLine(s string) string { return sanitize(s, modeLine) }

// SanitizeStyled is SanitizeText that keeps plain SGR (colour/attribute)
// sequences — except SGR 8 (conceal), which hides text — so a trusted, coloured
// host string keeps its colours while still never moving the cursor.
func SanitizeStyled(s string) string { return sanitize(s, modeStyled) }

// SanitizeToolOutput is SanitizeText for tool results (Bash output, coloured
// test runs, `ls --color`, `git diff --color`): it keeps SGR sequences whose
// every parameter is a known-safe colour or attribute (see safeSGRParams) and
// removes everything else exactly as SanitizeText does — OSC/DCS/APC/PM
// strings, cursor movement, erase, queries, C1 controls, bidi controls, SGR 8
// (conceal) and any unknown SGR parameter. The model always receives the raw
// text; this is display only. The renderers close every displayed result line
// with a reset (see closeSGRPieces) so a colour left open cannot bleed.
func SanitizeToolOutput(s string) string { return sanitize(s, modeToolOutput) }

// maxSafeSGRLen bounds a kept SGR parameter list; real colour output never
// comes close.
const maxSafeSGRLen = 64

// safeSGRParams reports whether an SGR parameter list (the bytes between
// "ESC [" and "m") holds only plain colours and the common attributes:
//
//	0 (or empty) reset; 1 bold, 2 dim, 3 italic, 4 underline, 5 blink,
//	7 inverse, 9 strikethrough and their resets 21-25, 27, 29;
//	30-37/39, 40-47/49, 90-97, 100-107; 38/48;5;n and 38/48;2;r;g;b
//	(also in the colon form 38:5:n, 38:2:r:g:b, 38:2::r:g:b).
//
// SGR 8 (conceal) and anything else make the whole sequence unsafe.
func safeSGRParams(p string) bool {
	if len(p) > maxSafeSGRLen || !isSGRBody(p) {
		return false
	}
	if p == "" {
		return true
	}
	fields := strings.Split(p, ";")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.Contains(f, ":") {
			if !safeColonColour(f) {
				return false
			}
			continue
		}
		n, ok := sgrNum(f)
		if !ok {
			return false
		}
		switch {
		case n == 0, n >= 1 && n <= 5, n == 7, n == 9,
			n >= 21 && n <= 25, n == 27, n == 29,
			n >= 30 && n <= 37, n == 39, n >= 40 && n <= 47, n == 49,
			n >= 90 && n <= 97, n >= 100 && n <= 107:
		case n == 38 || n == 48:
			if i+1 >= len(fields) {
				return false
			}
			mode, ok := sgrNum(fields[i+1])
			if !ok {
				return false
			}
			args := 0
			switch mode {
			case 5:
				args = 1
			case 2:
				args = 3
			default:
				return false
			}
			if i+1+args >= len(fields) {
				return false
			}
			for _, a := range fields[i+2 : i+2+args] {
				if v, ok := sgrNum(a); !ok || v > 255 {
					return false
				}
			}
			i += 1 + args
		default:
			return false
		}
	}
	return true
}

// safeColonColour accepts the colon sub-parameter colour forms 38:5:n,
// 48:5:n, 38:2:r:g:b and 38:2:<cs>:r:g:b (empty colour-space id).
func safeColonColour(f string) bool {
	parts := strings.Split(f, ":")
	if n, ok := sgrNum(parts[0]); !ok || (n != 38 && n != 48) || len(parts) < 3 {
		return false
	}
	var args []string
	switch parts[1] {
	case "5":
		args = parts[2:]
		if len(args) != 1 {
			return false
		}
	case "2":
		args = parts[2:]
		if len(args) == 4 && args[0] == "" {
			args = args[1:]
		}
		if len(args) != 3 {
			return false
		}
	default:
		return false
	}
	for _, a := range args {
		if v, ok := sgrNum(a); !ok || v > 255 {
			return false
		}
	}
	return true
}

// sgrNum parses one SGR parameter (digits only; empty means 0).
func sgrNum(f string) (int, bool) {
	if len(f) > 3 {
		f = strings.TrimLeft(f, "0")
		if len(f) > 3 {
			return 0, false
		}
	}
	n := 0
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// needsSanitize reports whether s holds anything a sanitiser would change. It
// is the allocation-free fast path: ordinary text is returned as is.
func needsSanitize(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f {
				return true
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			return true
		}
		if isC1(r) || isBidiControl(r) || r == 0x2028 || r == 0x2029 {
			return true
		}
		i += n
	}
	return false
}

func isC1(r rune) bool { return r >= 0x80 && r <= 0x9f }

// isBidiControl reports the Unicode bidirectional formatting characters: the
// embeddings/overrides (LRE RLE PDF LRO RLO), the isolates (LRI RLI FSI PDI)
// and the implicit marks (LRM RLM ALM). They reorder what the user reads
// without changing what the bytes say.
func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0x200e || r == 0x200f || r == 0x061c
}

// escapeEnd scans the escape sequence that starts at s[i] (an ESC byte, or a C1
// introducer whose width is introLen) and returns the index just past it and
// whether it is a keepable SGR sequence. kind is the byte that follows ESC (or
// the 7-bit equivalent of the C1 control).
func escapeEnd(s string, i, introLen int, kind byte) (end int, sgr bool) {
	j := i + introLen
	switch kind {
	case '[': // CSI: params 0x30-0x3f, intermediates 0x20-0x2f, final 0x40-0x7e
		k := j
		for k < len(s) && s[k] >= 0x30 && s[k] <= 0x3f {
			k++
		}
		params := s[j:k]
		inter := k
		for k < len(s) && s[k] >= 0x20 && s[k] <= 0x2f {
			k++
		}
		if k < len(s) && s[k] >= 0x40 && s[k] <= 0x7e {
			return k + 1, s[k] == 'm' && inter == k && plainSGRParams(params)
		}
		return k, false // unterminated / malformed: drop what was consumed
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: a control string
		for k := j; k < len(s); {
			c := s[k]
			switch {
			case c == 0x07:
				return k + 1, false
			case c == 0x1b && k+1 < len(s) && s[k+1] == '\\':
				return k + 2, false
			case c == 0x1b:
				// A new escape aborts the string (as terminals do).
				return k, false
			case c == '\n':
				// Unterminated: stop at the end of the line so one stray
				// introducer cannot swallow the rest of the text.
				return k, false
			case c >= utf8.RuneSelf:
				r, n := utf8.DecodeRuneInString(s[k:])
				if r == 0x9c { // C1 ST
					return k + n, false
				}
				k += n
				continue
			}
			k++
		}
		return len(s), false
	}
	switch {
	case kind >= 0x20 && kind <= 0x2f: // nF: intermediates then a final byte
		k := j
		for k < len(s) && s[k] >= 0x20 && s[k] <= 0x2f {
			k++
		}
		if k < len(s) && s[k] >= 0x30 && s[k] <= 0x7e {
			k++
		}
		return k, false
	case kind >= 0x30 && kind <= 0x7e: // two-byte Fp/Fe/Fs escape
		return j, false
	}
	// A lone introducer (end of text, or followed by a control byte).
	return j, false
}

// plainSGRParams reports whether an SGR parameter list is digits and
// separators only and does not turn on conceal (SGR 8), which hides text. An 8
// that is an argument of an extended colour (38;5;8, 48;2;8;8;8) is a colour,
// not conceal.
func plainSGRParams(p string) bool {
	if !isSGRBody(p) {
		return false
	}
	fields := strings.FieldsFunc(p, func(r rune) bool { return r == ';' })
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.Contains(f, ":") {
			continue // colon sub-parameters (38:2:r:g:b) carry their own args
		}
		switch strings.TrimLeft(f, "0") {
		case "8":
			return false
		case "38", "48", "58":
			if i+1 < len(fields) {
				switch strings.TrimLeft(fields[i+1], "0") {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
		}
	}
	return true
}

func sanitize(s string, mode sanitizeMode) string {
	if !needsSanitize(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == 0x1b:
				var kind byte
				if i+1 < len(s) {
					kind = s[i+1]
				}
				if i+1 >= len(s) || kind < 0x20 || kind > 0x7e {
					i++ // lone ESC
					continue
				}
				end, sgr := escapeEnd(s, i, 2, kind)
				switch {
				case sgr && mode == modeStyled:
					b.WriteString(s[i:end])
				case sgr && mode == modeToolOutput && safeSGRParams(s[i+2:end-1]):
					b.WriteString(s[i:end])
				}
				i = end
				continue
			case c == '\n' || c == '\t':
				if mode == modeLine {
					b.WriteByte(' ')
				} else {
					b.WriteByte(c)
				}
			case c == '\r':
				if i+1 < len(s) && s[i+1] == '\n' {
					i++
					continue // CRLF: the \n is written next
				}
				// A bare CR would return the cursor and let the rest of the
				// line overwrite what came before: show it as a line break.
				if mode == modeLine {
					b.WriteByte(' ')
				} else {
					b.WriteByte('\n')
				}
			case c < 0x20 || c == 0x7f:
				// Other C0 controls (BEL, BS, VT, FF, NUL …) and DEL: dropped.
			default:
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
		case isC1(r):
			// 8-bit controls: U+009B is CSI, U+009D OSC, U+0090 DCS … Parse
			// the sequence they introduce and drop it; other C1s are dropped.
			kind := byte(r - 0x40)
			switch kind {
			case '[', ']', 'P', 'X', '^', '_':
				end, _ := escapeEnd(s, i, n, kind)
				i = end
				continue
			}
			if r == 0x85 { // NEL is a line break
				if mode == modeLine {
					b.WriteByte(' ')
				} else {
					b.WriteByte('\n')
				}
			}
		case r == 0x2028 || r == 0x2029:
			if mode == modeLine {
				b.WriteByte(' ')
			} else {
				b.WriteByte('\n')
			}
		case isBidiControl(r):
			// dropped
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// VisibleControls returns s with every control, format and invisible character
// made visible as a Go-style escape — ESC as \x1b, CR as \r, a C1 control as
// \u009b, a bidi override as ‮, a zero-width space as ​, invalid
// UTF-8 as \xff — and nothing removed. Newlines and tabs are kept as they are.
// It is for permission prompts: the user must see the exact command, with no
// sequence able to restyle, reorder, hide or overdraw any part of it.
func VisibleControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '\n' || c == '\t':
				b.WriteByte(c)
			case c == '\r':
				b.WriteString(`\r`)
			case c < 0x20 || c == 0x7f:
				fmt.Fprintf(&b, `\x%02x`, c)
			default:
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, c)
		case isInvisible(r):
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// isInvisible reports a non-ASCII rune that draws nothing (or reorders/joins
// its neighbours) and so could hide or disguise part of a command: C1
// controls, format characters (Cf: zero-width chars, bidi controls, the BOM,
// soft hyphen, Unicode tags), the line/paragraph separators, and the blank
// "filler" letters some fonts draw as nothing.
func isInvisible(r rune) bool {
	if isC1(r) || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r) {
		return true
	}
	switch r {
	case 0x034f, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180e,
		0x3164, 0xffa0, 0x2800:
		return true
	}
	return false
}

// filterFrame is the last line of defence on a whole rendered frame: it keeps
// printable text, newlines and SGR sequences (all the renderers emit, plus the
// safe colours kept in tool output) except SGR 8 (conceal), and drops
// every other escape sequence, control character and bidi control. Ingress sanitisation
// should already have removed anything hostile; this guarantees that a missed
// path still cannot drive the terminal.
func filterFrame(s string) string {
	if !frameNeedsFilter(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == 0x1b:
				if i+1 >= len(s) || s[i+1] < 0x20 || s[i+1] > 0x7e {
					i++
					continue
				}
				end, _ := escapeEnd(s, i, 2, s[i+1])
				if s[i+1] == '[' && end > i+2 && s[end-1] == 'm' && plainSGRParams(s[i+2:end-1]) {
					b.WriteString(s[i:end])
				}
				i = end
				continue
			case c == '\n' || c == '\t' || c >= 0x20 && c != 0x7f:
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if isC1(r) {
			kind := byte(r - 0x40)
			switch kind {
			case '[', ']', 'P', 'X', '^', '_':
				end, _ := escapeEnd(s, i, n, kind)
				i = end
				continue
			}
			i += n
			continue
		}
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
		case isBidiControl(r):
			// dropped: a stray override must not reorder the frame
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// isSGRBody reports whether a CSI body (between "ESC [" and the final "m") is
// a plain SGR parameter list.
func isSGRBody(p string) bool {
	for i := 0; i < len(p); i++ {
		if c := p[i]; (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}

// frameNeedsFilter is filterFrame's fast path: true when the frame holds any
// escape that is not SGR, a control other than \n/\t, a C1 control or invalid
// UTF-8.
func frameNeedsFilter(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == 0x1b:
				if i+1 >= len(s) || s[i+1] != '[' {
					return true
				}
				k := i + 2
				for k < len(s) && ((s[k] >= '0' && s[k] <= '9') || s[k] == ';' || s[k] == ':') {
					k++
				}
				if k >= len(s) || s[k] != 'm' {
					return true
				}
				// SGR 8 (conceal) hides text: an SGR with an 8 in it is
				// checked in full.
				if strings.IndexByte(s[i+2:k], '8') >= 0 && !plainSGRParams(s[i+2:k]) {
					return true
				}
				i = k + 1
				continue
			case c == '\n' || c == '\t':
			case c < 0x20 || c == 0x7f:
				return true
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && n == 1) || isC1(r) || isBidiControl(r) {
			return true
		}
		i += n
	}
	return false
}

// sanitizeToolCall cleans every displayed field of a tool-call block.
func sanitizeToolCall(t ToolCall) ToolCall {
	t.Name = SanitizeLine(t.Name)
	t.Args = SanitizeLine(t.Args)
	// Tool output keeps its safe SGR colours (coloured test results, ls
	// --color, git diff --color); everything else is stripped.
	t.ResultText = SanitizeToolOutput(t.ResultText)
	t.FullText = SanitizeToolOutput(t.FullText)
	return t
}

func sanitizeDiff(d Diff) Diff {
	d.Path = SanitizeLine(d.Path)
	if len(d.Hunks) == 0 {
		return d
	}
	hunks := make([]DiffHunk, len(d.Hunks))
	for i, h := range d.Hunks {
		h.Header = SanitizeLine(h.Header)
		lines := make([]DiffLine, len(h.Lines))
		for j, l := range h.Lines {
			l.Text = SanitizeText(l.Text)
			lines[j] = l
		}
		h.Lines = lines
		hunks[i] = h
	}
	d.Hunks = hunks
	return d
}

func sanitizeMessage(m Message) Message {
	m.Text = SanitizeText(m.Text)
	if m.Tool != nil {
		t := sanitizeToolCall(*m.Tool)
		m.Tool = &t
	}
	if m.Diff != nil {
		d := sanitizeDiff(*m.Diff)
		m.Diff = &d
	}
	return m
}

// sanitizeDialog returns a cleaned copy of a dialog. The host's own Dialog is
// never written (it may still hold and read it on another goroutine); answers
// reach the host through the OnDialog* callbacks carrying option Values, which
// are left untouched. A permission prompt is not stripped but made exact: every
// control or invisible character in the title, detail lines and option labels
// (which name the rule that would be saved) is shown as a visible escape.
func sanitizeDialog(d *Dialog) *Dialog {
	if d == nil {
		return nil
	}
	text, line := SanitizeText, SanitizeLine
	if d.Kind == DialogPermission {
		text, line = VisibleControls, VisibleControls
	}
	c := *d
	c.Title = text(d.Title)
	c.Body = make([]string, len(d.Body))
	for i, b := range d.Body {
		c.Body[i] = text(b)
	}
	c.Options = append([]DialogOption(nil), d.Options...)
	for i := range c.Options {
		c.Options[i].Label = line(c.Options[i].Label)
	}
	c.Items = append([]ListItem(nil), d.Items...)
	for i := range c.Items {
		c.Items[i].Label = SanitizeLine(c.Items[i].Label)
		c.Items[i].Detail = SanitizeLine(c.Items[i].Detail)
	}
	if d.Marked != nil {
		c.Marked = append([]bool(nil), d.Marked...)
	}
	return &c
}

// sanitizeEvent returns ev with every untrusted display string cleaned. Reduce
// applies it first, so the transcript, dialogs, footer, banner, agent panels
// and menus only ever hold terminal-safe text.
func sanitizeEvent(ev Event) Event {
	switch e := ev.(type) {
	case AppendMessage:
		e.Message = sanitizeMessage(e.Message)
		return e
	case AppendToolCall:
		e.Tool = sanitizeToolCall(e.Tool)
		return e
	case UpdateToolCall:
		e.Tool = sanitizeToolCall(e.Tool)
		return e
	case AppendDiff:
		e.Diff = sanitizeDiff(e.Diff)
		return e
	case StreamAssistant:
		e.Text = SanitizeText(e.Text)
		return e
	case StreamThinking:
		e.Text = SanitizeText(e.Text)
		return e
	case SetStatus:
		e.Status.Label = SanitizeLine(e.Status.Label)
		e.Status.Interrupt = SanitizeLine(e.Status.Interrupt)
		return e
	case SetRetryStatus:
		e.Text = SanitizeLine(e.Text)
		return e
	case OpenDialog:
		e.Dialog = sanitizeDialog(e.Dialog)
		return e
	case SetExitMessage:
		e.Message = SanitizeLine(e.Message)
		return e
	case SetStatusLine:
		// The statusline command is the user's own, and may colour its output.
		e.Line = SanitizeStyled(e.Line)
		return e
	case SetBanner:
		if e.Banner != nil {
			b := *e.Banner
			b.AppName, b.Version, b.Cwd = SanitizeLine(b.AppName), SanitizeLine(b.Version), SanitizeLine(b.Cwd)
			b.Provider, b.Model = SanitizeLine(b.Provider), SanitizeLine(b.Model)
			b.Hint, b.Warning = SanitizeText(b.Hint), SanitizeText(b.Warning)
			b.Greeting = SanitizeLine(b.Greeting)
			e.Banner = &b
		}
		return e
	case SetFooterModel:
		e.Model = SanitizeLine(e.Model)
		return e
	case SetFooterInfo:
		e.Cwd, e.Branch = SanitizeLine(e.Cwd), SanitizeLine(e.Branch)
		return e
	case SetActiveAgents:
		if e.Agents != nil {
			agents := make([]ActiveAgent, len(e.Agents))
			for i, a := range e.Agents {
				a.Name, a.Activity = SanitizeLine(a.Name), SanitizeLine(a.Activity)
				agents[i] = a
			}
			e.Agents = agents
		}
		return e
	case SetViewAgent:
		e.Name = SanitizeLine(e.Name)
		return e
	case SetAgentTranscript:
		e.Name = SanitizeLine(e.Name)
		if e.Entries != nil {
			entries := make([]TranscriptEntry, len(e.Entries))
			for i, t := range e.Entries {
				t.Text = SanitizeText(t.Text)
				t.ToolName, t.ToolArgs = SanitizeLine(t.ToolName), SanitizeLine(t.ToolArgs)
				t.ToolResult = SanitizeToolOutput(t.ToolResult)
				entries[i] = t
			}
			e.Entries = entries
		}
		return e
	case SetCommands:
		if e.Commands != nil {
			cmds := make([]Suggestion, len(e.Commands))
			for i, c := range e.Commands {
				// Name and Value are what completion inserts; only the
				// displayed text is cleaned.
				c.Display, c.Description = SanitizeLine(c.Display), SanitizeLine(c.Description)
				c.ArgumentHint = SanitizeLine(c.ArgumentHint)
				if needsSanitize(c.Name) {
					c.Name = SanitizeLine(c.Name)
				}
				cmds[i] = c
			}
			e.Commands = cmds
		}
		return e
	case SetFiles:
		if e.Files != nil {
			files := make([]FileCandidate, len(e.Files))
			for i, f := range e.Files {
				// A filename with control characters would also inject them
				// into the composer: show it (and insert it) with them
				// visible instead.
				if needsSanitize(f.Path) || hasInvisible(f.Path) {
					f.Path = VisibleControls(f.Path)
				}
				files[i] = f
			}
			e.Files = files
		}
		return e
	}
	return ev
}

// hasInvisible reports whether s holds a rune VisibleControls would escape.
func hasInvisible(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError || isInvisible(r) {
			return true
		}
	}
	return false
}

func sanitizeTodos(in []Todo) []Todo {
	if in == nil {
		return nil
	}
	out := make([]Todo, len(in))
	for i, t := range in {
		t.Text = SanitizeLine(t.Text)
		out[i] = t
	}
	return out
}

func sanitizeMCPServers(in []MCPServer) []MCPServer {
	if in == nil {
		return nil
	}
	out := make([]MCPServer, len(in))
	for i, s := range in {
		s.Name, s.State = SanitizeLine(s.Name), SanitizeLine(s.State)
		out[i] = s
	}
	return out
}

func sanitizeRecentSessions(in []RecentSession) []RecentSession {
	if in == nil {
		return nil
	}
	out := make([]RecentSession, len(in))
	for i, s := range in {
		s.Title, s.When = SanitizeLine(s.Title), SanitizeLine(s.When)
		out[i] = s
	}
	return out
}
