package toolreg

import (
	"regexp"
	"strings"
)

// parseShellList is a conservative stand-in for a full shell parse plus
// simple-command extraction, used only by the
// read-only classifier. It splits cmd into simple commands joined by ;, &&,
// ||, | or a newline and returns, per command, the de-quoted argv, the
// env-prefix names, the redirections and the raw text; bare holds the names
// of assignment-only statements. ok is false for anything outside that
// shape — the "too-complex", subshell and `&` refusals and every
// construct whose expansion could hide a write: command / process
// substitution, `${…}`, `$VAR`, backticks, heredocs and here-strings,
// subshells, brace groups and brace expansion, `|&`, background `&`,
// zsh `=cmd`, `~[…]` and `<N-M>`, control characters, Unicode whitespace,
// backslash-escaped whitespace and line continuations, unbalanced quotes.
func parseShellList(cmd string) (cmds []shCommand, bare []string, ok bool) {
	if controlCharRe.MatchString(cmd) || unicodeSpaceRe.MatchString(cmd) ||
		tildeBracketRe.MatchString(cmd) || zshEqualsRe.MatchString(cmd) || zshRangeRe.MatchString(cmd) {
		return nil, nil, false
	}
	p := &shParser{s: cmd}
	for {
		c, more, err := p.command()
		if err {
			return nil, nil, false
		}
		if c != nil {
			if len(c.argv) == 0 {
				if len(c.redirects) > 0 {
					return nil, nil, false // a bare redirection
				}
				bare = append(bare, c.envNames...)
			} else {
				cmds = append(cmds, *c)
			}
		}
		if !more {
			break
		}
	}
	return cmds, bare, true
}

