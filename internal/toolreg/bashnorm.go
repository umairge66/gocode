package toolreg

import (
	"regexp"
	"strings"
)

// Bash command normalization for DENY and ASK rule matching.
//
// A Bash deny / ask rule is matched not only against the command as
// written but against every "real command" spelling it can be stripped to
// (with all env vars stripped for the deny and ask lists):
//
//   - stripCommandWrappers: drop comment lines, leading SAFE env assignments
//     (safeEnvVars), the
//     wrappers timeout / time / nice / stdbuf / nohup / command / builtin /
//     noglob, and the quoting of the first word;
//   - stripAllEnv: drop ANY leading env assignment (VAR=x, VAR+=x, A[i]=x, quoted);
//   - stripWrappers: drop env assignments and the wrapper commands of the
//     wrapperValueFlags table
//     (env, sudo, doas, pkexec, watch, ionice, setsid, taskset, chrt, strace,
//     ltrace, flock, script, unshare, nsenter, exec, command, builtin, noglob,
//     nocorrect) with their value-taking options, following a split-string
//     option (env -S, flock -c, script -c) into the command it carries;
//   - a prefix / wildcard rule also matches "xargs <rule>".
//
// The inner script of `sh -c '…'` / `bash -c "…"` is unwrapped and judged
// as a command of its own.
//
// Further normalizations, all on the deny / ask side only (they can only make
// a deny or ask rule match more often): a path-qualified command word is also
// tried by its base name (`/bin/rm` → `rm`), the remaining wrappers
// (busybox, toybox, eval, xargs with options, su / runuser -c) are unwrapped,
// and every word is de-quoted.
//
// Allow rules are NOT normalized: an allow rule still covers only the command
// as written (and never one with shell operators).

// safeEnvVars are the env assignments stripCommandWrappers strips (values that only tune
// output / locale, never what runs).
var safeEnvVars = map[string]bool{
	"GOEXPERIMENT": true, "GOOS": true, "GOARCH": true, "CGO_ENABLED": true, "GO111MODULE": true,
	"RUST_BACKTRACE": true, "RUST_LOG": true, "NODE_ENV": true, "PYTHONUNBUFFERED": true,
	"PYTHONDONTWRITEBYTECODE": true, "PYTEST_DISABLE_PLUGIN_AUTOLOAD": true, "PYTEST_DEBUG": true,
	"LANG": true, "LANGUAGE": true, "LC_ALL": true, "LC_CTYPE": true,
	"LC_TIME": true, "CHARSET": true, "TERM": true, "COLORTERM": true, "NO_COLOR": true,
	"FORCE_COLOR": true, "TZ": true, "LS_COLORS": true, "LSCOLORS": true, "GREP_COLOR": true,
	"GREP_COLORS": true, "GCC_COLORS": true, "TIME_STYLE": true, "BLOCK_SIZE": true,
	"BLOCKSIZE": true, "COLUMNS": true, "LINES": true, "CLICOLOR": true, "CLICOLOR_FORCE": true,
	"CI": true, "DEBIAN_FRONTEND": true, "GIT_TERMINAL_PROMPT": true,
}

// commandWrapperRes are stripCommandWrappers's wrapper-stripping regexps.
var commandWrapperRes = []*regexp.Regexp{
	regexp.MustCompile(`^timeout[ \t]+(?:(?:--(?:foreground|preserve-status|verbose)|--(?:kill-after|signal)=[A-Za-z0-9_.+-]+|--(?:kill-after|signal)[ \t]+[A-Za-z0-9_.+-]+|-v|-[ks][ \t]+[A-Za-z0-9_.+-]+|-[ks][A-Za-z0-9_.+-]+)[ \t]+)*(?:--[ \t]+)?\d+(?:\.\d+)?[smhd]?[ \t]+`),
	regexp.MustCompile(`^time[ \t]+(?:--[ \t]+)?`),
	regexp.MustCompile(`^nice(?:[ \t]+-n[ \t]+-?\d+|[ \t]+-\d+)?[ \t]+(?:--[ \t]+)?`),
	regexp.MustCompile(`^stdbuf(?:[ \t]+-[ioe][LN0-9]+)+[ \t]+(?:--[ \t]+)?`),
	regexp.MustCompile(`^nohup[ \t]+(?:--[ \t]+)?`),
	// The intended pattern is /^command(?:[ \t]+-p+)*(?:[ \t]+--)?[ \t]+(?!-)/;
	// Go has no lookahead, so the "(?!-)" is checked after the match.
	regexp.MustCompile(`^command(?:[ \t]+-p+)*(?:[ \t]+--)?[ \t]+`),
	regexp.MustCompile(`^builtin(?:[ \t]+--)?[ \t]+`),
	regexp.MustCompile(`^noglob[ \t]+`),
}

