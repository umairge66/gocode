package toolreg

import (
	"regexp"
	"strings"
)

// sedReadOnly is the sed check without file writes (the sed entry's callback
// in the read-only table): sed is read-only only as a pure printer
// (`sed -n 'Np'`, sedPrintOnly) or a single stdin-to-stdout substitution
// (`sed 's/a/b/g'`, sedSubstitution), and never with a w/W/e command or flag,
// a brace block, a comment, a negation or a GNU address extension
// (sedExprDangerous).
//
// args are the words after "sed". An -i (in-place) branch is unreachable
// without allowFileWrites (neither sedPrintOnly nor sedSubstitution accepts
// -i) and is not implemented; constructs the lexer already refuses are not
// re-checked.
func sedReadOnly(args []string) bool {
	exprs, ok := sedExpressions(args)
	if !ok {
		return false
	}
	hasFiles := sedHasFileArgs(args)
	y := sedPrintOnly(args, exprs)
	s := sedSubstitution(args, exprs, hasFiles)
	if !y && !s {
		return false
	}
	for _, h := range exprs {
		if s && strings.Contains(h, ";") {
			return false
		}
	}
	for _, h := range exprs {
		if sedExprDangerous(h) {
			return false
		}
	}
	return true
}

var (
	sedDangerousCombo1 = regexp.MustCompile(`^-e[wWe]`)
	sedDangerousCombo2 = regexp.MustCompile(`^-w[eE]`)
)

// sedExpressions extracts the sed expressions from args; ok is false when
// they cannot be determined safely.
func sedExpressions(s []string) ([]string, bool) {
	for _, a := range s {
		if sedDangerousCombo1.MatchString(a) || sedDangerousCombo2.MatchString(a) {
			return nil, false
		}
	}
	if len(s) == 0 {
		return nil, false
	}
	var n []string
	h, y := false, false
	for i := 0; i < len(s); i++ {
		w := s[i]
		switch {
		case (w == "-e" || w == "--expression") && i+1 < len(s):
			h = true
			n = append(n, s[i+1])
			i++
			continue
		case strings.HasPrefix(w, "--expression="):
			h = true
			n = append(n, w[len("--expression="):])
			continue
		case strings.HasPrefix(w, "-e="):
			h = true
			n = append(n, w[3:])
			continue
		case strings.HasPrefix(w, "-"):
			continue
		}
		if !h && !y {
			n = append(n, w)
			y = true
			continue
		}
		break
	}
	return n, true
}

// sedHasFileArgs reports whether sed is given file operands.
func sedHasFileArgs(r []string) bool {
	s, h := 0, false
	for y := 0; y < len(r); y++ {
		S := r[y]
		switch {
		case (S == "-e" || S == "--expression") && y+1 < len(r):
			h = true
			y++
			continue
		case strings.HasPrefix(S, "--expression="), strings.HasPrefix(S, "-e="):
			h = true
			continue
		case strings.HasPrefix(S, "-"):
			continue
		}
		s++
		if h || s > 1 {
			return true
		}
	}
	return false
}

// sedFlagsAllowed reports whether every flag is in the allowed list.
func sedFlagsAllowed(flags, allowed []string) bool {
	for _, r := range flags {
		if strings.HasPrefix(r, "-") && !strings.HasPrefix(r, "--") && len(r) > 2 {
			for i := 1; i < len(r); i++ {
				if !contains(allowed, "-"+string(r[i])) {
					return false
				}
			}
		} else if !contains(allowed, r) {
			return false
		}
	}
	return true
}

func sedFlags(args []string) []string {
	var out []string
	for _, w := range args {
		if strings.HasPrefix(w, "-") && w != "--" {
			out = append(out, w)
		}
	}
	return out
}

var sedPrintCmd = regexp.MustCompile(`^(?:\d+|\d+,\d+)?p$`)

// sedPrintOnly accepts a pure printer (`sed -n 'Np'`).
func sedPrintOnly(args, exprs []string) bool {
	h := sedFlags(args)
	if !sedFlagsAllowed(h, []string{"-n", "--quiet", "--silent", "-E", "--regexp-extended", "-r", "-z", "--zero-terminated", "--posix"}) {
		return false
	}
	quiet := false
	for _, w := range h {
		if w == "-n" || w == "--quiet" || w == "--silent" || strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "n") {
			quiet = true
			break
		}
	}
	if !quiet || len(exprs) == 0 {
		return false
	}
	for _, w := range exprs {
		for _, q := range strings.Split(w, ";") {
			if !sedPrintCmd.MatchString(strings.TrimSpace(q)) {
				return false
			}
		}
	}
	return true
}

var sedSubstFlags = regexp.MustCompile(`^[gpimIM]*[1-9]?[gpimIM]*$`)

// sedSubstitution accepts a single substitution, without allowFileWrites.
func sedSubstitution(args, exprs []string, hasFiles bool) bool {
	if hasFiles {
		return false
	}
	if !sedFlagsAllowed(sedFlags(args), []string{"-E", "--regexp-extended", "-r", "--posix"}) {
		return false
	}
	if len(exprs) != 1 {
		return false
	}
	q := strings.TrimSpace(exprs[0])
	if !strings.HasPrefix(q, "s/") || strings.Contains(q, "\n") {
		return false
	}
	Q := q[2:]
	count, last := 0, -1
	for i := 0; i < len(Q); i++ {
		if Q[i] == '\\' {
			i++
			continue
		}
		if Q[i] == '/' {
			count++
			last = i
		}
	}
	if count != 2 {
		return false
	}
	return sedSubstFlags.MatchString(Q[last+1:])
}

