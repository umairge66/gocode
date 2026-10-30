package toolreg

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// Path validation of a read-only Bash command (for the path-restricted read
// commands). A read-only command is
// auto-allowed only when every path it reads lies inside the session's
// working directories or is covered by a Read allow rule; a path covered by a
// Read deny rule denies; any other path (outside the working directories, an
// unresolvable `~user`, a relative read after `cd` while Read deny rules
// exist, more than one `cd`) leaves the call to the normal flow, which asks.
//
// Only read commands are covered. The write side (mkdir, touch, rm, mv, cp,
// tee, sed -i, output redirections) is not needed here: a command using them
// is never read-only.

// bashReadVerdict is the outcome of the read-path check.
type bashReadVerdict int

const (
	bashReadAllowed bashReadVerdict = iota // every path inside / allowed
	bashReadAsk                            // fall through to the normal flow
	bashReadDenied                         // a Read deny rule covers a path
)

// bashReadOnlyDecision returns the decision for a read-only Bash call that no
// deny / ask / allow rule decided, or ok=false to fall through.
func (d *Decider) bashReadOnlyDecision(call Call, rules RuleSet) (Decision, bool) {
	cmds, ro := analyzeReadOnlyBash(call.RuleContent)
	if !ro {
		return Decision{}, false
	}
	verdict, denied := d.checkBashReadPaths(cmds, rules)
	switch verdict {
	case bashReadDenied:
		return Decision{
			Behavior:   BehaviorDeny,
			ReasonType: ReasonRule,
			Reason:     "Permission to read " + pathguard.EscapeControl(denied) + " has been denied.",
		}, true
	case bashReadAllowed:
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: "Read-only command is allowed"}, true
	}
	return Decision{}, false
}

// checkBashReadPaths judges every path the commands read.
func (d *Decider) checkBashReadPaths(cmds []shCommand, rules RuleSet) (bashReadVerdict, string) {
	cdCount := 0
	for _, c := range cmds {
		if isCdCommand(c) {
			cdCount++
		}
	}
	if cdCount > 1 {
		return bashReadAsk, "" // "Multiple directory changes in one command require approval"
	}
	readDenyRules := false
	for _, r := range rules {
		if r.Behavior == RuleDeny && r.ToolName == string(protocol.ToolRead) {
			readDenyRules = true
		}
	}
	verdict := bashReadAllowed
	for _, c := range cmds {
		paths, ok := bashReadPathsOf(c)
		if !ok {
			verdict = bashReadAsk
			continue
		}
		for _, p := range paths {
			if cdCount > 0 && readDenyRules && !isCdCommand(c) && !filepath.IsAbs(expandHomePath(p)) {
				verdict = bashReadAsk // cd-compound relative read with a Read deny rule
				continue
			}
			switch d.judgeReadPath(p, rules) {
			case bashReadDenied:
				return bashReadDenied, p
			case bashReadAsk:
				verdict = bashReadAsk
			}
		}
	}
	return verdict, ""
}

// judgeReadPath judges one read path by the rule and working-directory
// checks.
func (d *Decider) judgeReadPath(p string, rules RuleSet) bashReadVerdict {
	if devPath(p) {
		return bashReadAllowed
	}
	if strings.HasPrefix(p, "~") && p != "~" && !strings.HasPrefix(p, "~/") {
		return bashReadAsk
	}
	abs := d.absReadPath(expandHomePath(p))
	if abs == "" {
		return bashReadAsk
	}
	rc := Call{Tool: protocol.ToolRead, Rule: string(protocol.ToolRead), Path: abs, Op: pathguard.OpRead}
	if d.matchRule(rules, rc, &spellingCache{}, RuleDeny) != nil {
		return bashReadDenied
	}
	if d.matchRule(rules, rc, &spellingCache{}, RuleAsk) != nil {
		return bashReadAsk
	}
	if d.withinWorkingDirs(abs) {
		return bashReadAllowed
	}
	if d.matchRule(rules, rc, &spellingCache{}, RuleAllow) != nil {
		return bashReadAllowed
	}
	return bashReadAsk
}

// absReadPath resolves p against the shell's working directory.
func (d *Decider) absReadPath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	cwd := ""
	if d.Files != nil {
		cwd = d.Files.Cwd
		if cwd == "" {
			cwd = d.Files.OriginalCwd
		}
	}
	if cwd == "" {
		cwd = d.roots().Cwd
	}
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, p)
}