// safeEnvAssignRe is stripCommandWrappers's env-assignment regexp.
var safeEnvAssignRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=([A-Za-z0-9_./:-]+)[ \t]+`)

// anyEnvAssignRe is stripAllEnv's env-assignment regexp (any variable, quoted values).
var anyEnvAssignRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*(?:\[[^\]]*\])?)\+?=(?:'[^'\n\r]*'|"(?:\\.|[^"$` + "`" + `\\\n\r])*"|\\.|[^ \t\n\r$` + "`" + `;|&()<>\\'"])*[ \t]+`)

// stripCommentLines drops lines starting with "#" (unless every line is
// a comment).
func stripCommentLines(s string) string {
	lines := strings.Split(s, "\n")
	var kept []string
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return s
	}
	return strings.Join(kept, "\n")
}

// dequoteFirstWord removes the quoting of the first word, leaving
// the command unchanged when the quoting is unbalanced.
func dequoteFirstWord(s string) string {
	i := strings.IndexAny(s, " \t\n\r\f\v")
	word, rest := s, ""
	if i >= 0 {
		word, rest = s[:i], s[i:]
	}
	out, ok := dequoteWord(word)
	if !ok {
		return s
	}
	return out + rest
}

// dequoteWord removes shell quoting from one word: '…' literally, "…" with
// \$ \` \" \\ escapes, and a bare backslash escape. ok is false when a quote
// or escape is left open.
func dequoteWord(w string) (string, bool) {
	var b strings.Builder
	var q byte
	for i := 0; i < len(w); i++ {
		c := w[i]
		switch q {
		case '\'':
			if c == '\'' {
				q = 0
			} else {
				b.WriteByte(c)
			}
		case '"':
			if c == '\\' {
				if i+1 >= len(w) {
					return w, false
				}
				n := w[i+1]
				if n == '$' || n == '`' || n == '"' || n == '\\' {
					b.WriteByte(n)
					i++
				} else {
					b.WriteByte(c)
				}
			} else if c == '"' {
				q = 0
			} else {
				b.WriteByte(c)
			}
		default:
			switch c {
			case '\\':
				if i+1 >= len(w) {
					return w, false
				}
				b.WriteByte(w[i+1])
				i++
			case '"', '\'':
				q = c
			default:
				b.WriteByte(c)
			}
		}
	}
	if q != 0 {
		return w, false
	}
	return b.String(), true
}

// stripCommandWrappers strips safe env assignments, comment lines and simple
// wrappers, and de-quotes the first word.
func stripCommandWrappers(e string) string {
	s, prev := e, ""
	for s != prev {
		prev = s
		s = stripCommentLines(s)
		if m := safeEnvAssignRe.FindStringSubmatch(s); m != nil && safeEnvVars[m[1]] {
			s = s[len(m[0]):]
		}
	}
	s = dequoteFirstWord(stripCommentLines(s))
	prev = ""
	for s != prev {
		prev = s
		for i, re := range commandWrapperRes {
			loc := re.FindStringIndex(s)
			if loc == nil {
				continue
			}
			if i >= 5 && loc[1] < len(s) && s[loc[1]] == '-' { // command / builtin / noglob: (?!-)
				continue
			}
			s = s[loc[1]:]
		}
		if s != prev {
			s = dequoteFirstWord(stripCommentLines(s))
		}
	}
	return strings.TrimSpace(s)
}

// stripAllEnv drops every leading env assignment.
func stripAllEnv(e string) string {
	s, prev := e, ""
	for s != prev {
		prev = s
		s = stripCommentLines(s)
		if loc := anyEnvAssignRe.FindStringIndex(s); loc != nil {
			s = s[loc[1]:]
		}
	}
	return strings.TrimSpace(s)
}

