package cli

import (
	"regexp"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// `gocode logs` replay filter: keep the printable text plus the escape
// sequences that are safe to replay into the user's terminal (SGR colours and
// simple cursor/erase movement), drop everything else (mode switches, OSC,
// DCS, …), and report whether the output addressed the cursor absolutely
// (alternate screen or CUP), in which case the caller parks the cursor at the
// bottom afterwards.

var replayTokenRE = regexp.MustCompile(
	"\x1b\\[(?P<params>[<-?]?[0-;]*)(?P<inter>[ -/]*)(?P<final>[@-~])" +
		"|\x1b[\\]PX^_][^\x07\x18\x1a\x1b\u009c]*(?:\x07|\x1b\\\\|\u009c)?" +
		"|\x1b(?:\\[[0-?]*[ -/]*|[ -/]*)$" +
		"|\x1b(?P<esc>[ -/]*[0-~])" +
		"|\x1b" +
		"|[\x00-\x07\x0e-\x1a\x1c-\x1f\x7f-\u009f]")

const replayCSIFinals = "ABCDEFGHJKLMPSTXZ@`adefm"

func replayCSIAllowed(params string, final byte) bool {
	if !strings.ContainsRune(replayCSIFinals, rune(final)) {
		return false
	}
	if final != 'J' && final != 'T' {
		return true
	}
	parts := strings.Split(params, ";")
	nums := make([]int, len(parts))
	for i, p := range parts {
		if p == "" {
			nums[i] = 0
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		nums[i] = n
	}
	if final == 'J' {
		for _, n := range nums {
			if n != 0 && n != 1 && n != 2 {
				return false
			}
		}
		return true
	}
	return len(parts) == 1
}

// replayFilter returns the filtered text and whether it addressed the cursor.
func replayFilter(s string) (string, bool) {
	var b strings.Builder
	addressed := false
	last := 0
	names := replayTokenRE.SubexpNames()
	for _, m := range replayTokenRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[0]])
		last = m[1]
		group := func(name string) (string, bool) {
			for i, n := range names {
				if n == name && m[2*i] >= 0 {
					return s[m[2*i]:m[2*i+1]], true
				}
			}
			return "", false
		}
		if final, ok := group("final"); ok {
			params, _ := group("params")
			inter, _ := group("inter")
			lead := byte(0)
			if params != "" {
				lead = params[0]
			}
			private := lead == '?' || lead == '>' || lead == '<' || lead == '='
			if lead == '?' && final == "h" {
				for _, mode := range strings.Split(params[1:], ";") {
					if mode == "1049" || mode == "1047" || mode == "47" {
						addressed = true
					}
				}
			} else if !private && inter == "" && replayCSIAllowed(params, final[0]) {
				if final == "H" || final == "f" {
					addressed = true
				}
				b.WriteString(s[m[0]:m[1]])
			}
			continue
		}
		if esc, ok := group("esc"); ok && len(esc) == 1 && strings.Contains("78DEM", esc) {
			b.WriteString(s[m[0]:m[1]])
		}
	}
	b.WriteString(s[last:])
	return b.String(), addressed
}
