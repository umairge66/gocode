// Package procio implements the gocode's foundational process-I/O and
// exit-signalling primitives.
//
// It is NOT a logger or an event bus; it is the CLI's standard-stream plumbing
// layer that essentially every other package needs in order to emit output and
// terminate cleanly. It covers four closely related concerns:
//
//  1. Exit-cause IPC. A pair of single-shot files ("exit-cause", "exit-detail")
//     written into a job directory (env GOCODE_JOB_DIR) so a supervising
//     process can learn WHY this process exited. The reader consumes (deletes)
//     the file on read, and refuses/cleans files larger than 64 KiB.
//  2. Stream-error classification. Two sets of errno codes distinguishing
//     broken-pipe / destroy-worthy errors (EPIPE, EIO, ENXIO, EBADF) from other
//     ignorable stream errors (EISDIR, ENOTCONN, ECONNRESET).
//  3. A managed stdout writer that tracks queued-vs-flushed bytes, supports a
//     bounded "drain on exit" with a backlog-scaled time budget, and an
//     "externally clocked" mode (output paced by a downstream consumer).
//  4. A managed stderr writer that DROPS low-priority messages under sustained
//     backpressure (>=1 MiB unflushed for >=10 s) and later emits a synthetic
//     "N messages dropped" notice once the reader catches up.
//
// Plus a fatal-exit helper and two stream-reading helpers.
//
// SCOPE. The pure/self-contained pieces — exit-cause IPC, errno classification,
// the drain-budget math, and the stderr drop state machine — are implemented
// fully and tested. Stream backpressure and flush tracking are modelled
// against a small Writable abstraction, with the real os.Stdout/os.Stderr
// wiring reduced to Go's blocking writes (see stdout.go). Whether the process
// is diskless is injected through the isDisklessProcess hook.
package procio
