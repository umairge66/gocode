package toolreg

import (
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"
)

// Read-only Bash command classification.
//
// The Bash tool computes isReadOnly(input) per command: the command is
// parsed, and it is read-only only
// when it is a list of simple commands (joined by ;, &&, || or |) each of
// which is provably read-only:
//
//   - no subshell / compound statement / background `&`, no command or
//     process substitution, no unquoted variable expansion;
//   - output redirections only to /dev/null or fd duplication (2>&1);
//   - env-var prefixes only from the safe list (safeEnvVars);
//   - a command with an unquoted glob only from globSafeCommands (ls, cat,
//     grep, …);
//   - then each command must pass the simple allowlist (simpleReadOnly: cat,
//     head, ls, find without -exec/-delete/-fprint…, echo, printf, cd, …) or
//     the flag-allowlist table (readOnlyByFlags: git log/diff/status/…, sed,
//     sort, grep, rg, xargs, ps, date, …) or one of the whole-command regexes
//     (uniq, jq).
//
// Anything the classifier cannot prove read-only is NOT read-only.
//
// Parsing uses a small conservative lexer (not a full shell grammar) that
// accepts only the shapes the read-only path
// can ever accept and refuses everything else (heredocs, here-strings,
// `$(…)`, backticks, `${…}`, `$VAR`, subshells, braces, `&`, `|&`, process
// substitution, zsh `=cmd`, `~[`, `<N-M>`, control / Unicode-space
// characters, backslash-escaped whitespace and line continuations).

// maxReadOnlyCommandLen bounds analysis: a longer command is never analysed.
const maxReadOnlyCommandLen = 10000

// roCommand is one entry of the read-only flag table.
type roCommand struct {
	name         string
	safeFlags    map[string]string
	noDoubleDash bool   // respectsDoubleDash:false
	regex        string // whole-command regex (hostname)
}

// shRedirect is one redirection of a simple command.
type shRedirect struct {
	op     string
	target string
}

// shCommand is one simple command of a parsed list.
type shCommand struct {
	argv      []string // de-quoted words after the env prefix
	envNames  []string // names of the leading VAR=value assignments
	redirects []shRedirect
	text      string // the raw words (env prefix + argv), space-joined
}

// IsReadOnlyBashCommand reports whether cmd is provably read-only (the Bash
// tool's isReadOnly). It is conservative: any
// command it cannot prove read-only answers false.
func IsReadOnlyBashCommand(cmd string) bool {
	_, ok := analyzeReadOnlyBash(cmd)
	return ok
}

// analyzeReadOnlyBash returns the parsed commands and whether the
// whole command is read-only.
func analyzeReadOnlyBash(cmd string) ([]shCommand, bool) {
	if len(cmd) > maxReadOnlyCommandLen {
		return nil, false // "Command too long for read-only analysis"
	}
	cmds, bare, ok := parseShellList(cmd)
	if !ok || len(cmds) == 0 {
		return nil, false // too-complex / subshell / `&` / empty
	}
	// Bare assignments (`X=1; ls`) to a non-allowlisted variable can alter the
	// behavior of later commands. There is no session env-var key set, so
	// the strict rule applies: any non-safe name refuses.
	for _, name := range bare {
		if name == "PWD" || name == "OLDPWD" || !safeEnvVars[name] {
			return nil, false
		}
	}
	whole := expansionKind(cmd)
	if whole == "variable" {
		return nil, false
	}
	if windowsUNC(cmd) {
		return nil, false
	}
	// Compound commands with cd and git require permission checks.
	hasGit, hasCd := false, false
	for _, c := range cmds {
		if isGitCommandText(c) {
			hasGit = true
		}
		if isCdCommand(c) {
			hasCd = true
		}
	}
	if hasGit && hasCd {
		return nil, false
	}
	for _, c := range cmds {
		if !readOnlySimpleCommand(c, whole) {
			return cmds, false
		}
	}
	return cmds, true
}

// readOnlyInputOps are the redirections always accepted as read-only.
var readOnlyInputOps = map[string]bool{"<": true, "<<": true, "<&": true, "<<<": true}

var digitsRe = regexp.MustCompile(`^\d+$`)

// readOnlySimpleCommand judges one simple command of the list.
func readOnlySimpleCommand(c shCommand, whole string) bool {
	for _, r := range c.redirects {
		if !readOnlyInputOps[r.op] && r.target != "/dev/null" && !(r.op == ">&" && digitsRe.MatchString(r.target)) {
			return false
		}
		if strings.HasPrefix(r.target, "/dev/tcp/") || strings.HasPrefix(r.target, "/dev/udp/") {
			return false
		}
		if r.op == "<" && (windowsUNC(r.target) || tildeUser(r.target)) {
			return false
		}
	}
	for _, n := range c.envNames {
		if !safeEnvVars[n] {
			return false
		}
	}
	for i, a := range c.argv {
		if windowsUNC(a) {
			return false
		}
		if i > 0 && tildeUser(a) {
			return false
		}
	}
	ve := stripCommandBuiltin(c.argv)
	if len(ve) == 0 {
		return false
	}
	if expansionKind(c.text) == "glob" || (whole == "glob" && anyArgGlob(c.argv)) {
		return globSafeCommands[ve[0]]
	}
	if v, decided := simpleReadOnly(ve); decided {
		return v
	}
	return readOnlyByTable(stripCommandPrefixText(c.text), ve)
}

