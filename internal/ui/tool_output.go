package ui

import "strings"

// sgrReset ends every displayed tool-output piece that carries colour.
const sgrReset = "\x1b[0m"

// wrapResultLine wraps one tool-result line for display. Plain lines go
// through wrap (the renderer's own wrapper) unchanged. A line that kept SGR
// colours (see SanitizeToolOutput) is wrapped ANSI-aware and each piece is
// made self-contained by closeSGRPieces: it re-opens the colour carried in
// from the previous piece and ends with a reset, so a colour left open in a
// result never bleeds into the gutter, the next row, the next transcript
// message, a sidebar or a dialog.
func wrapResultLine(ln string, width int, wrap func(string, int) []string) []string {
	if strings.IndexByte(ln, 0x1b) < 0 {
		return wrap(ln, width)
	}
	return closeSGRPieces(wrapCells(ln, width))
}

// closeSGRPieces makes each piece of one wrapped line self-contained: the SGR
// state open at the end of piece i is re-emitted at the start of piece i+1,
// and every piece that carries colour starts and ends with a reset.
func closeSGRPieces(pieces []string) []string {
	state := ""
	out := make([]string, len(pieces))
	for i, p := range pieces {
		hasESC := strings.IndexByte(p, 0x1b) >= 0
		if state == "" && !hasESC {
			out[i] = p
			continue
		}
		// The leading reset drops the renderer's muted result style so the
		// kept colours show as they would in a terminal.
		out[i] = sgrReset + state + p + sgrReset
		if hasESC {
			state = sgrStateAfter(state, p)
		}
	}
	return out
}

// sgrStateAfter returns the SGR sequences still in effect after p, starting
// from state: everything since the last full reset (ESC[m, ESC[0m, or an SGR
// list starting with 0). It is an approximation that only ever re-emits
// sequences that were already in the (sanitised) text.
func sgrStateAfter(state, p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] != 0x1b || i+1 >= len(p) || p[i+1] != '[' {
			continue
		}
		k := i + 2
		for k < len(p) && (p[k] >= '0' && p[k] <= '9' || p[k] == ';' || p[k] == ':') {
			k++
		}
		if k >= len(p) || p[k] != 'm' {
			continue
		}
		params := p[i+2 : k]
		seq := p[i : k+1]
		switch {
		case params == "" || strings.TrimLeft(params, "0") == "":
			state = ""
		case strings.HasPrefix(params, "0;") || strings.HasPrefix(params, ";"):
			state = seq
		default:
			state += seq
		}
		if len(state) > 512 { // pathological: stop carrying rather than grow
			state = ""
		}
		i = k
	}
	return state
}
