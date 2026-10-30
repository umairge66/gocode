// Package capbuf provides an io.Writer that keeps a bounded amount of a
// command's output in memory: the first HeadCap bytes and the last TailCap
// bytes. A command that prints without end (a runaway loop, a binary dump)
// therefore cannot grow the process's memory past HeadCap+TailCap per stream.
package capbuf

import (
	"fmt"
	"sync"
)

// Default sizes for tool commands. The model sees at most a few tens of
// kilobytes of a result; the head is large enough for the full-output file
// that oversized results are saved to.
const (
	DefaultHeadCap = 8 << 20
	DefaultTailCap = 1 << 20
)

// Buffer keeps the first HeadCap and the last TailCap bytes written to it and
// counts the rest. The zero value uses DefaultHeadCap and DefaultTailCap. It
// is safe for concurrent writes.
type Buffer struct {
	HeadCap int
	TailCap int

	mu    sync.Mutex
	head  []byte
	tail  []byte // ring of TailCap bytes once full
	start int    // index of the oldest tail byte when the ring is full
	total int64
}

// New returns a Buffer with the given caps (0 means the default).
func New(headCap, tailCap int) *Buffer {
	return &Buffer{HeadCap: headCap, TailCap: tailCap}
}

func (b *Buffer) caps() (int, int) {
	h, t := b.HeadCap, b.TailCap
	if h <= 0 {
		h = DefaultHeadCap
	}
	if t < 0 {
		t = 0
	} else if t == 0 {
		t = DefaultTailCap
	}
	return h, t
}

// Write never fails; bytes beyond the head go to the tail ring.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	headCap, tailCap := b.caps()
	if room := headCap - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 || tailCap == 0 {
		return n, nil
	}
	if len(p) >= tailCap {
		b.tail = append(b.tail[:0], p[len(p)-tailCap:]...)
		b.start = 0
		return n, nil
	}
	for len(p) > 0 {
		if len(b.tail) < tailCap {
			k := min(tailCap-len(b.tail), len(p))
			b.tail = append(b.tail, p[:k]...)
			p = p[k:]
			continue
		}
		k := copy(b.tail[b.start:], p)
		p = p[k:]
		b.start = (b.start + k) % tailCap
	}
	return n, nil
}

// Total is the number of bytes written, including the dropped ones.
func (b *Buffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Truncated reports whether any output was dropped.
func (b *Buffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(len(b.head)+len(b.tail))
}

// String returns the kept output. When bytes were dropped, a line between the
// head and the tail says how many.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := append(append([]byte(nil), b.tail[b.start:]...), b.tail[:b.start]...)
	kept := int64(len(b.head) + len(tail))
	if b.total <= kept {
		return string(b.head) + string(tail)
	}
	return fmt.Sprintf("%s\n\n[... %s of output omitted: the command printed %s in total; the first %s and the last %s are kept ...]\n\n%s",
		b.head, size(b.total-kept), size(b.total), size(int64(len(b.head))), size(int64(len(tail))), tail)
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