var argGlobRe = regexp.MustCompile(`[*?]|\[.*\]`)

func anyArgGlob(argv []string) bool {
	for _, a := range argv {
		if argGlobRe.MatchString(a) {
			return true
		}
	}
	return false
}

// tildeUser reports a `~user` path (not `~` or `~/…`), which cannot be resolved.
func tildeUser(s string) bool {
	return strings.HasPrefix(s, "~") && len(s) > 1 && s[1] != '/'
}

// windowsUNC reports a UNC / WebDAV path, which on Windows could reach the
// network.
func windowsUNC(s string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return strings.Contains(s, `\\`) || strings.Contains(s, "//") || strings.Contains(strings.ToLower(s), "davwwwroot")
}

// globSafeCommands are the commands that stay read-only with an
// unquoted glob.
var globSafeCommands = setOf("ls", "cat", "head", "tail", "wc", "stat", "grep", "egrep", "fgrep", "diff", "du", "df", "echo", "strings", "hexdump", "od", "nl", "cut", "column", "tr", "tac", "rev", "cmp", "basename", "dirname", "realpath", "readlink", "sha256sum", "sha1sum", "md5sum", "cd")

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// stripCommandBuiltin drops leading `command [-p] [--]`, `builtin
// [--]` and `noglob`.
func stripCommandBuiltin(e []string) []string {
	n := e
	for {
		switch {
		case len(n) > 0 && n[0] == "command":
			r := 1
			for r < len(n) && dashPRe.MatchString(n[r]) {
				r++
			}
			if r < len(n) && n[r] == "--" {
				r++
			}
			if r >= len(n) || strings.HasPrefix(n[r], "-") {
				return n
			}
			n = n[r:]
		case len(n) > 0 && n[0] == "builtin":
			r := 1
			if len(n) > 1 && n[1] == "--" {
				r = 2
			}
			if r >= len(n) {
				return n
			}
			n = n[r:]
		case len(n) > 0 && n[0] == "noglob":
			if len(n) <= 1 {
				return n
			}
			n = n[1:]
		default:
			return n
		}
	}
}

var dashPRe = regexp.MustCompile(`^-p+$`)

// commandPrefixRe is the text form of stripCommandBuiltin. Go has no lookahead;
// the "(?!-)" is checked after the match.
var commandPrefixRe = regexp.MustCompile(`^(?:(?:command(?:[ \t]+-p+)*|builtin)(?:[ \t]+--)?|noglob)[ \t]+`)

// stripCommandPrefixText strips commandPrefixRe prefixes from command text.
func stripCommandPrefixText(e string) string {
	for {
		loc := commandPrefixRe.FindStringIndex(e)
		if loc == nil || (loc[1] < len(e) && e[loc[1]] == '-') {
			return e
		}
		e = e[loc[1]:]
	}
}

// isGitCommandText reports whether a parsed command is a git command.
func isGitCommandText(c shCommand) bool {
	if strings.HasPrefix(c.text, "git ") || c.text == "git" {
		return true
	}
	w, _ := shellWords(stripCommandWrappers(c.text))
	if len(w) > 0 && w[0] == "git" {
		return true
	}
	if len(w) > 0 && w[0] == "xargs" {
		for _, x := range w {
			if x == "git" {
				return true
			}
		}
	}
	return false
}

// isCdCommand reports whether a parsed command is cd.
func isCdCommand(c shCommand) bool {
	w, _ := shellWords(stripCommandWrappers(c.text))
	if len(w) == 0 {
		return false
	}
	switch w[0] {
	case "cd", "pushd", "popd", "chdir":
		return true
	}
	return false
}

