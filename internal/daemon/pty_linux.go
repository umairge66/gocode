//go:build linux

package daemon

import (
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPTY opens a new pty master and returns it with its slave path
// (posix_openpt + unlockpt + ptsname, via TIOCSPTLCK/TIOCGPTN).
func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	rc, err := master.SyscallConn()
	if err != nil {
		master.Close()
		return nil, "", err
	}
	var n uint32
	var ierr error
	if cerr := rc.Control(func(fd uintptr) {
		if ierr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ierr != nil {
			return
		}
		n, ierr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
	}); cerr != nil {
		master.Close()
		return nil, "", cerr
	}
	if ierr != nil {
		master.Close()
		return nil, "", ierr
	}
	return master, "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
