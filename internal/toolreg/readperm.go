package toolreg

import (
	"path/filepath"
	"strings"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// readToolDecision is the tail of the read tools' own permission check,
// reached once no deny / ask / allow settings rule decided:
//
//   - a read inside the working directories is allowed, in every mode
//     (decisionReason {type:"mode",mode:"default"});
//   - an Edit allow rule covering the path implies read ("editImpliesRead");
//   - anything else asks with a workingDir reason and the message "gocode
//     requested permissions to read from <path>, but you haven't granted it
//     yet.".
//
// The mode then shapes the ask: auto mode allows it — Read,
// Glob and Grep are on the auto mode safe allowlist — and
// dontAsk denies it (via modeDefault). ok=false falls through to modeDefault.
//
// In auto mode there is no one-time offer to turn on
// blockReadsOutsideWorkingDirectories; the read is simply allowed.
func (d *Decider) readToolDecision(call Call, mode Mode, rules RuleSet, sp *spellingCache) (Decision, bool) {
	if call.Path == "" || d.readInsideWorkingDirs(call.Path) || d.readInsideReadableDirs(call.Path) {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: string(call.Tool) + " reads inside the working directories.", Mode: ModeDefault}, true
	}
	// The session's own plan files are always readable.
	if d.Files != nil && d.Files.IsPlanFile(call.Path) {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: pathguard.MsgPlanFileReadable}, true
	}
	// So is the session scratchpad.
	if d.Files != nil && d.Files.IsScratchpad(call.Path) {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: pathguard.MsgScratchpadReadable}, true
	}
	edit := Call{Tool: protocol.ToolEdit, Rule: string(protocol.ToolEdit), Path: call.Path, Op: pathguard.OpWrite, PathCanonical: call.PathCanonical}
	if r := d.matchRule(rules, edit, &spellingCache{}, RuleAllow); r != nil {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonRule, Reason: string(call.Tool) + " is allowed by an Edit allow rule (edit implies read).", Rule: r}, true
	}
	switch mode {
	case ModeAuto:
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: string(call.Tool) + " is on the auto mode safe allowlist.", Mode: ModeAuto}, true
	case ModeDontAsk, ModeBypassPermissions:
		return Decision{}, false
	}
	return Decision{
		Behavior:   BehaviorAsk,
		ReasonType: ReasonWorkingDir,
		Reason:     MsgReadNotGranted(call.Path),
		Mode:       mode,
	}, true
}

// MsgReadNotGranted is the outside-read ask message.
func MsgReadNotGranted(path string) string {
	return "gocode requested permissions to read from " + pathguard.EscapeControl(path) + ", but you haven't granted it yet."
}

// readInsideWorkingDirs reports whether a read path (absolute, ~-relative or
// relative to the session's working directory) lies inside the working
// directories.
func (d *Decider) readInsideWorkingDirs(p string) bool {
	abs := d.absReadPath(expandHomePath(p))
	if abs == "" {
		return false
	}
	return d.withinWorkingDirs(abs)
}

// autoModeAllowlisted is auto mode's safe allowlist: these tools skip the
// classifier (Read, Grep, Glob, LSP, ToolSearch, ListMcpResourcesTool,
// ReadMcpResourceTool, ReadMcpResourceDirTool, TodoWrite, the Task tools,
// EnterPlanMode, ExitPlanMode, and the browser read tools).
func autoModeAllowlisted(t protocol.Tool) bool {
	switch t {
	case protocol.ToolRead, protocol.ToolGlob, protocol.ToolGrep,
		protocol.ToolSearch, protocol.ToolTodoWrite,
		protocol.ToolBrowserReadPage, protocol.ToolBrowserGetPageText,
		protocol.ToolBrowserTabsContext, protocol.ToolBrowserFind:
		return true
	}
	switch string(t) {
	case "LSP", "ListMcpResourcesTool", "ReadMcpResourceTool", "ReadMcpResourceDirTool",
		"TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "TaskStop", "GetTask",
		"EnterPlanMode", "ExitPlanMode":
		return true
	}
	return false
}

// readInsideReadableDirs reports whether p lies inside one of ReadableDirs.
func (d *Decider) readInsideReadableDirs(p string) bool {
	if len(d.ReadableDirs) == 0 {
		return false
	}
	abs := d.absReadPath(expandHomePath(p))
	if abs == "" {
		return false
	}
	for _, dir := range d.ReadableDirs {
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(dir), abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