var (
	sedNonASCII     = regexp.MustCompile(`[^\x01-\x7F]`)
	sedNegation     = regexp.MustCompile(`^!|[/\d$]!`)
	sedStep         = regexp.MustCompile(`\d\s*~\s*\d|,\s*~\s*\d|\$\s*~\s*\d`)
	sedRelRange     = regexp.MustCompile(`,\s*[+-]`)
	sedOddDelim     = regexp.MustCompile(`s\\|\\[|#%@]`)
	sedEscSlashW    = regexp.MustCompile(`\\/.*[wW]`)
	sedSpaceCmd     = regexp.MustCompile(`/[^/]*\s+[wWeE]`)
	sedPlainSubst   = regexp.MustCompile(`^s/[^/]*/[^/]*/[^/]*$`)
	sedAnySubst     = regexp.MustCompile(`^s.`)
	sedEndsWE       = regexp.MustCompile(`[wWeE]$`)
	sedWE           = regexp.MustCompile(`[wWeE]`)
	sedYCmd         = regexp.MustCompile(`y[^\\\n]`)
	sedWriteForms   = mustAll(`^[wW]\s*\S+`, `^\d+\s*[wW]\s*\S+`, `^\$\s*[wW]\s*\S+`, `^/[^/]*/[IMim]*\s*[wW]\s*\S+`, `^\d+,\d+\s*[wW]\s*\S+`, `^\d+,\$\s*[wW]\s*\S+`, `^/[^/]*/[IMim]*,/[^/]*/[IMim]*\s*[wW]\s*\S+`)
	sedExecuteForms = mustAll(`^e`, `^\d+\s*e`, `^\$\s*e`, `^/[^/]*/[IMim]*\s*e`, `^\d+,\d+\s*e`, `^\d+,\$\s*e`, `^/[^/]*/[IMim]*,/[^/]*/[IMim]*\s*e`)
)

func mustAll(srcs ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(srcs))
	for i, s := range srcs {
		out[i] = regexp.MustCompile(s)
	}
	return out
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// sedExprDangerous reports a sed expression that could write, execute or use
// unsupported syntax.
func sedExprDangerous(e string) bool {
	n := strings.TrimSpace(e)
	if n == "" {
		return false
	}
	if sedNonASCII.MatchString(n) {
		return true
	}
	if strings.ContainsAny(n, "{}") || strings.ContainsAny(n, "\n\r") {
		return true
	}
	if r := strings.Index(n, "#"); r != -1 && !(r > 0 && n[r-1] == 's') {
		return true
	}
	if sedNegation.MatchString(n) || sedStep.MatchString(n) || strings.HasPrefix(n, ",") || sedRelRange.MatchString(n) {
		return true
	}
	if sedOddDelim.MatchString(n) || sedEscSlashW.MatchString(n) || sedSpaceCmd.MatchString(n) {
		return true
	}
	if strings.HasPrefix(n, "s/") && !sedPlainSubst.MatchString(n) {
		return true
	}
	if sedAnySubst.MatchString(n) && sedEndsWE.MatchString(n) && !sedSubstTailClean(n) {
		return true
	}
	if anyMatch(sedWriteForms, n) || anyMatch(sedExecuteForms, n) {
		return true
	}
	if tail, ok := sedSubstTail(n); ok && strings.ContainsAny(tail, "wWeE") {
		return true
	}
	if sedYCmd.MatchString(n) && sedWE.MatchString(n) {
		return true
	}
	return false
}

// sedSubstTailClean is /^s([^\\\n]).*?\1.*?\1[^wWeE]*$/ (a back-reference,
// so hand-written): after the delimiter's third occurrence the rest has no
// w/W/e/E.
func sedSubstTailClean(n string) bool {
	if len(n) < 2 || n[0] != 's' || n[1] == '\\' || n[1] == '\n' {
		return false
	}
	d := n[1]
	last, count := -1, 0
	for i := 2; i < len(n); i++ {
		if n[i] == d {
			count++
			last = i
		}
	}
	if count < 2 {
		return false
	}
	return !strings.ContainsAny(n[last+1:], "wWeE")
}

// sedSubstTail is the unanchored /s([^\\\n]).*?\1.*?\1(.*?)$/: the text after
// the first s-command's third delimiter (lazy, leftmost match).
func sedSubstTail(n string) (string, bool) {
	for p := 0; p+1 < len(n); p++ {
		if n[p] != 's' || n[p+1] == '\\' || n[p+1] == '\n' {
			continue
		}
		d := n[p+1]
		i := strings.IndexByte(n[p+2:], d)
		if i < 0 {
			continue
		}
		i += p + 2
		j := strings.IndexByte(n[i+1:], d)
		if j < 0 {
			continue
		}
		j += i + 1
		return n[j+1:], true
	}
	return "", false
}
