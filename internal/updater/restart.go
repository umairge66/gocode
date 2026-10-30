package updater

import (
	"errors"
	"os"
)

// -----------------------------------------------------------------------------
// Self-restart after an in-place update.
//
// Once the freshly downloaded binary has atomically replaced the running
// executable, the auto-updater hands control to the new binary by re-executing
// it with the original arguments and environment. On unix this is a true
// in-place replacement of the process image (syscall.Exec), so the call never
// returns on success. Windows has no in-place re-exec, so there the handoff
// instead spawns the freshly installed binary as a new process (carrying the
// same argv and environment) and then exits the current process so the new one
// takes over — see restart_windows.go.

// errHandoffExit is the sentinel an ExecFunc returns once it has successfully
// spawned a detached replacement process instead of replacing the current
// process image (the Windows handoff). It is not a failure: it tells Restart to
// terminate the current process through its injected Exit seam so the freshly
// spawned binary takes over. It never escapes Restart.
var errHandoffExit = errors.New("updater: restart spawned a detached process; exit now")

// ExecFunc replaces the running process image with the binary at argv0, passing
// argv and env. It mirrors the signature of syscall.Exec: on a successful unix
// exec it never returns. It is the injectable seam that lets tests assert the
// exec argv/env without actually replacing the test process.
type ExecFunc func(argv0 string, argv []string, env []string) error

// Restarter re-execs the freshly installed binary after an in-place update. Its
// fields are seams for tests; left zero, it uses the real process args, env, and
// the platform's exec primitive.
type Restarter struct {
	// Exec performs the re-exec; nil uses the platform default (syscall.Exec on
	// unix, an ErrRestartUnsupported stub elsewhere).
	Exec ExecFunc
	// Args is the argument vector to restart with; nil uses os.Args. The first
	// element is replaced with the installed executable's path.
	Args []string
	// Env is the environment to restart with; nil uses os.Environ().
	Env []string
	// Exit terminates the current process after a successful detached-spawn
	// handoff (Windows), so the freshly spawned binary takes over. nil uses
	// os.Exit. It is never invoked on unix, where the exec replaces the process
	// image and does not return. Tests inject it to observe the handoff without
	// actually exiting.
	Exit func(code int)
}

func (r *Restarter) exec() ExecFunc {
	if r.Exec != nil {
		return r.Exec
	}
	return defaultExec
}

func (r *Restarter) exit() func(code int) {
	if r.Exit != nil {
		return r.Exit
	}
	return os.Exit
}

// Restart hands control to executablePath (the just-installed binary) with the
// original arguments and environment.
//
// On unix this re-execs in place (syscall.Exec), so a successful call does not
// return. On Windows it spawns executablePath as a new process and then exits
// the current one via the Exit seam, so a successful call likewise does not
// return in production (the injected Exit seam lets tests observe it instead).
// Any returned error is one that prevented the handoff.
func (r *Restarter) Restart(executablePath string) error {
	argv := r.Args
	if argv == nil {
		argv = os.Args
	}
	// argv[0] becomes the installed binary path; the remaining args are carried
	// over so the new process sees the same command line.
	newArgv := make([]string, 0, len(argv))
	if len(argv) == 0 {
		newArgv = append(newArgv, executablePath)
	} else {
		newArgv = append(newArgv, executablePath)
		newArgv = append(newArgv, argv[1:]...)
	}

	env := r.Env
	if env == nil {
		env = os.Environ()
	}

	err := r.exec()(executablePath, newArgv, env)
	if errors.Is(err, errHandoffExit) {
		// The exec spawned a detached replacement process rather than replacing
		// this process image (the Windows path). Complete the handoff by exiting
		// so the new process takes over. In production this does not return; with
		// an injected Exit seam it does, and we report success.
		r.exit()(0)
		return nil
	}
	return err
}
