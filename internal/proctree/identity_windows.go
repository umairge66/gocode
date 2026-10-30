//go:build windows

package proctree

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// startTime is the process creation time (100 ns ticks).
func startTime(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return ""
	}
	return strconv.FormatInt(c.Nanoseconds(), 10)
}

// ownedBySelf compares the process token's user SID with ours.
func ownedBySelf(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return false
	}
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return windows.EqualSid(tu.User.Sid, me.User.Sid)
}
