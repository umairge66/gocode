//go:build windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/umairge66/gocode/internal/proctree"
)

// conptyTerminal is a child attached to a Windows pseudo console (ConPTY).
type conptyTerminal struct {
	hpc     windows.Handle
	in      *os.File // write end feeding the console input
	out     *os.File // read end of the console output
	process windows.Handle
	pid     int

	closeOnce sync.Once
	code      int
	done      chan struct{}
}

func startTerminal(spec TerminalSpec) (Terminal, error) {
	path, err := exec.LookPath(spec.File)
	if err != nil {
		return nil, err
	}
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, err
	}
	closeAll := func() {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			if h != 0 {
				windows.CloseHandle(h)
			}
		}
	}
	var hpc windows.Handle
	size := windows.Coord{X: int16(spec.Cols), Y: int16(spec.Rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		closeAll()
		return nil, err
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself; reinterpret its storage as an
	// unsafe.Pointer (not a uintptr conversion).
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	si := new(windows.StartupInfoEx)
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(*si))
	si.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = attrs.List()

	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{path}, spec.Args...)))
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	appName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	var dir *uint16
	if spec.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(spec.Dir); err != nil {
			windows.ClosePseudoConsole(hpc)
			closeAll()
			return nil, err
		}
	}
	var envBlock *uint16
	if spec.Env != nil {
		envBlock = environmentBlock(spec.Env)
	}
	pi := new(windows.ProcessInformation)
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(appName, cmdline, nil, nil, false, flags, envBlock, dir, &si.StartupInfo, pi); err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	// The console duplicated its ends; drop ours so EOF propagates.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	t := &conptyTerminal{
		hpc:     hpc,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
		process: pi.Process,
		pid:     int(pi.ProcessId),
		done:    make(chan struct{}),
	}
	go t.reap()
	return t, nil
}

// environmentBlock renders KEY=VALUE pairs as a double-NUL-terminated UTF-16
// block.
func environmentBlock(env []string) *uint16 {
	var b []uint16
	for _, kv := range env {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	b = append(b, 0)
	if len(b) == 1 {
		b = append(b, 0)
	}
	return &b[0]
}

func (t *conptyTerminal) reap() {
	_, _ = windows.WaitForSingleObject(t.process, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(t.process, &code); err != nil {
		t.code = -1
	} else {
		t.code = int(int32(code))
	}
	windows.CloseHandle(t.process)
	close(t.done)
}

func (t *conptyTerminal) Read(p []byte) (int, error) { return t.out.Read(p) }

func (t *conptyTerminal) Write(p []byte) (int, error) { return t.in.Write(p) }

func (t *conptyTerminal) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(t.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (t *conptyTerminal) Pid() int { return t.pid }

// Signal maps the POSIX names onto ConPTY: SIGINT/SIGQUIT become ^C/^\ on the
// console input, SIGTERM closes the pseudo console,
// SIGKILL kills the process tree via proctree.
func (t *conptyTerminal) Signal(sig string) error {
	switch sig {
	case "SIGINT":
		_, err := t.in.Write([]byte{3})
		return err
	case "SIGQUIT":
		_, err := t.in.Write([]byte{28})
		return err
	case "SIGTERM", "SIGHUP":
		return t.Close()
	case "SIGKILL":
		return proctree.KillGroup(t.pid)
	case "SIGWINCH":
		return nil
	}
	return errors.New("unsupported signal " + sig)
}

func (t *conptyTerminal) Wait() (int, string) {
	<-t.done
	return t.code, ""
}

func (t *conptyTerminal) Close() error {
	t.closeOnce.Do(func() {
		windows.ClosePseudoConsole(t.hpc)
		t.in.Close()
	})
	return nil
}