// wrapperValueFlags are the wrapper commands stripWrappers strips, each with the
// options that take a separate value.
var wrapperValueFlags = map[string][]string{
	"env":       {"-u", "-C", "--unset", "--chdir"},
	"sudo":      {"-u", "-g", "-U", "-C", "-D", "-h", "-p", "-r", "-R", "-t", "-T", "--user", "--group", "--other-user", "--close-from", "--chdir", "--host", "--prompt", "--role", "--chroot", "--type", "--command-timeout", "-a", "--auth-type"},
	"doas":      {"-a", "-u", "-C"},
	"pkexec":    {"--user"},
	"watch":     {"-n", "--interval", "--equexit"},
	"ionice":    {"-c", "-n", "-p", "-P", "-u", "--class", "--classdata", "--pid", "--pgid", "--uid"},
	"setsid":    {},
	"taskset":   {"-c", "--cpu-list"},
	"chrt":      {"-p", "--pid", "-T", "-P", "-D", "--sched-runtime", "--sched-period", "--sched-deadline"},
	"strace":    {"-e", "-o", "-p", "-s", "-E", "-P", "-S", "-a", "-b", "-I", "-u", "-X", "-O", "-U", "--output", "--trace", "--expr", "--attach", "--string-limit", "--env", "--trace-path", "--columns", "--user", "--interruptible", "--detach-on", "--const-print-style", "--summary-sort-by", "--summary-syscall-overhead", "--summary-columns"},
	"ltrace":    {"-a", "-A", "-e", "-l", "-n", "-o", "-p", "-s", "-u", "-x", "-D", "-F", "--align", "--config", "--debug", "--indent", "--library", "--output", "--string-max", "-w", "--where"},
	"flock":     {"-w", "-E", "--timeout", "--wait", "--conflict-exit-code"},
	"script":    {"-E", "-T", "-m", "-o", "-O", "-B", "-I", "--echo", "--log-timing", "--logging-format", "--output-limit", "--log-out", "--log-io", "--log-in"},
	"unshare":   {"-R", "-w", "-S", "-G", "--setuid", "--setgid", "--root", "--wd", "--propagation", "--setgroups", "--monotonic", "--boottime"},
	"nsenter":   {"-t", "-S", "-G", "--target", "--setuid", "--setgid"},
	"exec":      {"-a"},
	"command":   {},
	"builtin":   {},
	"noglob":    {},
	"nocorrect": {},
}

// wrapperSplitFlags are options whose value is itself a command.
var wrapperSplitFlags = map[string][]string{
	"env":    {"-S", "--split-string"},
	"flock":  {"-c", "--command"},
	"script": {"-c", "--command"},
}

// wrapperPositional is a wrapper's leading positional operand that is
// not the command (chrt's priority, taskset's mask, flock's lock file,
// script's typescript file).
var wrapperPositional = map[string]func(string) bool{
	"chrt":    regexp.MustCompile(`^\d+$`).MatchString,
	"taskset": regexp.MustCompile(`(?i)^(0x[\da-f]+|\d+)$`).MatchString,
	"flock":   func(string) bool { return true },
	"script":  func(string) bool { return true },
}

var envAssignWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)
var timeoutDuration = regexp.MustCompile(`^\d+(?:\.\d+)?[smhd]?$`)
var timeoutArg = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
var timeoutLongEq = regexp.MustCompile(`^--(?:kill-after|signal)=[A-Za-z0-9_.+-]+$`)
var timeoutShortJoined = regexp.MustCompile(`^-[ks][A-Za-z0-9_.+-]+$`)
var niceValue = regexp.MustCompile(`^-?\d+$`)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// timeoutCommandIndex returns the index of timeout's duration operand, or -1.
func timeoutCommandIndex(n []string) int {
	i := 1
	for i < len(n) {
		r := n[i]
		s := ""
		if i+1 < len(n) {
			s = n[i+1]
		}
		switch {
		case r == "--foreground" || r == "--preserve-status" || r == "--verbose":
			i++
		case timeoutLongEq.MatchString(r):
			i++
		case (r == "--kill-after" || r == "--signal") && s != "" && timeoutArg.MatchString(s):
			i += 2
		case r == "--":
			return i + 1
		case strings.HasPrefix(r, "--"):
			return -1
		case r == "-v":
			i++
		case (r == "-k" || r == "-s") && s != "" && timeoutArg.MatchString(s):
			i += 2
		case timeoutShortJoined.MatchString(r):
			i++
		case strings.HasPrefix(r, "-"):
			return -1
		default:
			return i
		}
	}
	return i
}

