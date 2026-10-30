package conversation

import (
	"regexp"
	"strings"
)

// ShellTextKind classifies a text block written for a shell-mode exchange.
type ShellTextKind int

const (
	// ShellTextNone is any other text.
	ShellTextNone ShellTextKind = iota
	// ShellTextCaveat is the note that the exchange was not a request.
	ShellTextCaveat
	// ShellTextInput is the <bash-input> block carrying the command.
	ShellTextInput
	// ShellTextOutput is the <bash-stdout>/<bash-stderr> block.
	ShellTextOutput
	// ShellTextInterrupted is the marker of a command the user stopped.
	ShellTextInterrupted
)

// ShellText is a parsed shell-mode text block.
type ShellText struct {
	Kind ShellTextKind
	// Command is the command line of a ShellTextInput block.
	Command string
	// Stdout and Stderr are the output of a ShellTextOutput block, with the
	// escaping applied when it was recorded undone.
	Stdout string
	Stderr string
}

var (
	shellInputRE  = regexp.MustCompile(`^\s*<bash-input>([\s\S]*?)</bash-input>\s*$`)
	shellOutputRE = regexp.MustCompile(`^\s*<bash-stdout>([\s\S]*?)</bash-stdout>\s*(?:<bash-stderr>([\s\S]*?)</bash-stderr>)?\s*$`)
)

// ParseShellText classifies a text block of a user turn as one of the blocks
// a shell-mode exchange records. ok is false for any other text.
func ParseShellText(text string) (ShellText, bool) {
	t := strings.TrimSpace(text)
	switch {
	case t == shellCaveat:
		return ShellText{Kind: ShellTextCaveat}, true
	case t == shellInterrupted:
		return ShellText{Kind: ShellTextInterrupted}, true
	}
	if m := shellInputRE.FindStringSubmatch(t); m != nil {
		return ShellText{Kind: ShellTextInput, Command: strings.TrimSpace(m[1])}, true
	}
	if m := shellOutputRE.FindStringSubmatch(t); m != nil {
		return ShellText{Kind: ShellTextOutput, Stdout: unescapeShellText(m[1]), Stderr: unescapeShellText(m[2])}, true
	}
	return ShellText{}, false
}

// unescapeShellText reverses escapeShellText.
func unescapeShellText(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}

// ShellOutputFailed reports whether recorded stderr ends with the exit-code or
// timeout line added for a failed command.
func ShellOutputFailed(stderr string) bool {
	last := stderr
	if i := strings.LastIndexByte(stderr, '\n'); i >= 0 {
		last = stderr[i+1:]
	}
	return last == "Command timed out" || shellExitLineRE.MatchString(last)
}

var shellExitLineRE = regexp.MustCompile(`^Exit code -?\d+$`)