var (
	controlCharRe  = regexp.MustCompile(`[\x00-\x08\x0B-\x1F\x7F]`)
	unicodeSpaceRe = regexp.MustCompile(`[\x{00A0}\x{1680}\x{2000}-\x{200B}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]`)
	tildeBracketRe = regexp.MustCompile(`~\[`)
	zshEqualsRe    = regexp.MustCompile(`(?:^|[\s;&|])=[a-zA-Z_]`)
	zshRangeRe     = regexp.MustCompile(`<\d*-\d*>`)
	assignRe       = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\+?=`)
)

type shParser struct {
	s string
	i int
	// sep is the separator that ended the previous command ("" at start).
	sep string
}

// shWord is one lexed word: its raw spelling, its de-quoted value and
// whether its leading NAME= part was unquoted (an assignment candidate).
type shWord struct {
	raw, val string
	assign   bool
}

// command lexes one simple command up to the next separator. more reports
// whether another command follows; err a refusal.
func (p *shParser) command() (c *shCommand, more, err bool) {
	var words []shWord
	var redirects []shRedirect
	startSep := p.sep
	for {
		p.skipBlanks()
		if p.i >= len(p.s) {
			if len(words) == 0 && len(redirects) == 0 && (startSep == "&&" || startSep == "||" || startSep == "|") {
				return nil, false, true // dangling operator
			}
			return p.finish(words, redirects), false, false
		}
		ch := p.s[p.i]
		switch ch {
		case '#':
			// A comment runs to the end of the line.
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
			continue
		case '\n', ';':
			p.i++
			if ch == ';' && p.i < len(p.s) && p.s[p.i] == ';' {
				return nil, false, true // ;; (case terminator)
			}
			if len(words) == 0 && len(redirects) == 0 {
				if startSep == "&&" || startSep == "||" || startSep == "|" {
					return nil, false, true
				}
				if ch == ';' {
					return nil, false, true // empty statement before ;
				}
				continue
			}
			p.sep = string(ch)
			return p.finish(words, redirects), true, false
		case '&':
			if strings.HasPrefix(p.s[p.i:], "&&") {
				if len(words) == 0 && len(redirects) == 0 {
					return nil, false, true
				}
				p.i += 2
				p.sep = "&&"
				return p.finish(words, redirects), true, false
			}
			if strings.HasPrefix(p.s[p.i:], "&>") {
				op := "&>"
				if strings.HasPrefix(p.s[p.i:], "&>>") {
					op = "&>>"
				}
				p.i += len(op)
				r, bad := p.redirectTarget(op)
				if bad {
					return nil, false, true
				}
				redirects = append(redirects, r)
				continue
			}
			return nil, false, true // background `&`
		case '|':
			if strings.HasPrefix(p.s[p.i:], "|&") {
				return nil, false, true
			}
			if len(words) == 0 && len(redirects) == 0 {
				return nil, false, true
			}
			if strings.HasPrefix(p.s[p.i:], "||") {
				p.i += 2
				p.sep = "||"
			} else {
				p.i++
				p.sep = "|"
			}
			return p.finish(words, redirects), true, false
		case '(', ')':
			return nil, false, true // subshell / grouping
		case '<', '>':
			op, bad := p.redirectOp()
			if bad {
				return nil, false, true
			}
			r, bad := p.redirectTarget(op)
			if bad {
				return nil, false, true
			}
			redirects = append(redirects, r)
			continue
		}
		w, bad := p.word()
		if bad {
			return nil, false, true
		}
		// An all-digit word glued to a redirection operator is its fd.
		if p.i < len(p.s) && (p.s[p.i] == '<' || p.s[p.i] == '>') && digitsRe.MatchString(w.raw) {
			op, bad := p.redirectOp()
			if bad {
				return nil, false, true
			}
			r, bad := p.redirectTarget(op)
			if bad {
				return nil, false, true
			}
			redirects = append(redirects, r)
			continue
		}
		words = append(words, w)
	}
}

func (p *shParser) finish(words []shWord, redirects []shRedirect) *shCommand {
	if len(words) == 0 && len(redirects) == 0 {
		return nil
	}
	c := &shCommand{redirects: redirects}
	var raw []string
	i := 0
	for ; i < len(words) && words[i].assign; i++ {
		m := assignRe.FindStringSubmatch(words[i].raw)
		c.envNames = append(c.envNames, m[1])
		raw = append(raw, words[i].raw)
	}
	for ; i < len(words); i++ {
		c.argv = append(c.argv, words[i].val)
		raw = append(raw, words[i].raw)
	}
	c.text = strings.Join(raw, " ")
	return c
}

func (p *shParser) skipBlanks() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// redirectOp lexes a redirection operator at p.i. Heredocs, here-strings,
// read-write opens and process substitution are refused.
func (p *shParser) redirectOp() (string, bool) {
	rest := p.s[p.i:]
	for _, op := range []string{"<<<", "<<", "<>", "<(", ">(", ">>", ">&", ">|", "<&", "<", ">"} {
		if strings.HasPrefix(rest, op) {
			switch op {
			case "<<<", "<<", "<>", "<(", ">(":
				return "", true
			}
			p.i += len(op)
			return op, false
		}
	}
	return "", true
}

// redirectTarget lexes the word after a redirection operator.
func (p *shParser) redirectTarget(op string) (shRedirect, bool) {
	p.skipBlanks()
	if p.i >= len(p.s) || strings.IndexByte(";&|<>()\n", p.s[p.i]) >= 0 {
		return shRedirect{}, true
	}
	w, bad := p.word()
	if bad || w.val == "" {
		return shRedirect{}, true
	}
	return shRedirect{op: op, target: w.val}, false
}

// word lexes one word starting at p.i, removing its quoting.
func (p *shParser) word() (shWord, bool) {
	start := p.i
	var b strings.Builder
	quotedPrefix := -1 // index (in raw) of the first quote/escape
	for p.i < len(p.s) {
		ch := p.s[p.i]
		switch ch {
		case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
			return p.mkWord(start, b.String(), quotedPrefix), false
		case '\'':
			if quotedPrefix < 0 {
				quotedPrefix = p.i - start
			}
			end := strings.IndexByte(p.s[p.i+1:], '\'')
			if end < 0 {
				return shWord{}, true
			}
			b.WriteString(p.s[p.i+1 : p.i+1+end])
			p.i += end + 2
		case '"':
			if quotedPrefix < 0 {
				quotedPrefix = p.i - start
			}
			p.i++
			closed := false
			for p.i < len(p.s) {
				c := p.s[p.i]
				if c == '"' {
					p.i++
					closed = true
					break
				}
				if c == '\\' && p.i+1 < len(p.s) {
					switch p.s[p.i+1] {
					case '"', '\\', '$', '`':
						b.WriteByte(p.s[p.i+1])
						p.i += 2
						continue
					case '\n':
						return shWord{}, true // line continuation
					}
					b.WriteByte(c)
					p.i++
					continue
				}
				if c == '`' {
					return shWord{}, true
				}
				if c == '$' && p.i+1 < len(p.s) && dollarExpands(p.s[p.i+1]) {
					return shWord{}, true
				}
				b.WriteByte(c)
				p.i++
			}
			if !closed {
				return shWord{}, true
			}
		case '\\':
			if quotedPrefix < 0 {
				quotedPrefix = p.i - start
			}
			if p.i+1 >= len(p.s) {
				return shWord{}, true
			}
			n := p.s[p.i+1]
			if n == ' ' || n == '\t' || n == '\n' || n == '\r' {
				return shWord{}, true // backslash-escaped whitespace / continuation
			}
			b.WriteByte(n)
			p.i += 2
		case '`':
			return shWord{}, true
		case '$':
			if p.i+1 < len(p.s) && (dollarExpands(p.s[p.i+1]) || strings.IndexByte(`'"`, p.s[p.i+1]) >= 0) {
				return shWord{}, true
			}
			b.WriteByte(ch)
			p.i++
		case '{', '}':
			// Only a bare `{}` word (find/xargs placeholder) is accepted; any
			// other brace is a group or an expansion.
			if p.i == start && strings.HasPrefix(p.s[p.i:], "{}") && (p.i+2 == len(p.s) || strings.IndexByte(" \t\n;&|<>()", p.s[p.i+2]) >= 0) {
				p.i += 2
				return shWord{raw: "{}", val: "{}"}, false
			}
			return shWord{}, true
		default:
			b.WriteByte(ch)
			p.i++
		}
	}
	return p.mkWord(start, b.String(), quotedPrefix), false
}

func (p *shParser) mkWord(start int, val string, quotedPrefix int) shWord {
	raw := p.s[start:p.i]
	w := shWord{raw: raw, val: val}
	if m := assignRe.FindStringIndex(raw); m != nil && (quotedPrefix < 0 || quotedPrefix >= m[1]) {
		w.assign = true
	}
	return w
}

// dollarExpands reports whether `$` followed by c starts an expansion: a
// name, a special parameter, `(` or `{`.
func dollarExpands(c byte) bool {
	return isIdentStart(c) || strings.IndexByte("@*#?!$0123456789-({", c) >= 0
}