// stripTimeNohup strips time / nohup / timeout / nice -n.
func stripTimeNohup(n []string) []string {
	for {
		switch {
		case len(n) > 0 && (n[0] == "time" || n[0] == "nohup"):
			if len(n) > 1 && n[1] == "--" {
				n = n[2:]
			} else {
				n = n[1:]
			}
		case len(n) > 0 && n[0] == "timeout":
			r := timeoutCommandIndex(n)
			if r < 0 || r >= len(n) || !timeoutDuration.MatchString(n[r]) {
				return n
			}
			n = n[r+1:]
		case len(n) > 2 && n[0] == "nice" && n[1] == "-n" && niceValue.MatchString(n[2]):
			if len(n) > 3 && n[3] == "--" {
				n = n[4:]
			} else {
				n = n[3:]
			}
		default:
			return n
		}
	}
}

// stripWrappers drops env assignments and wrapperValueFlags wrappers (with their
// options) until the real command word.
func stripWrappers(e []string) []string {
	n := append([]string(nil), e...)
	for {
		for len(n) > 0 && envAssignWord.MatchString(n[0]) {
			n = n[1:]
		}
		n = stripTimeNohup(n)
		if len(n) == 0 {
			return n
		}
		r := n[0]
		valued, ok := wrapperValueFlags[r]
		if !ok {
			return n
		}
		split := wrapperSplitFlags[r]
		positional := wrapperPositional[r]
		i, inner, tookPos := 1, "", false
		for i < len(n) {
			q := n[i]
			if q == "--" {
				i++
				if !tookPos && positional != nil && i+1 < len(n) && positional(n[i]) {
					tookPos = true
					i++
					continue
				}
				break
			}
			if split != nil {
				if contains(split, q) && i+1 < len(n) {
					if v := strings.TrimSpace(n[i+1]); v != "" {
						inner = v
						break
					}
					i += 2
					continue
				}
				if k := strings.Index(q, "="); k > 0 && contains(split, q[:k]) {
					if v := strings.TrimSpace(q[k+1:]); v != "" {
						inner = v
						break
					}
					i++
					continue
				}
				if len(q) > 2 && q[1] != '-' && contains(split, q[:2]) {
					if v := strings.TrimSpace(q[2:]); v != "" {
						inner = v
						break
					}
					i++
					continue
				}
			}
			if strings.HasPrefix(q, "-") && (q != "-" || positional == nil) {
				if r == "command" && (q == "-v" || q == "-V") {
					return n
				}
				if contains(valued, q) && i+1 < len(n) {
					i += 2
				} else {
					i++
				}
				continue
			}
			if r == "env" && envAssignWord.MatchString(q) {
				i++
				continue
			}
			if !tookPos && positional != nil && positional(q) && i+1 < len(n) {
				tookPos = true
				i++
				continue
			}
			break
		}
		if inner != "" {
			n = strings.Fields(inner)
			if len(n) == 0 {
				return append([]string(nil), e...)
			}
			continue
		}
		if i >= len(n) {
			return n
		}
		n = n[i:]
	}
}

// shellNames are the shells whose -c operand is a script.
var shellNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "hush": true, "rbash": true}

// xargsValueFlags are xargs options that take a separate value.
var xargsValueFlags = []string{"-I", "-L", "-n", "-P", "-s", "-d", "-E", "-a", "--max-args", "--max-procs", "--max-chars", "--delimiter", "--arg-file", "--max-lines", "--replace", "--eof"}

// shellWords splits a command into words, honouring quotes and backslash
// escapes, and removes the quoting. ok is false on an unbalanced quote.
func shellWords(s string) ([]string, bool) {
	var out []string
	var b strings.Builder
	in := false
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch q {
		case '\'':
			if c == '\'' {
				q = 0
			} else {
				b.WriteByte(c)
			}
			continue
		case '"':
			if c == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
				b.WriteByte(s[i+1])
				i++
			} else if c == '"' {
				q = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			if in {
				out = append(out, b.String())
				b.Reset()
				in = false
			}
		case '\\':
			in = true
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i++
			}
		case '\'', '"':
			in = true
			q = c
		default:
			in = true
			b.WriteByte(c)
		}
	}
	if q != 0 {
		return nil, false
	}
	if in {
		out = append(out, b.String())
	}
	return out, true
}

