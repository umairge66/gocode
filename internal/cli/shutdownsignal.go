package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Process shutdown signals for the interactive session and -p.
//
// One set of handlers is installed per process: SIGINT shuts down
// with exit code 0, SIGTERM with 143, SIGHUP with 129. A background worker
// (GOCODE_BG_BACKEND=daemon) ignores SIGHUP unless it owns its controlling
// terminal (the pty its host gave it), so a hang-up of an unrelated terminal
// cannot end a detached session. A shutdown stops the current turn, runs the
// SessionEnd hooks, flushes the transcript and exits with the code; a
// failsafe timer (max(5 s, SessionEnd budget + 5 s)) forces the exit if the
// cleanup hangs.

// Shutdown exit codes.
const (
	exitCodeSIGINT  = 0
	exitCodeSIGTERM = 143
	exitCodeSIGHUP  = 129
)

// shutdownFailsafe bounds the graceful shutdown: the SessionEnd budget (the
// session close's 30 s) plus 5 s.
var shutdownFailsafe = 35 * time.Second

// turnDrainTimeout is how long a shutdown waits for the interrupted turn to
// wind down before closing the session.
const turnDrainTimeout = 2 * time.Second

// shutdownSignals delivers the exit code of the first shutdown signal.
type shutdownSignals struct {
	codes chan int
	stop  func()
	once  sync.Once
}

// watchShutdownSignals starts listening. getenv decides the SIGHUP rule.
func watchShutdownSignals(getenv func(string) string) *shutdownSignals {
	bgWorker := getenv != nil && getenv("GOCODE_BG_BACKEND") == "daemon"
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, shutdownSignalSet()...)
	ctx, cancel := context.WithCancel(context.Background())
	s := &shutdownSignals{codes: make(chan int, 1)}
	s.stop = func() {
		s.once.Do(func() {
			signal.Stop(ch)
			cancel()
		})
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-ch:
				code, ok := shutdownCode(sig, bgWorker, ownsControllingTerminal)
				if !ok {
					continue
				}
				select {
				case s.codes <- code:
				default:
				}
				return
			}
		}
	}()
	return s
}

// Codes yields the exit code of the shutdown signal (at most once).
func (s *shutdownSignals) Codes() <-chan int { return s.codes }

// Stop removes the handlers.
func (s *shutdownSignals) Stop() { s.stop() }

// armFailsafe forces the exit with code if the graceful shutdown does not
// finish in time. The returned func disarms it.
func armShutdownFailsafe(code int) func() {
	t := time.AfterFunc(shutdownFailsafe, func() { os.Exit(code) })
	return func() { t.Stop() }
}
