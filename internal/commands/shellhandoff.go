package commands

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// ErrShellHandOff is returned (possibly wrapped) by a ShellAuthorizer to hand a
// `!`-block to the MODEL instead of running it. ExpandShellBlocks then leaves
// the block unrun and replaces it with an instruction asking the
// model to run the command itself, through its Bash tool — where the ordinary
// tool-call permission checks apply again. It is only ever produced for a block
// that was NOT approved but also NOT denied: a deny verdict is a hard failure
// and is never handed off.
var ErrShellHandOff = errors.New("shell command handed to the model")

// handOffTool is the tool a handed-off command is meant for. There is one
// shell tool (Bash, which itself runs PowerShell on Windows), so the
// "with the <tool> tool" variants never trigger.
const handOffTool = "Bash"

// handOffEntry is one handed-off block.
type handOffEntry struct {
	index   int
	at      int
	command string
}

// handOffMarker is the placeholder a handed-off block is replaced with until
// all blocks are processed.
func handOffMarker(i int) string {
	return "\x00promptShellHandOff:" + strconv.Itoa(i) + "\x00"
}

// needsFence reports whether a command must be shown in a fenced block rather
// than inline backticks (it contains a backtick or newline).
func needsFence(cmd string) bool {
	return strings.ContainsAny(cmd, "`\n")
}

// renderHandOff replaces the hand-off markers in body with the model-facing
// instructions. One handed-off command gets an inline "run this first"
// instruction in its place; several get a numbered header prepended to the
// prompt and an "[output of command N]" reference in each place, numbered in
// body order.
func renderHandOff(body string, handed []handOffEntry) string {
	if len(handed) == 0 {
		return body
	}
	sort.SliceStable(handed, func(i, j int) bool { return handed[i].at < handed[j].at })
	if len(handed) == 1 {
		return strings.Replace(body, handOffMarker(handed[0].index), handOffInstruction(handed[0].command, handOffTool), 1)
	}
	cmds := make([]string, len(handed))
	for n, e := range handed {
		cmds[n] = e.command
		body = strings.Replace(body, handOffMarker(e.index), handOffReference(e.command, n+1), 1)
	}
	return handOffHeader(cmds, handOffTool) + "\n\n" + body
}

// handOffInstruction is the in-place instruction for a single handed-off
// command.
func handOffInstruction(cmd, tool string) string {
	lead := "run this first, exactly as written, and use its output:"
	if tool != handOffTool {
		lead = "run this first, exactly as written, with the " + tool + " tool and use its output:"
	}
	if needsFence(cmd) {
		return "[" + lead + "]\n```\n" + cmd + "\n```"
	}
	return "[" + lead + " `" + cmd + "`]"
}

// handOffReference stands in for the n-th of several handed-off commands.
func handOffReference(cmd string, n int) string {
	if needsFence(cmd) {
		return "[output of command " + strconv.Itoa(n) + "]"
	}
	return "[output of command " + strconv.Itoa(n) + ", `" + cmd + "`]"
}

// handOffHeader is the numbered instruction prepended when several commands
// are handed off. All entries share one tool.
func handOffHeader(cmds []string, tool string) string {
	with := ""
	if tool != handOffTool {
		with = " with the " + tool + " tool"
	}
	joiner := ";"
	if tool == handOffTool {
		joiner = "&&"
	}
	lines := make([]string, 0, len(cmds)+1)
	lines = append(lines, "[Run these "+strconv.Itoa(len(cmds))+" commands first, exactly as written"+with+
		", and use their output where each is named below. Run them one per call, or all in one "+tool+" call joined with "+joiner+".]")
	for i, c := range cmds {
		if needsFence(c) {
			lines = append(lines, strconv.Itoa(i+1)+".\n```\n"+c+"\n```")
			continue
		}
		lines = append(lines, strconv.Itoa(i+1)+". `"+c+"`")
	}
	return strings.Join(lines, "\n")
}