// withinWorkingDirs reports whether abs (and where it leads) lies inside the
// session's working directories; without session files, inside the process
// working directory.
func (d *Decider) withinWorkingDirs(abs string) bool {
	if d.Files != nil {
		res := d.Files.ResolvePath(abs)
		if res.Unresolved {
			return false
		}
		return d.Files.WithinWorkingDirs(res.Spellings)
	}
	r := d.roots()
	if r.Cwd == "" {
		return false
	}
	roots := []string{filepath.Clean(r.Cwd)}
	if real, err := filepath.EvalSymlinks(r.Cwd); err == nil {
		roots = append(roots, real)
	}
	spellings := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		spellings = append(spellings, real)
	}
	for _, s := range spellings {
		inside := false
		for _, root := range roots {
			if rel, err := filepath.Rel(root, s); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

var devFdRe = regexp.MustCompile(`^/dev/fd/\d+$|^/proc/self/fd/[0-2]$`)

// devPath reports the stream devices, which are never a file read.
func devPath(p string) bool {
	switch p {
	case "/dev/null", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/tty":
		return true
	}
	return devFdRe.MatchString(p)
}

// globPrefix trims a path at its first glob character to the directory that
// holds the matches.
func globPrefix(p string) string {
	i := strings.IndexAny(p, "*?[")
	if i < 0 {
		return p
	}
	j := strings.LastIndex(p[:i], "/")
	if j < 0 {
		return "."
	}
	if j == 0 {
		return "/"
	}
	return p[:j]
}

// bashReadPathsOf returns the paths one read-only command reads (its
// command's path extractor, plus input redirections). ok is false when the
// paths cannot be determined.
func bashReadPathsOf(c shCommand) ([]string, bool) {
	var paths []string
	for _, r := range c.redirects {
		if r.op == "<" {
			paths = append(paths, r.target)
		}
	}
	argv := stripCommandBuiltin(c.argv)
	if len(argv) == 0 {
		return paths, true
	}
	name, args := argv[0], argv[1:]
	var got []string
	switch name {
	case "cd":
		// cd with two or more directory arguments asks.
		if !cdSinglePositional(args) {
			return nil, false
		}
		n := positionalArgs(args)
		switch {
		case len(n) > 0:
			got = []string{n[0]}
		case len(args) > 0 && args[len(args)-1] == "-":
			return nil, false // `cd -` goes to $OLDPWD
		default:
			got = []string{"~"}
		}
	case "ls":
		got = positionalArgs(args)
		if len(got) == 0 {
			got = []string{"."}
		}
	case "find":
		got = findPaths(args)
	case "cat", "head", "tail", "sort", "uniq", "wc", "file", "stat", "diff", "strings", "hexdump", "od", "base64", "nl", "sha256sum", "sha1sum", "md5sum", "rev", "tsort":
		got = positionalArgs(args)
	case "cut", "paste", "column", "tac", "fold", "expand", "unexpand", "fmt", "comm", "cmp", "pr", "numfmt":
		got = pvPositional(pvSpecs[name], args)
	case "tr":
		del := false
		for _, s := range args {
			if s == "-d" || s == "--delete" || strings.HasPrefix(s, "-") && strings.Contains(s, "d") {
				del = true
			}
		}
		n := positionalArgs(args)
		skip := 2
		if del {
			skip = 1
		}
		if len(n) > skip {
			got = n[skip:]
		}
	case "grep", "egrep", "fgrep":
		got = grepPaths(args, setOf("-e", "--regexp", "-f", "--file", "--exclude", "--include", "--exclude-dir", "--include-dir", "-m", "--max-count", "-A", "--after-context", "-B", "--before-context", "-C", "--context"), nil)
		if len(got) == 0 {
			for _, s := range args {
				if s == "-r" || s == "-R" || s == "--recursive" {
					got = []string{"."}
				}
			}
		}
	case "rg":
		got = grepPaths(args, setOf("-e", "--regexp", "-f", "--file", "-t", "--type", "-T", "--type-not", "-g", "--glob", "-m", "--max-count", "--max-depth", "-r", "--replace", "-A", "--after-context", "-B", "--before-context", "-C", "--context"), []string{"."})
	case "sed":
		got = sedPaths(args)
	case "jq":
		got = jqPaths(args)
	case "git":
		var ok bool
		got, ok = gitPaths(args)
		if !ok {
			return nil, false
		}
	case "man":
		for _, a := range args {
			if strings.ContainsAny(a, `/\~`) || strings.HasPrefix(a, ".") {
				return nil, false
			}
		}
	}
	for _, g := range got {
		if g == "" {
			continue
		}
		paths = append(paths, globPrefix(g))
	}
	return paths, true
}

// positionalArgs returns the arguments from the first non-flag (or after
// "--") on.
func positionalArgs(e []string) []string {
	var n []string
	dd, started := false, false
	for _, h := range e {
		switch {
		case dd || started:
			n = append(n, h)
		case h == "--":
			dd = true
		case h == "-" || !strings.HasPrefix(h, "-"):
			n = append(n, h)
			started = true
		}
	}
	return n
}

// cdSinglePositional returns cd's single directory operand.
func cdSinglePositional(e []string) bool {
	started, r := false, 0
	for _, s := range e {
		if !started {
			if s == "--" {
				started = true
				continue
			}
			if strings.HasPrefix(s, "-") && s != "-" {
				continue
			}
			started = true
		}
		r++
	}
	return r <= 1
}

// findPaths returns the paths find reads.
func findPaths(e []string) []string {
	var n []string
	valued := setOf("-newer", "-anewer", "-cnewer", "-mnewer", "-samefile", "-path", "-wholename", "-ilname", "-lname", "-ipath", "-iwholename")
	h, y := false, false
	for S := 0; S < len(e); S++ {
		w := e[S]
		if w == "" {
			continue
		}
		if y {
			n = append(n, w)
			continue
		}
		if w == "--" {
			y = true
			continue
		}
		if strings.HasPrefix(w, "-") {
			if w == "-H" || w == "-L" || w == "-P" {
				continue
			}
			h = true
			if valued[w] || findPathNewerXY.MatchString(w) {
				if S+1 < len(e) && e[S+1] != "" {
					n = append(n, e[S+1])
					S++
				}
			}
			continue
		}
		if !h {
			n = append(n, w)
		}
	}
	if len(n) == 0 {
		return []string{"."}
	}
	return n
}

// pvSpec is an option table for pvPositional.
type pvSpec struct {
	booleanShort, valuedShort, attachedShort string
	booleanLong, valuedLong, attachedLong    map[string]bool
}

var pvSpecs = map[string]pvSpec{
	"cut":      {booleanShort: "nswz", valuedShort: "bcdf", booleanLong: setOf("--complement", "--only-delimited", "--zero-terminated"), valuedLong: setOf("--bytes", "--characters", "--delimiter", "--fields", "--output-delimiter")},
	"paste":    {booleanShort: "sz", valuedShort: "d", booleanLong: setOf("--serial", "--zero-terminated"), valuedLong: setOf("--delimiters")},
	"column":   {booleanShort: "txdemLJhV", valuedShort: "cosNOClEHRTWrip", booleanLong: setOf("--table", "--fillrows", "--table-noheadings", "--table-header-repeat", "--table-maxout", "--keep-empty-lines", "--json"), valuedLong: setOf("--separator", "--output-separator", "--output-width", "--table-name", "--table-order", "--table-column", "--table-columns", "--table-columns-limit", "--table-noextreme", "--table-hide", "--table-right", "--table-truncate", "--table-wrap", "--tree", "--tree-id", "--tree-parent")},
	"tac":      {booleanShort: "br", valuedShort: "s", booleanLong: setOf("--before", "--regex"), valuedLong: setOf("--separator")},
	"fold":     {booleanShort: "bs", valuedShort: "w", booleanLong: setOf("--bytes", "--spaces"), valuedLong: setOf("--width")},
	"expand":   {booleanShort: "i", valuedShort: "t", booleanLong: setOf("--initial"), valuedLong: setOf("--tabs")},
	"unexpand": {booleanShort: "af", valuedShort: "t", booleanLong: setOf("--all", "--first-only"), valuedLong: setOf("--tabs")},
	"fmt":      {booleanShort: "csumn", valuedShort: "wgdl", booleanLong: setOf("--crown-margin", "--split-only", "--tagged-paragraph", "--uniform-spacing"), valuedLong: setOf("--width", "--goal", "--prefix")},
	"comm":     {booleanShort: "123iz", booleanLong: setOf("--check-order", "--nocheck-order", "--total", "--zero-terminated"), valuedLong: setOf("--output-delimiter")},
	"cmp":      {booleanShort: "blsvxzh", booleanLong: setOf("--print-bytes", "--verbose", "--quiet", "--silent"), valuedLong: setOf("--ignore-initial", "--bytes")},
	"pr":       {booleanShort: "acdFfJmprtTv", attachedShort: "einsS", valuedShort: "hlwWoND", booleanLong: setOf("--across", "--show-control-chars", "--double-space", "--form-feed", "--join-lines", "--merge", "--no-file-warnings", "--omit-header", "--omit-pagination", "--show-nonprinting"), attachedLong: setOf("--expand-tabs", "--output-tabs", "--number-lines", "--separator", "--sep-string"), valuedLong: setOf("--header", "--length", "--width", "--page-width", "--indent", "--first-line-number", "--date-format", "--columns")},
	"numfmt":   {booleanShort: "z", valuedShort: "d", booleanLong: setOf("--grouping", "--zero-terminated", "--debug"), attachedLong: setOf("--header"), valuedLong: setOf("--from", "--to", "--from-unit", "--to-unit", "--format", "--padding", "--delimiter", "--field", "--round", "--suffix", "--invalid")},
}

var (
	dashDigits      = regexp.MustCompile(`^-\d+$`)
	findPathNewerXY = regexp.MustCompile(`^-newer[acmBt][acmtB]$`)
	grepClusterF    = regexp.MustCompile(`^-[a-zA-Z]*f$`)
)

// pvPositional returns the positional operands under an option table.
func pvPositional(e pvSpec, r []string) []string {
	kind := func(r string) string {
		if strings.HasPrefix(r, "--") {
			switch {
			case strings.Contains(r, "="):
				return "attached"
			case e.booleanLong[r]:
				return "boolean"
			case e.attachedLong[r]:
				return "attached"
			case e.valuedLong[r]:
				return "valued"
			}
			return "unknown"
		}
		if dashDigits.MatchString(r) {
			return "attached"
		}
		for s := 1; s < len(r); s++ {
			h := r[s]
			if strings.IndexByte(e.booleanShort, h) >= 0 {
				continue
			}
			if strings.IndexByte(e.attachedShort, h) >= 0 {
				return "attached"
			}
			if strings.IndexByte(e.valuedShort, h) < 0 {
				return "unknown"
			}
			if s == len(r)-1 {
				return "valued"
			}
			return "attached"
		}
		return "boolean"
	}
	var s []string
	dd, started, flagsOK := false, false, true
	for w := 0; w < len(r); w++ {
		H := r[w]
		switch {
		case dd || started:
			s = append(s, H)
		case H == "--":
			dd = true
		case H != "-" && strings.HasPrefix(H, "-"):
			if !flagsOK {
				continue
			}
			switch kind(H) {
			case "valued":
				w++
			case "unknown":
				flagsOK = false
			}
		default:
			s = append(s, H)
			started = true
		}
	}
	return s
}

// grepPaths returns the operands after the pattern (or every operand when the
// pattern came from -e / -f), plus -f / --exclude-from files; def when none.
func grepPaths(e []string, valued map[string]bool, def []string) []string {
	var s []string
	patternSeen, dd, started := false, false, false
	for w := 0; w < len(e); w++ {
		H := e[w]
		if !dd && !started && H == "--" {
			dd = true
			continue
		}
		if !dd && !started && H != "-" && strings.HasPrefix(H, "-") {
			q := strings.Index(H, "=")
			K := H
			if q >= 0 {
				K = H[:q]
			}
			if K == "-e" || K == "--regexp" || K == "-f" || K == "--file" {
				patternSeen = true
				if K == "-f" || K == "--file" {
					var Q string
					if q >= 0 {
						Q = H[q+1:]
					} else if w+1 < len(e) {
						Q = e[w+1]
					}
					if Q != "" {
						s = append(s, Q)
					}
				}
			}
			if grepClusterF.MatchString(K) && K != "-f" && q < 0 && w+1 < len(e) {
				patternSeen = true
				s = append(s, e[w+1])
				w++
				continue
			}
			if K == "--exclude-from" || K == "--include-from" || K == "--ignore-file" {
				var Q string
				if q >= 0 {
					Q = H[q+1:]
				} else if w+1 < len(e) {
					Q = e[w+1]
				}
				if Q != "" {
					s = append(s, Q)
				}
				if q < 0 {
					w++
				}
				continue
			}
			if q < 0 {
				if Q, ok := attachedOpt(H, []string{"-f", "--file"}); ok {
					patternSeen = true
					s = append(s, Q)
					continue
				}
				if len(H) > 2 && strings.HasPrefix(H, "-e") {
					patternSeen = true
					continue
				}
			}
			if valued[K] && q < 0 {
				w++
			}
			continue
		}
		if started && !dd {
			if Q, ok := attachedOpt(H, []string{"-f", "--file"}); ok {
				s = append(s, Q)
			}
		}
		started = true
		if !patternSeen {
			patternSeen = true
			continue
		}
		s = append(s, H)
	}
	if len(s) > 0 {
		return s
	}
	return def
}

// attachedOpt splits an option with an attached value.
func attachedOpt(e string, n []string) (string, bool) {
	if !strings.HasPrefix(e, "-") {
		return "", false
	}
	if r := strings.Index(e, "="); r >= 0 {
		if contains(n, e[:r]) {
			return e[r+1:], true
		}
		return "", false
	}
	for _, s := range n {
		if len(s) == 2 && s[0] == '-' && strings.HasPrefix(e, s) && e != s {
			return e[2:], true
		}
	}
	return "", false
}

// sedPaths returns the paths sed reads.
func sedPaths(e []string) []string {
	var n []string
	skip, exprSeen, dd, started := false, false, false, false
	for _, w := range e {
		if skip {
			skip = false
			continue
		}
		if w == "" {
			continue
		}
		if !dd && !started && w == "--" {
			dd = true
			continue
		}
		if !dd && !started && w != "-" && strings.HasPrefix(w, "-") {
			switch {
			case w == "-f" || w == "--file":
				skip, exprSeen = true, true
			case w == "-e" || w == "--expression":
				skip, exprSeen = true, true
			case strings.ContainsAny(w, "ef"):
				exprSeen = true
			}
			continue
		}
		started = true
		if !exprSeen {
			exprSeen = true
			continue
		}
		n = append(n, w)
	}
	return n
}

// jqPaths returns the paths jq reads.
func jqPaths(e []string) []string {
	var n []string
	valued := setOf("-e", "--expression", "--arg", "--argjson", "--args", "--jsonargs", "-L", "--library-path", "--indent", "--tab")
	filterSeen, dd := false, false
	for y := 0; y < len(e); y++ {
		S := e[y]
		if !dd && S == "--" {
			dd = true
			continue
		}
		if !dd && strings.HasPrefix(S, "-") {
			w := strings.Index(S, "=")
			H := S
			if w >= 0 {
				H = S[:w]
			}
			if H == "-e" || H == "--expression" {
				filterSeen = true
			}
			if valued[H] && w < 0 {
				y++
			}
			continue
		}
		if !filterSeen {
			filterSeen = true
			continue
		}
		n = append(n, S)
	}
	return n
}

// gitGlobalValued are git's global options that take a value.
var gitGlobalValued = setOf("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--attr-source", "--shallow-file")

// gitPaths returns the read paths of git grep / git diff. A read-only git
// command never carries global options (the table matches "git <sub>"
// exactly), so -C / --work-tree prefixes do not arise; ok is false if they do.
func gitPaths(e []string) ([]string, bool) {
	r := 0
	for r < len(e) && strings.HasPrefix(e[r], "-") {
		if gitGlobalValued[e[r]] {
			return nil, false
		}
		r++
	}
	if r >= len(e) {
		return nil, true
	}
	S := e[r+1:]
	switch e[r] {
	case "grep":
		q := grepPaths(S, setOf("-e", "--regexp", "-f", "--file", "-A", "--after-context", "-B", "--before-context", "-C", "--context", "-m", "--max-count", "--max-depth", "--threads"), nil)
		if len(q) == 0 {
			return []string{"."}, true
		}
		return q, true
	case "diff":
		var w []string
		dd := false
		for _, q := range S {
			if !dd && q == "--" {
				dd = true
			} else if dd || q == "-" || !strings.HasPrefix(q, "-") {
				if !devPath(q) {
					w = append(w, q)
				}
			}
		}
		return w, true
	}
	return nil, true
}
