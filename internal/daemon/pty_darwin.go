//go:build darwin

package daemon

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a new pty master and returns it with its slave path
// (posix_openpt + grantpt + unlockpt + ptsname, via TIOCPTYGRANT/TIOCPTYUNLK/
// TIOCPTYGNAME).
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
	var name string
	var ierr error
	if cerr := rc.Control(func(fd uintptr) {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TIOCPTYGRANT), 0); e != 0 {
			ierr = e
			return
		}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TIOCPTYUNLK), 0); e != 0 {
			ierr = e
			return
		}
		buf := make([]byte, 128)
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
			ierr = e
			return
		}
		if i := bytes.IndexByte(buf, 0); i >= 0 {
			buf = buf[:i]
		}
		name = string(buf)
	}); cerr != nil {
		master.Close()
		return nil, "", cerr
	}
	if ierr != nil {
		master.Close()
		return nil, "", ierr
	}
	return master, name, nil
}
