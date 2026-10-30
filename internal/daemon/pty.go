package daemon

import (
	"errors"
	"io"
)

// ErrPTYUnsupported reports that this platform has no pseudo-terminal support
// for background sessions.
var ErrPTYUnsupported = errors.New("daemon: pseudo-terminals are not supported on this platform")

// -----------------------------------------------------------------------------
// Terminal: the pseudo-terminal a background session runs in.
//
// The pty host runs the worker under a pseudo-terminal (a POSIX pty on
// macOS/Linux, ConPTY on Windows) with TERM=xterm-256color, and kills/resizes
// it through that terminal. This is the OS seam: pty_unix.go (Linux/macOS
// /dev/ptmx), pty_windows.go (ConPTY via golang.org/x/sys/windows),
// pty_other.go (unsupported).

// Terminal is a running child attached to a pseudo-terminal.
type Terminal interface {
	// Read returns child output; io.EOF once the terminal is drained after the
	// child exited.
	io.Reader
	// Write feeds input to the child.
	io.Writer
	// Resize changes the terminal size.
	Resize(cols, rows int) error
	// Pid is the child's process id (the leader of its process group).
	Pid() int
	// Signal delivers SIGTERM/SIGKILL/SIGINT/SIGQUIT/SIGHUP/SIGWINCH to the
	// child's process group (Windows: SIGKILL/SIGTERM terminate the tree).
	Signal(sig string) error
	// Wait blocks until the child exits and reports its exit code and, when it
	// was killed by a signal, the signal name.
	Wait() (code int, signal string)
	// Close releases the terminal (closing a ConPTY also ends its client).
	Close() error
}

// TerminalSpec describes the child to start.
type TerminalSpec struct {
	File string
	Args []string
	Dir  string
	Env  []string
	Cols int
	Rows int
	// Argv0 overrides the process title on unix.
	Argv0 string
}

// StartTerminal starts spec under a new pseudo-terminal.
func StartTerminal(spec TerminalSpec) (Terminal, error) {
	if spec.Cols <= 0 {
		spec.Cols = 200
	}
	if spec.Rows <= 0 {
		spec.Rows = 50
	}
	return startTerminal(spec)
}
