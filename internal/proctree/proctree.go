// Package proctree starts child processes in their own process group and tears
// the whole group down with a graceful-then-forceful escalation.
//
// Escalation:
//
//  1. The shell is started in a new session and therefore a new process group
//     whose id equals the shell's pid.
//  2. Graceful step: SIGTERM is sent to the whole group, falling back to the
//     pid alone when the group signal fails.
//  3. While waiting, every 100ms the group and the leader are probed with
//     signal 0; once nothing answers the process counts as exited cleanly
//     (no SIGKILL).
//  4. After 1500ms SIGKILL is sent to the group.
//
// Notes:
//
//   - Descendants that left the group (setsid'd daemons) are not enumerated via
//     `ps`; only the process group is signalled.
//   - On Windows there is a graceful step: CTRL_BREAK_EVENT to the
//     child's console process group (it is started with
//     CREATE_NEW_PROCESS_GROUP), then a forceful tree kill with
//     `taskkill /T /F /PID <pid>`, falling back to TerminateProcess on the
//     leader.
package proctree

import (
	"errors"
	"os/exec"
	"time"
)

// DefaultGrace is how long the graceful signal is given before the group is
// force-killed.
const DefaultGrace = 1500 * time.Millisecond

// DefaultPoll is how often the group is probed during the grace period.
const DefaultPoll = 100 * time.Millisecond

// DefaultForceWait bounds how long Terminate waits for the process to be
// reaped after the forceful kill, so a caller never hangs on a process that
// nobody waits for.
const DefaultForceWait = 2 * time.Second

// Outcome is the final state reported by Terminate.
type Outcome int

const (
	// OutcomeAlreadyExited: nothing was running when Terminate was called; no
	// signal was sent.
	OutcomeAlreadyExited Outcome = iota
	// OutcomeGraceful: the group exited within the grace period after the
	// graceful signal; no forceful kill was needed.
	OutcomeGraceful
	// OutcomeForced: the group was still alive after the grace period (or the
	// graceful signal could not be delivered) and was force-killed.
	OutcomeForced
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAlreadyExited:
		return "already-exited"
	case OutcomeGraceful:
		return "exited"
	case OutcomeForced:
		return "killed"
	}
	return "unknown"
}

// ErrInvalidPID is returned for a pid that cannot name a child (<= 1).
var ErrInvalidPID = errors.New("proctree: invalid pid")

// Options tunes Terminate. Zero fields take the defaults.
type Options struct {
	Grace     time.Duration // DefaultGrace
	Poll      time.Duration // DefaultPoll
	ForceWait time.Duration // DefaultForceWait
}

func (o Options) withDefaults() Options {
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.ForceWait <= 0 {
		o.ForceWait = DefaultForceWait
	}
	return o
}

// ops is the per-platform seam. Tests substitute a fake to exercise the
// escalation and the selection rules without spawning anything.
type ops interface {
	// graceful asks the group led by pid to exit. An error means the signal
	// reached nothing (or cannot be delivered on this platform), and the
	// escalation proceeds straight to force.
	graceful(pid int) error
	// force kills the group (or tree) led by pid.
	force(pid int) error
	// alive reports whether the group led by pid, or the leader itself, is
	// still running.
	alive(pid int) bool
}

// Terminate runs the graceful-then-forceful escalation against the process
// group led by pid (started with Prepare).
//
// exited, when non-nil, must be closed by the caller once its Wait on the
// leader has returned; until then the leader is treated as alive (so a
// not-yet-reaped zombie is never mistaken for a running process, and the
// caller's Wait always reaps it). With a nil exited, liveness is probed only.
//
// It returns once the group is gone or has been force-killed, and never takes
// longer than Grace + ForceWait (+ one Poll).
func Terminate(pid int, exited <-chan struct{}, opts Options) (Outcome, error) {
	return terminate(platformOps, pid, exited, opts)
}

func terminate(o ops, pid int, exited <-chan struct{}, opts Options) (Outcome, error) {
	if pid <= 1 {
		return OutcomeAlreadyExited, ErrInvalidPID
	}
	opts = opts.withDefaults()

	leaderDone := func() bool {
		if exited == nil {
			return true
		}
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	gone := func() bool { return leaderDone() && !o.alive(pid) }

	if gone() {
		return OutcomeAlreadyExited, nil
	}

	// Graceful step. If it could not be delivered at all, waiting the grace
	// period is pointless: escalate immediately.
	if err := o.graceful(pid); err == nil {
		grace := time.NewTimer(opts.Grace)
		defer grace.Stop()
		tick := time.NewTicker(opts.Poll)
		defer tick.Stop()
		exitedCh := exited
	wait:
		for {
			select {
			case <-exitedCh:
				exitedCh = nil // closed; stop selecting on it
				if !o.alive(pid) {
					return OutcomeGraceful, nil
				}
			case <-tick.C:
				if gone() {
					return OutcomeGraceful, nil
				}
			case <-grace.C:
				break wait
			}
		}
		if gone() {
			return OutcomeGraceful, nil
		}
	}

	ferr := o.force(pid)

	// Bounded wait for the reap so the caller can report a final state.
	deadline := time.NewTimer(opts.ForceWait)
	defer deadline.Stop()
	tick := time.NewTicker(opts.Poll)
	defer tick.Stop()
	for !gone() {
		select {
		case <-deadline.C:
			if ferr == nil {
				ferr = errors.New("proctree: process group still alive after forceful kill")
			}
			return OutcomeForced, ferr
		case <-tick.C:
		case <-exited:
			exited = nil
		}
	}
	return OutcomeForced, nil
}

// KillGroup force-kills the group led by pid immediately, without a grace
// period.
func KillGroup(pid int) error {
	if pid <= 1 {
		return ErrInvalidPID
	}
	return platformOps.force(pid)
}

// SignalGraceful sends the graceful stop signal to the group led by pid
// (SIGTERM to the process group on unix, CTRL_BREAK_EVENT on Windows) without
// waiting. It is the first half of Terminate for callers that run their own
// grace timer (the background-session supervisor's 5 s SIGTERM -> SIGKILL).
func SignalGraceful(pid int) error {
	if pid <= 1 {
		return ErrInvalidPID
	}
	return platformOps.graceful(pid)
}

// Alive reports whether the group led by pid, or the leader, still runs.
func Alive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return platformOps.alive(pid)
}

// BindContext adapts an exec.CommandContext command so that context
// cancellation (timeout or interrupt) runs the full group escalation instead of
// os/exec's default leader-only Process.Kill. It must be called after Prepare
// and before Start. WaitDelay is set as an I/O backstop so Wait cannot hang on a
// stray descendant that keeps the output pipes open.
func BindContext(cmd *exec.Cmd, opts Options) {
	opts = opts.withDefaults()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pid := cmd.Process.Pid
		go func() { _, _ = Terminate(pid, nil, opts) }()
		return nil
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = opts.Grace + opts.ForceWait
	}
}
