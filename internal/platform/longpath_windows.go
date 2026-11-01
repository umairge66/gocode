package platform

import (
	"strings"

	"golang.org/x/sys/windows"
)

// LongPath expands the 8.3 short-name components of an existing path to
// their long form. A path that does not exist, or has no short-name
// component, is returned unchanged.
func LongPath(p string) string {
	if !strings.Contains(p, "~") {
		return p
	}
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 260)
	for {
		n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return p
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
}
