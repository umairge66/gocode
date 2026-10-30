package sandbox

import "strings"

// excludedPattern is one sandbox.excludedCommands entry in Bash
// permission-rule syntax.
type excludedPattern struct {
	kind  string // "prefix", "exact", "wildcard"
	value string
}

func parseExcluded(rule string) excludedPattern {
	rule = strings.TrimSpace(rule)
	switch {
	case strings.HasSuffix(rule, ":*"):
		return excludedPattern{kind: "prefix", value: strings.TrimSpace(strings.TrimSuffix(rule, ":*"))}
	case strings.Contains(rule, "*"):
		return excludedPattern{kind: "wildcard", value: rule}
	default:
		return excludedPattern{kind: "exact", value: rule}
	}
}

func (p excludedPattern) matches(cmd string) bool {
	switch p.kind {
	case "prefix":
		return p.value != "" && (cmd == p.value || strings.HasPrefix(cmd, p.value+" "))
	case "wildcard":
		return wildcardMatch(p.value, cmd)
	default:
		return cmd == p.value
	}
}

// wildcardMatch is the rule glob: "*" matches any run of characters.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// ContainsExcludedCommand reports whether command is taken out of the sandbox
// by sandbox.excludedCommands: EVERY subcommand of a compound command must
// match an exclusion (so `excluded && other` stays sandboxed), and a command
// the splitter cannot reason about (command or process substitution, a
// variable assigned in one subcommand and expanded in another) stays
// sandboxed. Exclusion is a convenience, not a security boundary: an excluded
// command still goes through the permission flow.
func ContainsExcludedCommand(command string, excluded []string) bool {
	if len(excluded) == 0 || strings.TrimSpace(command) == "" {
		return false
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "`") ||
		strings.Contains(command, "<(") || strings.Contains(command, ">(") {
		return false
	}
	// Hardening: constructs the splitter does not model keep the
	// command sandboxed — ANSI-C / locale quoting ($'…' / $"…", whose escapes
	// can hide a quote from the splitter), here-documents, line
	// continuations, and output redirection (an excluded command must not
	// become an unsandboxed write to an arbitrary file).
	for _, bad := range []string{"$'", `$"`, "<<", "\\\n", ">", "\x00"} {
		if strings.Contains(command, bad) {
			return false
		}
	}
	subs, ok := splitSubcommands(command)
	if !ok || len(subs) == 0 {
		return false
	}
	for _, sub := range subs {
		if !plainSimpleCommand(sub) {
			return false
		}
	}
	if crossSubcommandAssignment(subs) {
		return false
	}
	pats := make([]excludedPattern, 0, len(excluded))
	for _, e := range excluded {
		pats = append(pats, parseExcluded(e))
	}
	for _, sub := range subs {
		sub = strings.TrimSpace(sub)
		matched := false
		for _, p := range pats {
			if p.matches(sub) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// splitSubcommands splits a command on the control operators ;, &&, ||, |, &
// and newlines outside quotes. It reports false for unbalanced quotes.
func splitSubcommands(cmd string) ([]string, bool) {
	var subs []string
	var cur strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			subs = append(subs, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			escaped = false
			cur.WriteByte(c)
		case c == '\\' && !inSingle:
			escaped = true
			cur.WriteByte(c)
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case inSingle || inDouble:
			cur.WriteByte(c)
		case c == ';' || c == '\n' || c == '|' || c == '&':
			flush()
			if (c == '|' || c == '&') && i+1 < len(cmd) && cmd[i+1] == c {
				i++
			}
		case c == '<' || c == '>':
			// A redirection keeps the subcommand but is never an exact match.
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble || escaped {
		return nil, false
	}
	flush()
	return subs, true
}

// crossSubcommandAssignment reports a variable assigned in one subcommand and
// expanded in another (`X=evil; excluded $X`), which defeats matching the
// subcommands one by one.
func crossSubcommandAssignment(subs []string) bool {
	assigned := map[string]int{}
	for i, s := range subs {
		f := strings.Fields(s)
		for _, w := range f {
			eq := strings.Index(w, "=")
			if eq <= 0 {
				break
			}
			name := w[:eq]
			if !isIdent(name) {
				break
			}
			assigned[name] = i
		}
	}
	for name, at := range assigned {
		for i, s := range subs {
			if i == at {
				continue
			}
			if strings.Contains(s, "$"+name) || strings.Contains(s, "${"+name) {
				return true
			}
		}
	}
	return false
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// plainSimpleCommand reports a subcommand that starts with a command word:
// not an environment assignment (`PATH=/evil npm test` runs a different
// npm), a subshell or group (`( … )`, `{ …; }`), a negation, or a keyword
// that introduces a compound command.
func plainSimpleCommand(sub string) bool {
	f := strings.Fields(sub)
	if len(f) == 0 {
		return false
	}
	first := f[0]
	if eq := strings.Index(first, "="); eq > 0 && isIdent(strings.TrimSuffix(first[:eq], "+")) {
		return false
	}
	if strings.ContainsAny(first[:1], "({!") {
		return false
	}
	switch first {
	case "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac",
		"function", "select", "coproc", "[[", "]]", "}":
		return false
	}
	return true
}
