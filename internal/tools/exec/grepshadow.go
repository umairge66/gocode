package exec

import (
	"path/filepath"
	"strings"
)

// The model's shell runs `grep` as ugrep: before each command a shell
// function named grep is defined that calls the ugrep binary with
// grep-compatible defaults, so `grep -rn foo .` respects .gitignore, skips
// binary files and VCS metadata, and searches hidden files.
//
// The function hands the command to the system grep unchanged when
//   - an argument is one of the options in GrepShadowPassThrough — ugrep
//     options that start a pager, viewer, filter program or interactive
//     query, read or write a configuration file, or (-Z/-z, --null,
//     --null-data) mean something different in ugrep than in grep; the
//     system grep then runs them with its own meaning, or rejects an
//     option it does not have with its usual "unrecognized option" error;
//   - the ugrep binary is missing or not executable.
// egrep, fgrep and zgrep are left as they are.

// GrepShadowArgs are the options the shadowing function passes to ugrep
// before the command's own arguments.
var GrepShadowArgs = []string{
	"-G", "--ignore-files", "--hidden", "-I",
	"--exclude-dir=.git", "--exclude-dir=.svn", "--exclude-dir=.hg",
	"--exclude-dir=.bzr", "--exclude-dir=.jj", "--exclude-dir=.sl",
}

// GrepShadowPassThrough are the shell case patterns of arguments that send
// a grep command to the system grep instead of ugrep.
var GrepShadowPassThrough = []string{
	"-*-filter*", "-*-pager*", "-*-view*", "-*-format-open*", "-*-config*",
	"---*", "-@*", "-*-save-config*", "-[Zz]*", "-[!-]*[Zz]*",
	"--null", "--null-data",
}

// GrepShadowFunction returns the shell text that defines the grep function
// running ugrepPath. useKeyword selects `function grep { ... }` (bash, zsh),
// which an alias named grep cannot break; otherwise the POSIX `grep() { ... }`
// form is used (sh, dash). The text is one line ending in "; " so the command
// that follows keeps its line numbers.
func GrepShadowFunction(ugrepPath string, useKeyword bool) string {
	bin := shellQuote(ugrepPath)
	var b strings.Builder
	b.WriteString("unalias grep 2>/dev/null || :; ")
	if useKeyword {
		b.WriteString("function grep { ")
	} else {
		b.WriteString("grep() { ")
	}
	b.WriteString(`for __gocode_grep_a in ${1+"$@"}; do case "$__gocode_grep_a" in `)
	b.WriteString(strings.Join(GrepShadowPassThrough, "|"))
	b.WriteString(`) unset __gocode_grep_a; command grep ${1+"$@"}; return;; esac; done; unset __gocode_grep_a; `)
	b.WriteString("if [ ! -x " + bin + ` ]; then command grep ${1+"$@"}; return; fi; `)
	b.WriteString(bin + " " + strings.Join(GrepShadowArgs, " ") + ` ${1+"$@"}; }; `)
	return b.String()
}

// ShadowGrep returns inv with the grep function defined ahead of its command,
// so `grep` in the command runs ugrepPath. It leaves inv unchanged when
// ugrepPath is empty or the shell is PowerShell. The shell path picks the
// function syntax (see GrepShadowFunction); on Windows (Git Bash) the binary
// path is written in the /c/... form that shell understands.
func ShadowGrep(inv ShellInvocation, ugrepPath string) ShellInvocation {
	if ugrepPath == "" || inv.PowerShell || len(inv.Args) == 0 || !posixShell(inv.Path) {
		return inv
	}
	args := append([]string(nil), inv.Args...)
	args[len(args)-1] = GrepShadowPrefix(inv.Path, ugrepPath) + args[len(args)-1]
	inv.Args = args
	return inv
}

// shellBase is the lower-cased program name of a shell path, without ".exe".
func shellBase(shellPath string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(shellPath, `\`, "/")))
	return strings.TrimSuffix(base, ".exe")
}

// posixShell reports whether the shell at shellPath runs POSIX shell syntax
// (sh, dash, ash, bash, zsh, ksh), the shells the grep function is written
// for.
func posixShell(shellPath string) bool {
	switch b := shellBase(shellPath); {
	case b == "sh", b == "dash", b == "ash", b == "ksh", b == "mksh":
		return true
	default:
		return strings.Contains(b, "bash") || strings.Contains(b, "zsh")
	}
}

// GrepShadowPrefix is the grep function text for a command run by the shell
// at shellPath.
func GrepShadowPrefix(shellPath, ugrepPath string) string {
	base := shellBase(shellPath)
	keyword := strings.Contains(base, "bash") || strings.Contains(base, "zsh")
	if isWindowsPath(ugrepPath) {
		ugrepPath = msysPath(ugrepPath)
	}
	return GrepShadowFunction(ugrepPath, keyword)
}

// isWindowsPath reports a drive-letter ("C:\...") or UNC ("\\host\...") path.
func isWindowsPath(p string) bool {
	return (len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')) || strings.HasPrefix(p, `\\`)
}

// msysPath converts a Windows path to the form Git Bash uses:
// C:\Users\me\x.exe → /c/Users/me/x.exe, \\host\share\x → //host/share/x.
func msysPath(p string) string {
	if strings.HasPrefix(p, `\\`) {
		return strings.ReplaceAll(p, `\`, "/")
	}
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	return "/" + strings.ToLower(p[:1]) + rest
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
