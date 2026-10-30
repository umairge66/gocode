package bgtask

import (
	"io"
	"sync"
)

// DefaultTailCap is the maximum number of trailing output bytes retained for a
// task, i.e. the size of the get_task_output tail.
const DefaultTailCap = 8192

// ring is a bounded, concurrency-safe byte buffer that retains only the last
// cap bytes written while tracking the total number of bytes ever written. It
// is the output tail of a background task and implements io.Writer so it can be
// wired directly to a process's stdout/stderr pipes.
type ring struct {
	mu    sync.Mutex
	cap   int
	data  []byte // the retained tail, len(data) <= cap
	total int64  // total bytes ever written
}

func newRing(cap int) *ring {
	if cap <= 0 {
		cap = DefaultTailCap
	}
	return &ring{cap: cap, data: make([]byte, 0, cap)}
}

// Write appends p to the tail, dropping the oldest bytes so that at most cap
// bytes are retained. It never returns an error and always reports len(p)
// consumed, satisfying io.Writer.
func (r *ring) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.total += int64(n)

	// A single write at least as large as cap: keep only its trailing cap bytes.
	if n >= r.cap {
		r.data = append(r.data[:0], p[n-r.cap:]...)
		return n, nil
	}

	r.data = append(r.data, p...)
	if len(r.data) > r.cap {
		// Trim the oldest bytes. Copy into a fresh backing array so the
		// discarded prefix cannot be retained indefinitely.
		drop := len(r.data) - r.cap
		trimmed := make([]byte, r.cap)
		copy(trimmed, r.data[drop:])
		r.data = trimmed
	}
	return n, nil
}

var _ io.Writer = (*ring)(nil)

// since returns the output written after byte offset cursor, the new total
// (to be used as the next cursor), and whether any output older than the
// returned slice has already scrolled out of the retained tail.
//
// Read semantics: a cursor of 0 yields the full retained tail (the
// get_task_output "last 8 KiB" read); a cursor from a previous read yields only
// the output produced since then. Truncated reports bytesRead < bytesTotal:
// bytes between the cursor and the start of the retained window were discarded.
func (r *ring) since(cursor int64) (out []byte, newCursor int64, truncated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := r.total
	windowStart := total - int64(len(r.data)) // offset of the oldest retained byte

	if cursor < 0 {
		cursor = 0
	}
	from := cursor
	if from < windowStart {
		// The reader is behind the retained window: some output was dropped.
		from = windowStart
		truncated = true
	}
	if from > total {
		from = total
	}

	offset := from - windowStart
	out = append([]byte(nil), r.data[offset:]...)
	return out, total, truncated
}
