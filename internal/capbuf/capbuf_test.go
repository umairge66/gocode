package capbuf

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestSmallOutputUnchanged(t *testing.T) {
	b := New(10, 5)
	b.Write([]byte("hello"))
	b.Write([]byte(" world"))
	if got := b.String(); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if b.Truncated() || b.Total() != 11 {
		t.Fatalf("truncated %v total %d", b.Truncated(), b.Total())
	}
}

func TestKeepsHeadAndTail(t *testing.T) {
	b := New(4, 3)
	for _, s := range []string{"ab", "cdef", "ghij", "kl"} {
		b.Write([]byte(s))
	}
	got := b.String()
	if !strings.HasPrefix(got, "abcd\n\n[... 5 bytes of output omitted") || !strings.HasSuffix(got, "...]\n\njkl") {
		t.Fatalf("got %q", got)
	}
	if !b.Truncated() || b.Total() != 12 {
		t.Fatalf("truncated %v total %d", b.Truncated(), b.Total())
	}
}

func TestLargeSingleWriteKeepsLastTail(t *testing.T) {
	b := New(2, 3)
	b.Write([]byte("0123456789"))
	if got := b.String(); !strings.HasPrefix(got, "01\n\n") || !strings.HasSuffix(got, "789") {
		t.Fatalf("got %q", got)
	}
}

func TestMemoryStaysBounded(t *testing.T) {
	b := New(1<<10, 1<<10)
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for range 1000 { // 62.5 MiB written
		b.Write(chunk)
	}
	if len(b.head) != 1<<10 || len(b.tail) != 1<<10 {
		t.Fatalf("head %d tail %d", len(b.head), len(b.tail))
	}
	if !strings.Contains(b.String(), "62.5 MB in total") {
		t.Fatalf("missing total: %q", b.String()[1<<10:1<<10+160])
	}
}

func TestZeroValueUsesDefaults(t *testing.T) {
	var b Buffer
	b.Write(bytes.Repeat([]byte("y"), DefaultHeadCap+10))
	if b.Truncated() || len(b.String()) != DefaultHeadCap+10 {
		t.Fatalf("truncated %v len %d", b.Truncated(), len(b.String()))
	}
}

func TestConcurrentWrites(t *testing.T) {
	b := New(100, 100)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				b.Write([]byte("abc"))
			}
		}()
	}
	wg.Wait()
	if b.Total() != 24000 {
		t.Fatalf("total %d", b.Total())
	}
}
