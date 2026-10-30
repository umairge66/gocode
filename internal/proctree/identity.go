package proctree

// -----------------------------------------------------------------------------
// Process identity: a start-time token that tells a process apart from a later
// one reusing its pid, and an owner check, so a supervisor that re-adopts
// workers from a roster never signals or attaches to someone else's process.

// StartTime returns an opaque token for the start of process pid, or "" when
// it cannot be read (no such process, or not supported on this platform).
func StartTime(pid int) string {
	if pid <= 1 {
		return ""
	}
	return startTime(pid)
}

// OwnedBySelf reports whether pid is a live process that belongs to the
// current user.
func OwnedBySelf(pid int) bool {
	if pid <= 1 {
		return false
	}
	return ownedBySelf(pid)
}

// SameProcess reports whether pid is still the process whose start token was
// recorded as start. An empty recorded token cannot be verified and fails.
func SameProcess(pid int, start string) bool {
	if start == "" {
		return false
	}
	cur := StartTime(pid)
	return cur != "" && cur == start
}
