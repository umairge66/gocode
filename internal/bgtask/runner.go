package bgtask

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
)

// Runner starts a shell command and hands back a handle to the running process.
// It is injectable so tests can substitute a fake, harmless process and so the
// shell selection stays in one place.
type Runner interface {
	// Start launches command, wiring the process's stdout and stderr into the
	// provided writers (both may be the same writer for a merged tail). It
	// returns a handle to the started process. ctx cancellation should
	// terminate the process.
	Start(ctx context.Context, command string, stdout, stderr io.Writer) (Process, error)
}

// Process is a handle to a started background process.
type Process interface {
	// PID reports the process id.
	PID() int
	// Wait blocks until the process exits and returns its exit code. A non-nil
	// error indicates the process could not be awaited (distinct from an
	// ordinary non-zero exit, which is reported via the code with a nil error).
	Wait() (exitCode int, err error)
	// Kill terminates the process.
	Kill() error
}

// Terminator is implemented by processes that support the
// graceful-then-forceful group escalation. The Manager prefers it over
// Process.Kill, which remains the minimal contract for fakes.
type Terminator interface {
	// Terminate signals the process group gracefully, waits up to grace for
	// it to exit, then force-kills it, reporting which of those happened.
	Terminate(grace time.Duration) (proctree.Outcome, error)
}

// Lingerer is implemented by processes that can report descendants still
// holding the task's output after the leader exited. Shutdown terminates such
// groups too.
type Lingerer interface {
	Lingering() bool
}

// OSRunner is the default Runner: it runs commands through the platform shell
// with os/exec, each in its own process group (see proctree.Prepare).
type OSRunner struct {
	// Grace is the SIGTERM -> SIGKILL grace used when ctx is cancelled or Kill
	// is called. Zero means DefaultKillGrace.
	Grace time.Duration
}

const (
	shellPOSIX    = "/bin/sh"
	shellArgPOSIX = "-c"

	shellPwsh       = "pwsh"
	shellPowerShell = "powershell"
	shellArgPwsh    = "-Command"
)

// outputDrainDelay bounds how long Wait keeps collecting output after the
// shell exits while a descendant still holds the output pipe open (e.g.
// `server &`). Wait then returns, and the descendant's output keeps streaming
// into the tail until it closes the pipe or the task is terminated.
const outputDrainDelay = 500 * time.Millisecond

func shellCommand() (bin, arg string) {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath(shellPwsh); err == nil {
			return shellPwsh, shellArgPwsh
		}
		return shellPowerShell, shellArgPwsh
	}
	return shellPOSIX, shellArgPOSIX
}

// Start implements Runner using os/exec. The shell becomes the leader of a new
// process group so the whole tree can be terminated; cancelling ctx runs the
// graceful-then-forceful escalation.
//
// Output goes through pipes this package owns (not os/exec's internal copy
// goroutines), so the leader's exit is observed promptly even when a
// descendant keeps the pipe open, and the pipe can be closed on termination
// so nothing blocks forever.
func (r OSRunner) Start(ctx context.Context, command string, stdout, stderr io.Writer) (Process, error) {
	bin, arg := shellCommand()
	cmd := exec.Command(bin, arg, command)
	// Background tasks and monitors never need gocode's own provider keys.
	cmd.Env = envsec.ChildEnv(nil)
	proctree.Prepare(cmd)

	p := &osProcess{
		cmd:    cmd,
		grace:  r.Grace,
		exited: make(chan struct{}),
		ioDone: make(chan struct{}),
	}

	var writeEnds []*os.File
	closeAll := func() {
		for _, f := range writeEnds {
			_ = f.Close()
		}
		for _, f := range p.readEnds {
			_ = f.Close()
		}
	}
	pipeFor := func(w io.Writer) (*os.File, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		p.readEnds = append(p.readEnds, pr)
		writeEnds = append(writeEnds, pw)
		p.copiers.Add(1)
		go p.copy(w, pr)
		return pw, nil
	}

	if stdout != nil {
		w, err := pipeFor(stdout)
		if err != nil {
			closeAll()
			return nil, err
		}
		cmd.Stdout = w
	}
	if stderr != nil {
		if stderr == stdout && cmd.Stdout != nil {
			// One pipe for a merged tail keeps the interleaving order.
			cmd.Stderr = cmd.Stdout
		} else {
			w, err := pipeFor(stderr)
			if err != nil {
				closeAll()
				return nil, err
			}
			cmd.Stderr = w
		}
	}

	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child holds its own copies of the write ends; ours must be closed so
	// the copiers see EOF once every writer in the tree is gone.
	for _, f := range writeEnds {
		_ = f.Close()
	}
	go func() {
		p.copiers.Wait()
		close(p.ioDone)
	}()

	go p.reapLeader()

	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				_, _ = p.Terminate(p.grace)
			case <-p.exited:
			}
		}()
	}
	return p, nil
}

type osProcess struct {
	cmd   *exec.Cmd
	grace time.Duration

	readEnds []*os.File
	copiers  sync.WaitGroup

	exited chan struct{} // closed once the leader has been reaped
	ioDone chan struct{} // closed once every output pipe reached EOF/closed

	code    int
	waitErr error

	termMu sync.Mutex // serializes Terminate
}

func (p *osProcess) copy(w io.Writer, r *os.File) {
	defer p.copiers.Done()
	_, _ = io.Copy(w, r)
	_ = r.Close()
}

// reapLeader is the single cmd.Wait caller; it always runs, so the leader can
// never be left a zombie regardless of whether anyone calls Wait.
func (p *osProcess) reapLeader() {
	err := p.cmd.Wait()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.code = ee.ExitCode()
		} else {
			p.code, p.waitErr = -1, err
		}
	}
	close(p.exited)
}

func (p *osProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Wait blocks until the leader exits, then gives the output pipes up to
// outputDrainDelay to reach EOF so the tail is complete for the common case.
// It may be called more than once.
func (p *osProcess) Wait() (int, error) {
	<-p.exited
	t := time.NewTimer(outputDrainDelay)
	defer t.Stop()
	select {
	case <-p.ioDone:
	case <-t.C:
	}
	return p.code, p.waitErr
}

// Lingering reports that the leader has exited but something in its tree
// still holds the output pipe open.
func (p *osProcess) Lingering() bool {
	select {
	case <-p.exited:
	default:
		return false
	}
	select {
	case <-p.ioDone:
		return false
	default:
		return true
	}
}

// Terminate runs the escalation against the process group: SIGTERM
// to the group, up to grace (DefaultKillGrace when <= 0) for it to exit, then
// SIGKILL to the group. Afterwards any output pipe still held open by an
// escaped descendant is closed, so no goroutine is left blocked.
func (p *osProcess) Terminate(grace time.Duration) (proctree.Outcome, error) {
	if p.cmd.Process == nil {
		return proctree.OutcomeAlreadyExited, nil
	}
	if grace <= 0 {
		grace = DefaultKillGrace
	}
	p.termMu.Lock()
	defer p.termMu.Unlock()

	outcome, err := proctree.Terminate(p.cmd.Process.Pid, p.exited, proctree.Options{Grace: grace})

	// Let the pipes drain briefly, then force them closed.
	t := time.NewTimer(outputDrainDelay)
	select {
	case <-p.ioDone:
	case <-t.C:
		for _, f := range p.readEnds {
			_ = f.Close()
		}
	}
	t.Stop()
	return outcome, err
}

// Kill terminates the process group with the default escalation.
func (p *osProcess) Kill() error {
	_, err := p.Terminate(p.grace)
	return err
}
