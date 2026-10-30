//go:build linux || darwin

package daemon

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// unixTerminal is a child running on a /dev/ptmx pseudo-terminal. The child is
// started with setsid + TIOCSCTTY so it leads its own session and process group
// with the pty as its controlling terminal; signals go to that whole group.
type unixTerminal struct {
	master *os.File
	cmd    *exec.Cmd

	code   int
	signal string
	done   chan struct{}
}

func startTerminal(spec TerminalSpec) (Terminal, error) {
	master, slaveName, err := openPTY()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close()
	if err := setWinsize(master, spec.Cols, spec.Rows); err != nil {
		master.Close()
		return nil, err
	}
	cmd := exec.Command(spec.File, spec.Args...)
	if spec.Argv0 != "" {
		cmd.Args[0] = spec.Argv0
	}
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Session leader with the pty as controlling terminal (Ctty is the child's
	// fd 0). setsid already makes it a process-group leader, so this child's
	// group is signalled as one unit; proctree.Prepare is deliberately not used
	// here because setpgid after setsid fails with EPERM.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	t := &unixTerminal{master: master, cmd: cmd, done: make(chan struct{})}
	go t.reap()
	return t, nil
}

func (t *unixTerminal) reap() {
	err := t.cmd.Wait()
	code, sig := 0, ""
	if t.cmd.ProcessState != nil {
		if ws, ok := t.cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			switch {
			case ws.Signaled():
				sig = unixSignalName(ws.Signal())
				code = 128 + int(ws.Signal())
			case ws.Exited():
				code = ws.ExitStatus()
			}
		} else {
			code = t.cmd.ProcessState.ExitCode()
		}
	} else if err != nil {
		code = -1
	}
	t.code, t.signal = code, sig
	close(t.done)
}

func (t *unixTerminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	if err != nil && isPTYEOF(err) {
		return n, io.EOF
	}
	return n, err
}

func (t *unixTerminal) Write(p []byte) (int, error) { return t.master.Write(p) }

func (t *unixTerminal) Resize(cols, rows int) error { return setWinsize(t.master, cols, rows) }

func (t *unixTerminal) Pid() int { return t.cmd.Process.Pid }

func (t *unixTerminal) Signal(sig string) error {
	s := unixSignal(sig)
	if s == 0 {
		return errors.New("unsupported signal " + sig)
	}
	pid := t.cmd.Process.Pid
	if err := unix.Kill(-pid, s); err != nil {
		return unix.Kill(pid, s)
	}
	return nil
}

func (t *unixTerminal) Wait() (int, string) {
	<-t.done
	return t.code, t.signal
}

func (t *unixTerminal) Close() error { return t.master.Close() }

func setWinsize(f *os.File, cols, rows int) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	cerr := rc.Control(func(fd uintptr) {
		ierr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	})
	if cerr != nil {
		return cerr
	}
	return ierr
}

// isPTYEOF reports the read errors a pty master returns once every slave fd
// is closed (EIO on Linux, EOF elsewhere).
func isPTYEOF(err error) bool {
	if errors.Is(err, syscall.EIO) {
		return true
	}
	var pe *os.PathError
	if errors.As(err, &pe) && errors.Is(pe.Err, syscall.EIO) {
		return true
	}
	return errors.Is(err, io.EOF)
}

func unixSignal(name string) syscall.Signal {
	switch name {
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGINT":
		return syscall.SIGINT
	case "SIGQUIT":
		return syscall.SIGQUIT
	case "SIGHUP":
		return syscall.SIGHUP
	case "SIGWINCH":
		return syscall.SIGWINCH
	}
	return 0
}

func unixSignalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGBUS:
		return "SIGBUS"
	}
	return "SIG" + s.String()
}