// expansionKind returns "variable" when the text has an unquoted (or double-quoted)
// `$name` expansion, "glob" when it has an unquoted glob, else "".
func expansionKind(e string) string {
	sq, dq, bt, esc, glob, br := false, false, false, false, false, false
	wordStart := true
	for i := 0; i < len(e); i++ {
		q := e[i]
		if esc {
			esc = false
			wordStart = false
			continue
		}
		if q == '\\' && !sq {
			if i+1 < len(e) && e[i+1] == '\n' {
				i++
				continue
			}
			esc = true
			continue
		}
		if bt {
			if q == '`' {
				bt = false
				wordStart = false
			}
			continue
		}
		if q == '`' && !sq {
			bt = true
			wordStart = false
			continue
		}
		if q == '#' && wordStart && !sq && !dq {
			for i < len(e) && e[i] != '\n' {
				i++
			}
			wordStart = true
			continue
		}
		if q == '\'' && !dq {
			sq = !sq
			wordStart = false
			continue
		}
		if q == '"' && !sq {
			dq = !dq
			wordStart = false
			continue
		}
		if sq {
			continue
		}
		if q == '$' && i+1 < len(e) && strings.IndexByte("@*#?!$0123456789-", e[i+1]) >= 0 || q == '$' && i+1 < len(e) && isIdentStart(e[i+1]) {
			return "variable"
		}
		if dq {
			continue
		}
		switch q {
		case ' ', '\t', '\n', '|', '&', ';', '(', ')', '<', '>':
			br = false
			wordStart = true
			continue
		}
		wordStart = false
		switch q {
		case '?', '*':
			glob = true
		case '[':
			br = true
		case ']':
			if br {
				glob = true
			}
		}
	}
	if glob {
		return "glob"
	}
	return ""
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// --- the simple allowlist ------------------------------------------------

// simpleReadOnlyCommands are always read-only (multi-word docker entries
// included).
var simpleReadOnlyCommands = []string{"docker ps", "docker images", "cal", "uptime", "cat", "head", "tail", "wc", "stat", "strings", "hexdump", "od", "nl", "id", "uname", "free", "df", "du", "locale", "groups", "nproc", "basename", "dirname", "realpath", "cut", "paste", "tr", "column", "tac", "rev", "fold", "expand", "unexpand", "fmt", "comm", "cmp", "numfmt", "readlink", "diff", "true", "false", "sleep", "which", "type", "expr", "seq", "tsort", "pr"}

var simpleReadOnlySet = setOf(simpleReadOnlyCommands...)

// findDangerous are find actions that execute, delete or write.
var findDangerous = setOf("-delete", "-rm", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fls", "-fprintf", "-files0-from")

// findValued are the find primaries that take a value, plus the -newerXY
// and option-value forms.
var (
	findValued   = setOf("-name", "-iname", "-path", "-ipath", "-lname", "-ilname", "-regex", "-iregex", "-wholename", "-iwholename", "-samefile", "-newer", "-anewer", "-cnewer", "-mnewer", "-perm", "-user", "-group", "-uid", "-gid", "-size", "-type", "-xtype", "-fstype", "-inum", "-links", "-used", "-context", "-amin", "-cmin", "-mmin", "-atime", "-ctime", "-mtime", "-mindepth", "-maxdepth", "-printf", "-regextype", "-D", "-f", "-flags", "-Bnewer", "-Btime", "-Bmin", "-files0-from", "-xattrname")
	findNewerXY  = regexp.MustCompile(`^-newer[aBcm][aBcmt]$`)
	findOptValue = regexp.MustCompile(`^-(?:[EHLPXdsx]*D|f|[dsx]*[EHLPX][EHLPXdsx]*f)$`)
	findFilesArg = regexp.MustCompile(`^-[dsx]+f$`)
)

// noArgReadOnly allows no arguments; exactArgvRO matches an exact argv.
var (
	noArgReadOnly = setOf("pwd", "whoami", "alias")
	exactArgvRO   = [][]string{{"gocode", "-h"}, {"gocode", "--help"}, {"node", "-v"}, {"node", "--version"}, {"python", "--version"}, {"python3", "--version"}, {"ip", "addr"}}
)

var (
	printfOctalHex = regexp.MustCompile(`%[^%a-zA-Z]*[lLhqjzZt]*\\[0-7xX]`)
	printfConv     = regexp.MustCompile(`%[-+ 0#']*[0-9.*]*[lLhqjzZt]*[diouxXeEfFgGaAn]`)
	printfStar     = regexp.MustCompile(`%[^%a-zA-Z]*\*`)
	printfUnicode  = regexp.MustCompile(`\\[uU]`)
	printfNumber   = regexp.MustCompile(`^[-+]?(0[xX][0-9a-fA-F]+|[0-9]+#[0-9a-zA-Z]+|[0-9]*\.?[0-9]+([eE][-+]?[0-9]+)?)$`)
	testNumber     = regexp.MustCompile(`^-?(0[xX][0-9a-fA-F]+|[0-9]+#[0-9a-zA-Z]+|[0-9]+)$`)
	testCompare    = setOf("-eq", "-ne", "-lt", "-le", "-gt", "-ge")
	ifconfigArg    = regexp.MustCompile(`^[a-zA-Z]`)
)

// simpleReadOnly is the simple allowlist. decided is false when it has no
// verdict (the table and regexes then decide).
func simpleReadOnly(e []string) (readOnly, decided bool) {
	if len(e) == 0 {
		return false, true
	}
	n := e[0]
	if noArgReadOnly[n] {
		return len(e) == 1, true
	}
	for _, s := range exactArgvRO {
		if equalStrings(e, s) {
			return true, true
		}
	}
	if simpleReadOnlySet[n] {
		return true, true
	}
	for _, s := range simpleReadOnlyCommands {
		h := strings.Split(s, " ")
		if len(h) < 2 || len(e) < len(h) || !equalStrings(e[:len(h)], h) {
			continue
		}
		if h[0] == "docker" && (dockerHostFlag(e) || tildeExpansion(e[len(h):])) {
			return false, true
		}
		return true, true
	}
	switch n {
	case "echo":
		return true, true
	case "printf":
		if len(e) > 1 && strings.HasPrefix(e[1], "-") && e[1] != "--" {
			return false, true
		}
		s := 1
		if len(e) > 1 && e[1] == "--" {
			s = 2
		}
		h := ""
		if s < len(e) {
			h = e[s]
		}
		if strings.Contains(h, "$") {
			return false, true
		}
		y := strings.ReplaceAll(h, "%%", "")
		if printfOctalHex.MatchString(y) || printfUnicode.MatchString(y) {
			return false, true
		}
		if printfConv.MatchString(y) || printfStar.MatchString(y) {
			for i := s + 1; i < len(e); i++ {
				w := e[i]
				if strings.Contains(w, "[") || strings.Contains(w, "`") || strings.Contains(w, "$(") || !printfNumber.MatchString(w) {
					return false, true
				}
			}
		}
		return true, true
	case "[[":
		for s := 1; s < len(e); s++ {
			h := e[s]
			var y string
			hasY := s+1 < len(e)
			if hasY {
				y = e[s+1]
			}
			if (h == "-v" || h == "-R" || h == "-t") && hasY && strings.Contains(y, "[") {
				return false, true
			}
			if h == "-t" && hasY && !testNumber.MatchString(y) {
				return false, true
			}
			if testCompare[h] {
				for _, idx := range []int{s - 1, s + 1} {
					if idx >= 0 && idx < len(e) && (strings.Contains(e[idx], "[") || !testNumber.MatchString(e[idx])) {
						return false, true
					}
				}
			}
		}
		return true, true
	case "ls":
		return true, true
	case "cd":
		return len(e) <= 2, true
	case "find":
		valueNext := false
		for h := 1; h < len(e); h++ {
			y := e[h]
			wasValue := valueNext
			valueNext = false
			// "-rm" is dangerous only on some platforms; it is treated as
			// dangerous everywhere (conservative).
			if findDangerous[y] {
				return false, true
			}
			if findFilesArg.MatchString(y) {
				return false, true
			}
			if !wasValue && findOptValue.MatchString(y) {
				valueNext = true
				continue
			}
			if !wasValue && (findValued[y] || findNewerXY.MatchString(y) || findOptValue.MatchString(y)) {
				h++
				continue
			}
		}
		return true, true
	case "history":
		return len(e) == 1 || len(e) == 2 && digitsRe.MatchString(e[1]), true
	case "arch":
		return len(e) == 1 || len(e) == 2 && (e[1] == "-h" || e[1] == "--help"), true
	case "ifconfig":
		return len(e) == 1 || len(e) == 2 && ifconfigArg.MatchString(e[1]), true
	}
	return false, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- the flag table and the whole-command regexes ----------------

var (
	// uniqRe and jqRe are the reachable whole-command regexes (every other
	// one belongs to a command simpleReadOnly already decides).
	uniqRe = regexp.MustCompile(`^uniq(?:\s+(?:-[a-zA-Z]+|--[a-zA-Z-]+(?:=\S+)?|-[fsw]\s+\d+))*(?:\s|$)\s*$`)
	// jqRe is the jq regex without its leading negative lookahead, which is
	// checked separately by jqForbidden.
	jqRe        = regexp.MustCompile(`^jq(?:\s+(?:-[a-zA-Z]+|--[a-zA-Z-]+(?:=\S+)?))*(?:\s+'[^'` + "`" + `]*'|\s+"[^"` + "`" + `]*"|\s+[^-\s'"][^\s]*)+\s*$`)
	jqForbidden = regexp.MustCompile(`(?:\s['"]?-[a-zA-Z]*[fL]|--from-file|--rawfile|--slurpfile|--run-tests|--library-path|\benv\b|\$ENV\b|\binclude\b|\bimport\b)`)
	gitDashC    = regexp.MustCompile(`\s-c[\s=]`)
	gitExecPath = regexp.MustCompile(`\s--exec-path[\s=]`)
	gitCfgEnv   = regexp.MustCompile(`\s--config-env[\s=]`)
)

// readOnlyByTable judges a command by the flag table and whole-command
// regexes.
func readOnlyByTable(text string, argv []string) bool {
	n := strings.TrimSpace(text)
	n = strings.TrimSpace(strings.TrimSuffix(n, " 2>&1"))
	if windowsUNC(n) || expansionKind(n) == "variable" {
		return false
	}
	if readOnlyByFlags(n, argv) {
		return true
	}
	matched := uniqRe.MatchString(n) || (jqRe.MatchString(n) && !jqForbidden.MatchString(n[2:]))
	if !matched {
		return false
	}
	if strings.Contains(n, "git") && (gitDashC.MatchString(n) || gitExecPath.MatchString(n) || gitCfgEnv.MatchString(n)) {
		return false
	}
	return true
}

// xargsTargets are the commands xargs may run read-only.
var xargsTargets = setOf("echo", "printf", "wc", "grep", "egrep", "fgrep", "head", "tail")

var backtickRe = regexp.MustCompile("`")

// readOnlyByFlags: the first table entry whose words prefix the argv
// decides, by the flag allowlist (validateFlags), the entry regex and its callback.
func readOnlyByFlags(raw string, n []string) bool {
	if len(n) == 0 {
		return false
	}
	var r *roCommand
	s := 0
	for i := range readOnlyCommandTable {
		c := &readOnlyCommandTable[i]
		if runtime.GOOS == "windows" && c.name == "xargs" {
			continue // xargs is dropped on Windows
		}
		w := strings.Split(c.name, " ")
		if len(n) >= len(w) && equalStrings(n[:len(w)], w) {
			r, s = c, len(w)
			break
		}
	}
	if r == nil {
		return false
	}
	if n[0] == "git" && len(n) > 1 && n[1] == "ls-remote" {
		for _, x := range n {
			if x == "-o" || x == "--server-option" || strings.HasPrefix(x, "--server-option=") {
				return false
			}
		}
		afterDD := false
		for _, w := range n[2:] {
			if w == "" {
				continue
			}
			if !afterDD && w == "--" {
				afterDD = true
				continue
			}
			if afterDD || w == "-" || !strings.HasPrefix(w, "-") {
				return false
			}
		}
	}
	for _, a := range n[s:] {
		if a == "" {
			continue
		}
		if strings.Contains(a, "$") {
			return false
		}
		if strings.Contains(a, "{") && (strings.Contains(a, ",") || strings.Contains(a, "..")) {
			return false
		}
	}
	var targets map[string]bool
	if n[0] == "xargs" {
		targets = xargsTargets
	}
	if !validateFlags(n, s, r, n[0], targets) {
		return false
	}
	if r.regex != "" {
		if !tableRegex(r.regex).MatchString(raw) {
			return false
		}
	} else if backtickRe.MatchString(raw) {
		return false
	}
	if r.regex == "" && (n[0] == "rg" || n[0] == "grep" || n[0] == "egrep" || n[0] == "fgrep") && strings.ContainsAny(raw, "\n\r") {
		return false
	}
	if cb := readOnlyCallbacks[r.name]; cb != nil && cb(raw, n[s:]) {
		return false
	}
	return true
}

var (
	tableRegexMu    sync.Mutex
	tableRegexCache = map[string]*regexp.Regexp{}
)

// tableRegex compiles (once) an entry's whole-command regex.
func tableRegex(src string) *regexp.Regexp {
	tableRegexMu.Lock()
	defer tableRegexMu.Unlock()
	re, ok := tableRegexCache[src]
	if !ok {
		re = regexp.MustCompile(src)
		tableRegexCache[src] = re
	}
	return re
}

var (
	flagLike    = regexp.MustCompile(`^-[a-zA-Z0-9_-]`)
	gitNumFlag  = regexp.MustCompile(`^-\d+$`)
	gitSortNeg  = regexp.MustCompile(`^-[a-zA-Z]`)
	numberValue = regexp.MustCompile(`^\d+$`)
)

// isFlag reports whether a word is a flag.
func isFlag(e string) bool {
	return strings.HasPrefix(e, "-") && len(e) > 1 && flagLike.MatchString(e)
}

// flagValueOK reports whether a flag value is of the expected kind.
func flagValueOK(e, kind string) bool {
	switch kind {
	case "none":
		return false
	case "number":
		return numberValue.MatchString(e)
	case "string":
		return true
	case "char":
		return utf8.RuneCountInString(e) == 1
	case "{}":
		return e == "{}"
	case "EOF":
		return e == "EOF"
	}
	return false
}

// validateFlags checks that every flag after the command words is in the
// entry's safe-flag table, with a value of the right kind.
func validateFlags(e []string, start int, r *roCommand, commandName string, xargsTargets map[string]bool) bool {
	h := start
	for h < len(e) {
		y := e[h]
		if y == "" {
			h++
			continue
		}
		if xargsTargets != nil && commandName == "xargs" && (!strings.HasPrefix(y, "-") || y == "--") {
			if y == "--" && h+1 < len(e) {
				h++
				y = e[h]
			}
			if y != "" && xargsTargets[y] {
				break
			}
			return false
		}
		if y == "--" {
			if !r.noDoubleDash {
				break
			}
			h++
			continue
		}
		if !isFlag(y) {
			h++
			continue
		}
		hasEq := strings.Contains(y, "=")
		parts := strings.SplitN(y, "=", 2)
		S := parts[0]
		C := ""
		if len(parts) == 2 {
			C = parts[1]
		}
		if S == "" {
			return false
		}
		A, ok := r.safeFlags[S]
		if !ok {
			if commandName == "git" && gitNumFlag.MatchString(S) {
				h++
				continue
			}
			if (commandName == "grep" || commandName == "egrep" || commandName == "fgrep" || commandName == "rg") &&
				strings.HasPrefix(S, "-") && !strings.HasPrefix(S, "--") && len(S) > 2 {
				I, B := S[:2], S[2:]
				if H, ok := r.safeFlags[I]; ok && numberValue.MatchString(B) {
					if H == "number" || H == "string" {
						if flagValueOK(B, H) {
							h++
							continue
						}
						return false
					}
				}
			}
			if strings.HasPrefix(S, "-") && !strings.HasPrefix(S, "--") && len(S) > 2 {
				for i := 1; i < len(S); i++ {
					H, ok := r.safeFlags["-"+string(S[i])]
					if !ok || H != "none" {
						return false
					}
				}
				h++
				continue
			}
			return false
		}
		if A == "none" {
			if hasEq {
				return false
			}
			h++
			continue
		}
		var I string
		if hasEq {
			I = C
			h++
		} else {
			if h+1 >= len(e) || (e[h+1] != "" && isFlag(e[h+1])) {
				return false
			}
			I = e[h+1]
			h += 2
		}
		if A == "string" && strings.HasPrefix(I, "-") {
			if !(S == "--sort" && commandName == "git" && gitSortNeg.MatchString(I)) {
				return false
			}
		}
		if !flagValueOK(I, A) {
			return false
		}
	}
	return true
}

// --- callbacks (additionalCommandIsDangerousCallback) ------------------------

var (
	gitSigFormat  = regexp.MustCompile(`%[-+ ]?G|%\(\*?signature`)
	tildeAfterSep = regexp.MustCompile(`(?:^|[=:])~`)
	gitRemoteName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]*$`)
	psFlagSet     = regexp.MustCompile(`^-[AacdeFfjlwHLTm]+$`)
	psHLTm        = regexp.MustCompile(`[HLTm]`)
	psDashE       = regexp.MustCompile(`^-[a-zA-Z]*e`)
	lsofIAt       = regexp.MustCompile(`^-[a-zA-Z]*i\S*@`)
	lsofI         = regexp.MustCompile(`^-[a-zA-Z]*i$`)
	hasLetter     = regexp.MustCompile(`[a-zA-Z]`)
	ssWord        = regexp.MustCompile(`^(dst|src|dport|sport|and|or|not|eq|ne|ge|le|gt|lt|autobound|state|exclude|dev|fwmark|cgroup)$`)
	ssValued      = regexp.MustCompile(`^(state|exclude|dport|sport|dev|fwmark|cgroup)$`)
	ssOptValued   = regexp.MustCompile(`^(-f|--family|-A|--query|--socket)$`)
	ssSplit       = regexp.MustCompile(`[\s()=!<>&|,]+`)
	ssNonHex      = regexp.MustCompile(`[g-zG-Z]`)
	ssHex         = regexp.MustCompile(`[a-fA-F]`)
	dockerShort   = regexp.MustCompile(`^-([A-Za-z]+)`)
)

// gitBuiltinFormats are git's built-in --format/--pretty names.
var gitBuiltinFormats = setOf("oneline", "short", "medium", "full", "fuller", "email", "raw")

// gitFormatDangerous: a GPG-signature placeholder runs gpg, a `~`
// could expand, and a named (non-builtin) --format/--pretty is a config alias.
func gitFormatDangerous(e []string) bool {
	bad := func(r string) bool {
		return tildeExpansion([]string{r}) || gitSigFormat.MatchString(r) || strings.Contains(r, "signature")
	}
	vf := func(e string) bool {
		if e == "" || strings.Contains(e, "%") || strings.HasPrefix(e, "format:") || strings.HasPrefix(e, "tformat:") {
			return false
		}
		return !gitBuiltinFormats[e]
	}
	for r, s := range e {
		if gitSigFormat.MatchString(s) {
			return true
		}
		for _, h := range []string{"--format", "--pretty", "--sort", "--group"} {
			var y string
			found := false
			if s == h && r+1 < len(e) {
				y, found = e[r+1], true
			} else if strings.HasPrefix(s, h+"=") {
				y, found = s[len(h)+1:], true
			}
			if !found {
				continue
			}
			if bad(y) {
				return true
			}
			if (h == "--format" || h == "--pretty") && vf(y) {
				return true
			}
		}
	}
	return false
}

// anyTilde reports whether any argument contains a tilde.
func anyTilde(e []string) bool {
	for _, n := range e {
		if strings.Contains(n, "~") {
			return true
		}
	}
	return false
}

// tildeExpansion reports whether a word would undergo tilde expansion.
func tildeExpansion(e []string) bool {
	for _, n := range e {
		if tildeAfterSep.MatchString(n) || strings.Contains(n, "~") && strings.Contains(n, "{") {
			return true
		}
	}
	return false
}

// dockerHostFlags / dockerHostFlag detect docker options that select a
// different daemon, context or config.
var dockerHostFlags = []string{"-H", "-c", "-r", "--host", "--context", "--config", "--tlscacert", "--tlscert", "--tlskey", "--url", "--connection", "--identity", "--remote", "--module", "--out"}

func dockerHostFlag(e []string) bool {
	for _, n := range e {
		for _, s := range dockerHostFlags {
			if n == s || strings.HasPrefix(n, s+"=") || len(s) == 2 && len(n) > 2 && strings.HasPrefix(n, s) {
				return true
			}
		}
		if m := dockerShort.FindStringSubmatch(n); m != nil && len(m[1]) >= 2 {
			for _, c := range m[1] {
				if c == 'H' || c == 'c' || c == 'r' {
					return true
				}
			}
		}
	}
	return false
}

// beforeFirst returns the part of a string before the first separator.
func beforeFirst(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// gitTagBranchListing is the shared shape of the git tag / git branch
// callbacks: a positional argument is only allowed in list mode.
func gitListOnly(n []string, valued map[string]bool, mergedLike map[string]bool) bool {
	h, last, list, afterDD := 0, "", false, false
	for h < len(n) {
		p := n[h]
		if p == "" {
			h++
			continue
		}
		if p == "--" && !afterDD {
			afterDD, last = true, ""
			h++
			continue
		}
		if !afterDD && strings.HasPrefix(p, "-") {
			if p == "--list" || p == "-l" {
				list = true
			} else if p[0] == '-' && len(p) > 1 && p[1] != '-' && len(p) > 2 && !strings.Contains(p, "=") && strings.Contains(p[1:], "l") {
				list = true
			}
			switch {
			case strings.Contains(p, "="):
				last = beforeFirst(p, "=")
				h++
			case valued[p]:
				last = p
				h += 2
			default:
				last = p
				h++
			}
			continue
		}
		if !list && !mergedLike[last] {
			return true
		}
		h++
	}
	return false
}

// readOnlyCallbacks are the table's additionalCommandIsDangerousCallback
// functions, keyed by entry name (true = dangerous).
var readOnlyCallbacks = map[string]func(raw string, n []string) bool{
	"git log":          func(_ string, n []string) bool { return gitFormatDangerous(n) },
	"git show":         func(_ string, n []string) bool { return gitFormatDangerous(n) },
	"git shortlog":     func(_ string, n []string) bool { return gitFormatDangerous(n) },
	"git rev-list":     func(_ string, n []string) bool { return gitFormatDangerous(n) },
	"git for-each-ref": func(_ string, n []string) bool { return gitFormatDangerous(n) },
	"git reflog": func(_ string, n []string) bool {
		if len(n) > 0 && n[0] != "" && !strings.HasPrefix(n[0], "-") && n[0] != "show" && n[0] != "list" {
			return true
		}
		for _, y := range n {
			switch y {
			case "expire", "delete", "exists", "drop", "write":
				return true
			}
		}
		return false
	},
	"git ls-remote": func(_ string, n []string) bool {
		if gitFormatDangerous(n) {
			return true
		}
		afterDD := false
		for s := 0; s < len(n); s++ {
			h := n[s]
			if !afterDD && h == "--" {
				afterDD = true
				continue
			}
			if !afterDD && (h == "" || strings.HasPrefix(h, "-")) {
				if h == "--sort" {
					s++
				}
				continue
			}
			return true
		}
		return false
	},
	"git remote show": func(_ string, n []string) bool {
		var s, h []string
		dd := -1
		for i, x := range n {
			if x == "--" {
				dd = i
				break
			}
		}
		if dd == -1 {
			s = n
		} else {
			s, h = n[:dd], n[dd+1:]
		}
		var y []string
		hasN := false
		for _, w := range s {
			if w == "-n" {
				hasN = true
				continue
			}
			y = append(y, w)
		}
		y = append(y, h...)
		if len(y) != 1 || !hasN {
			return true
		}
		return !gitRemoteName.MatchString(y[0])
	},
	"git remote": func(_ string, n []string) bool {
		for _, r := range n {
			if r != "-v" && r != "--verbose" {
				return true
			}
		}
		return false
	},
	"git tag": func(_ string, n []string) bool {
		if gitFormatDangerous(n) {
			return true
		}
		// The tag callback advances past "--opt=value" by one and a valued
		// option by two; it has no merged-like exemption.
		return gitListOnly(n, setOf("--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--sort", "--format", "-n"), nil)
	},
	"git branch": func(_ string, n []string) bool {
		if gitFormatDangerous(n) {
			return true
		}
		return gitListOnly(n, setOf("--contains", "--no-contains", "--points-at", "--sort"), setOf("--merged", "--no-merged"))
	},
	"sed": func(_ string, n []string) bool { return !sedReadOnly(n) },
	"man": func(_ string, n []string) bool {
		r := setOf("-k", "-f", "--apropos", "--whatis")
		valued := setOf("-S", "-s")
		h := false
		for _, w := range n {
			if w == "--" {
				break
			}
			if !strings.HasPrefix(w, "-") || w == "-" {
				continue
			}
			if strings.HasPrefix(w, "--") {
				if r[w] {
					h = true
				}
			} else if strings.ContainsAny(w[1:], "kf") {
				h = true
			}
		}
		y, S := false, false
		for w := 0; w < len(n); w++ {
			H := n[w]
			if !S && H == "--" {
				S = true
				continue
			}
			if !(S || !strings.HasPrefix(H, "-") || H == "-") {
				if r[H] {
					y = true
				} else if valued[H] {
					w++
				}
				continue
			}
			S = true
			if h && strings.HasPrefix(H, "-") {
				return true
			}
			if !y && (strings.Contains(H, "/") || strings.Contains(H, `\`) || strings.Contains(H, "~")) {
				return true
			}
		}
		return false
	},
	"help": func(_ string, n []string) bool {
		if anyTilde(n) {
			return true
		}
		for _, r := range n {
			if strings.Contains(r, "/") || strings.Contains(r, `\`) {
				return true
			}
		}
		return false
	},
	"ps": func(_ string, n []string) bool {
		r, s, all := false, false, true
		count := 0
		for _, x := range n {
			if !isFlag(x) && strings.ContainsAny(x, "eE") {
				r = true
			}
			if psDashE.MatchString(x) {
				s = true
			}
			if !(x == "--forest" || psFlagSet.MatchString(x)) {
				all = false
			}
			if x == "--forest" {
				count++
			} else {
				count += len(psHLTm.FindAllString(x, -1))
			}
		}
		return r || s && !(all && count <= 1)
	},
	"date": func(_ string, n []string) bool {
		valued := setOf("-d", "--date", "-r", "--reference", "--rfc-3339")
		s := 0
		for s < len(n) {
			h := n[s]
			switch {
			case strings.HasPrefix(h, "--") && strings.Contains(h, "="):
				s++
			case strings.HasPrefix(h, "-"):
				if valued[h] {
					s += 2
				} else {
					s++
				}
			default:
				if !strings.HasPrefix(h, "+") {
					return true
				}
				s++
			}
		}
		return false
	},
	"lsof": func(_ string, n []string) bool {
		for r := 0; r < len(n); r++ {
			s := n[r]
			if strings.HasPrefix(s, "+m") {
				return true
			}
			if lsofIAt.MatchString(s) {
				if hasLetter.MatchString(beforeFirst(s[strings.Index(s, "@")+1:], ":")) {
					return true
				}
			}
			if lsofI.MatchString(s) {
				h := ""
				if r+1 < len(n) {
					h = n[r+1]
				}
				if strings.Contains(h, "~") {
					return true
				}
				if i := strings.Index(h, "@"); i >= 0 && hasLetter.MatchString(beforeFirst(h[i+1:], ":")) {
					return true
				}
			}
		}
		return false
	},
	"tput": func(_ string, n []string) bool {
		bad := setOf("init", "reset", "rs1", "rs2", "rs3", "is1", "is2", "is3", "iprog", "if", "rf", "clear", "flash", "mc0", "mc4", "mc5", "mc5i", "mc5p", "pfkey", "pfloc", "pfx", "pfxl", "smcup", "rmcup")
		h, afterDD := 0, false
		for h < len(n) {
			S := n[h]
			switch {
			case S == "--":
				afterDD = true
				h++
			case !afterDD && strings.HasPrefix(S, "-"):
				if S == "-S" {
					return true
				}
				if !strings.HasPrefix(S, "--") && len(S) > 2 && strings.Contains(S, "S") {
					return true
				}
				if S == "-T" {
					h += 2
				} else {
					h++
				}
			default:
				if bad[S] {
					return true
				}
				h++
			}
		}
		return false
	},
	"ss": func(_ string, n []string) bool {
		var y []string
		afterDD := false
		for q := 0; q < len(n); q++ {
			K := n[q]
			if !afterDD && K == "--" {
				afterDD = true
				continue
			}
			if !afterDD && strings.HasPrefix(K, "-") {
				if ssOptValued.MatchString(K) {
					q++
				}
				continue
			}
			y = append(y, K)
		}
		if anyTilde(y) {
			return true
		}
		skip := false
		for _, q := range ssSplit.Split(strings.Join(y, " "), -1) {
			if q == "" {
				continue
			}
			if skip {
				skip = false
				continue
			}
			if ssWord.MatchString(q) {
				skip = ssValued.MatchString(q)
				continue
			}
			if ssNonHex.MatchString(q) || ssHex.MatchString(q) && (strings.Contains(q, ".") || !strings.Contains(q, ":")) {
				return true
			}
		}
		return false
	},
	"docker logs":    func(_ string, n []string) bool { return dockerHostFlag(n) || tildeExpansion(n) },
	"docker inspect": func(_ string, n []string) bool { return dockerHostFlag(n) || tildeExpansion(n) },
	"test": func(_ string, n []string) bool {
		for _, r := range n {
			if r == "-v" || r == "-R" || r == "-a" || r == "-o" || strings.Contains(r, "[") {
				return true
			}
		}
		for r := 0; r < len(n); r++ {
			if testCompare[n[r]] {
				for _, idx := range []int{r - 1, r + 1} {
					if idx >= 0 && idx < len(n) && !testNumber.MatchString(n[idx]) {
						return true
					}
				}
			}
			if n[r] == "-t" && r+1 < len(n) && !testNumber.MatchString(n[r+1]) {
				return true
			}
		}
		return false
	},
}
