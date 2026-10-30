package proctree

import "strconv"

// This file holds the platform-independent half of the Windows strategy so the
// selection logic is unit-tested on every OS; proctree_windows.go only performs
// the system calls it selects.

// createNewProcessGroup is the Windows CREATE_NEW_PROCESS_GROUP creation flag
// (0x00000200). Reproduced here so the flag arithmetic is testable off
// Windows.
const createNewProcessGroup = 0x00000200

// windowsCreationFlags returns the creation flags Prepare applies on Windows:
// the caller's flags plus CREATE_NEW_PROCESS_GROUP, which makes the child the
// root of a console process group (group id == pid) that CTRL_BREAK_EVENT can
// target without also signalling this process.
func windowsCreationFlags(existing uint32) uint32 {
	return existing | createNewProcessGroup
}

// taskkillArgs are the arguments of the forceful tree kill: /T kills the
// process and every child it started, /F forces termination.
func taskkillArgs(pid int) []string {
	return []string{"/T", "/F", "/PID", strconv.Itoa(pid)}
}

// windowsForceStep is one forceful-kill strategy, tried in order.
type windowsForceStep int

const (
	forceTaskkillTree    windowsForceStep = iota // taskkill /T /F /PID
	forceTerminateLeader                         // TerminateProcess on the leader
)

// windowsForcePlan selects the forceful strategies: the tree kill when
// taskkill.exe is available, then TerminateProcess on the leader as the
// fallback.
func windowsForcePlan(haveTaskkill bool) []windowsForceStep {
	if haveTaskkill {
		return []windowsForceStep{forceTaskkillTree, forceTerminateLeader}
	}
	return []windowsForceStep{forceTerminateLeader}
}