// baseName strips a directory from a command word (`/usr/bin/rm` → `rm`).
func baseName(w string) string {
	if i := strings.LastIndexAny(w, `/\`); i >= 0 && i+1 < len(w) {
		return w[i+1:]
	}
	return w
}

// unwrapArgv strips one layer of the extra wrappers, a path-qualified command
// word and a shell -c script. It returns the remaining argv, the inner script
// of a shell -c (to be judged as a command of its own), and whether anything
// changed.
func unwrapArgv(n []string) (rest []string, script string, changed bool) {
	if len(n) == 0 {
		return n, "", false
	}
	if b := baseName(n[0]); b != n[0] {
		n = append([]string{b}, n[1:]...)
		changed = true
	}
	w := n[0]
	switch {
	case w == "su" || w == "runuser":
		sawUser := false
		for i := 1; i < len(n); i++ {
			a := n[i]
			switch {
			case a == "-c" || a == "--command":
				if i+1 < len(n) {
					return nil, n[i+1], true
				}
				return n, "", changed
			case strings.HasPrefix(a, "--command="):
				return nil, strings.TrimPrefix(a, "--command="), true
			case a == "--":
				if i+1 < len(n) {
					return n[i+1:], "", true
				}
				return n, "", changed
			case a == "-u" || a == "-g" || a == "-G" || a == "--user" || a == "--group" || a == "-s" || a == "--shell":
				sawUser = sawUser || a == "-u" || a == "--user"
				i++
			case strings.HasPrefix(a, "-"):
			default:
				if w == "runuser" && sawUser {
					return n[i:], "", true
				}
			}
		}
	case shellNames[w]:
		for i := 1; i < len(n); i++ {
			a := n[i]
			if a == "--" || !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
				break
			}
			if a == "--command" || (!strings.HasPrefix(a, "--") && strings.Contains(a[1:], "c")) {
				if i+1 < len(n) {
					return nil, n[i+1], true
				}
				break
			}
		}
	case w == "busybox" || w == "toybox":
		if len(n) > 1 {
			return n[1:], "", true
		}
	case w == "eval":
		if len(n) > 1 {
			return nil, strings.Join(n[1:], " "), true
		}
	case w == "xargs":
		i := 1
		for i < len(n) && strings.HasPrefix(n[i], "-") {
			if n[i] == "--" {
				i++
				break
			}
			if contains(xargsValueFlags, n[i]) {
				i++
			}
			i++
		}
		if i < len(n) {
			return n[i:], "", true
		}
	}
	return n, "", changed
}

// maxBashCandidates bounds the candidate spellings of one command.
const maxBashCandidates = 64

// bashRuleCandidates returns every spelling a deny / ask rule is matched
// against for cmd: the command, its operator-separated segments, and for each
// the stripCommandWrappers / stripAllEnv / stripWrappers normalizations, the extra-wrapper and shell -c unwraps
// and the de-quoted word form (see the file comment).
func bashRuleCandidates(cmd string) []string {
	seen := map[string]bool{}
	var out []string
	var queue []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(out) >= maxBashCandidates {
			return
		}
		seen[s] = true
		out = append(out, s)
		queue = append(queue, s)
	}
	addCommand := func(c string) {
		add(c)
		for _, seg := range strings.FieldsFunc(c, func(r rune) bool { return strings.ContainsRune(bashShellOperators, r) }) {
			add(seg)
		}
	}
	addCommand(cmd)
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		add(stripCommandWrappers(c))
		add(stripAllEnv(c))
		words, ok := shellWords(c)
		if !ok || len(words) == 0 {
			continue
		}
		add(strings.Join(words, " "))
		argvs := [][]string{words}
		if s := stripWrappers(words); len(s) > 0 {
			add(strings.Join(s, " "))
			argvs = append(argvs, s)
		}
		// Unwrap on the word lists (not their re-joined text) so a shell -c
		// script keeps its quoting.
		for depth := 0; depth < 8 && len(argvs) > 0; depth++ {
			var next [][]string
			for _, a := range argvs {
				rest, script, changed := unwrapArgv(a)
				if !changed {
					continue
				}
				if script != "" {
					addCommand(script)
					continue
				}
				if len(rest) > 0 {
					add(strings.Join(rest, " "))
					next = append(next, rest)
					if s := stripWrappers(rest); len(s) > 0 {
						add(strings.Join(s, " "))
						next = append(next, s)
					}
				}
			}
			argvs = next
		}
	}
	return out
}
