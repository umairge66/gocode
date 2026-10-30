package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

// HostSpawnRequest describes one pty-host launch.
type HostSpawnRequest struct {
	// Exe is the gocode binary that runs `--bg-pty-host`.
	Exe     string
	PTYSock string
	Cols    int
	Rows    int
	// File/Args are the worker the host runs in its terminal.
	File string
	Args []string
	Dir  string
	Env  []string
	// ErrPath receives the host's stderr (the crash breadcrumb).
	ErrPath string
}

// HostSpawner starts pty hosts (injectable for tests).
type HostSpawner func(HostSpawnRequest) (HostProcess, error)

type execHost struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (h *execHost) Pid() int              { return h.cmd.Process.Pid }
func (h *execHost) Done() <-chan struct{} { return h.done }
func (h *execHost) ExitCode() int         { return h.code }

// SpawnHost launches `<exe> --bg-pty-host <sock> <cols> <rows> -- <file>
// [args...]` detached (its own session / process group), stdin and stdout
// discarded and stderr appended to the .err breadcrumb.
func SpawnHost(req HostSpawnRequest) (HostProcess, error) {
	args := append([]string{"--bg-pty-host", req.PTYSock, strconv.Itoa(req.Cols), strconv.Itoa(req.Rows), "--", req.File}, req.Args...)
	cmd := exec.Command(req.Exe, args...)
	if runtime.GOOS != "windows" {
		cmd.Args[0] = "gocode bg-pty-host"
	}
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	var errFile *os.File
	if req.ErrPath != "" {
		_ = os.MkdirAll(filepath.Dir(req.ErrPath), 0o700)
		if f, err := os.OpenFile(req.ErrPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
			errFile = f
			cmd.Stderr = f
		}
	}
	Detach(cmd)
	err := cmd.Start()
	if errFile != nil {
		errFile.Close()
	}
	if err != nil {
		return nil, err
	}
	h := &execHost{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			h.code = cmd.ProcessState.ExitCode()
		} else {
			h.code = -1
		}
		close(h.done)
	}()
	return h, nil
}
