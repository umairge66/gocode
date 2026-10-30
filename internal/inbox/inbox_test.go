package inbox

import (
	"sync"
	"testing"
)

// Send then Fetch returns the message; multiple sends come back in FIFO order
// as each front is acked.
func TestSendFetchFIFO(t *testing.T) {
	s := NewStore()
	s.Send(Message{To: "alice", Body: "first", FromKind: SenderPeer})
	s.Send(Message{To: "alice", Body: "second", FromKind: SenderPeer})

	m, ok := s.Fetch("alice")
	if !ok || m.Body != "first" {
		t.Fatalf("fetch1 = %q, %v; want first", m.Body, ok)
	}
	if !s.Ack("alice", m.ID) {
		t.Fatal("ack of first returned false")
	}
	m, ok = s.Fetch("alice")
	if !ok || m.Body != "second" {
		t.Fatalf("fetch2 = %q, %v; want second", m.Body, ok)
	}
	if !s.Ack("alice", m.ID) {
		t.Fatal("ack of second returned false")
	}
	if _, ok := s.Fetch("alice"); ok {
		t.Fatal("fetch on drained inbox should report empty")
	}
}

// Fetch marks the message delivered; Ack removes it.
func TestFetchMarksAckRemoves(t *testing.T) {
	s := NewStore()
	res := s.Send(Message{To: "bob", Body: "hi", FromKind: SenderPeer})
	if !res.Ok || res.ID == "" {
		t.Fatalf("send result = %+v", res)
	}
	m, _ := s.Fetch("bob")
	if !m.Fetched {
		t.Fatal("Fetch should mark the message Fetched")
	}
	if s.Len("bob") != 1 {
		t.Fatal("Fetch must not remove the message")
	}
	if !s.Ack("bob", res.ID) {
		t.Fatal("Ack should remove the fetched message")
	}
	if s.Len("bob") != 0 {
		t.Fatal("message still present after Ack")
	}
	if s.Ack("bob", res.ID) {
		t.Fatal("second Ack of same id should report false")
	}
}

// Peek returns the message without removing it or marking it fetched.
func TestPeekNonDestructive(t *testing.T) {
	s := NewStore()
	s.Send(Message{To: "carol", Body: "ping", FromKind: SenderPeer})

	m, ok := s.Peek("carol")
	if !ok || m.Body != "ping" {
		t.Fatalf("peek = %q, %v", m.Body, ok)
	}
	if m.Fetched {
		t.Fatal("Peek must not mark the message Fetched")
	}
	if s.Len("carol") != 1 {
		t.Fatal("Peek must not remove the message")
	}
	// A second Peek still sees it, still unfetched.
	if m2, _ := s.Peek("carol"); m2.Fetched {
		t.Fatal("Peek should remain non-destructive")
	}
}

// Each recipient has an isolated queue.
func TestRecipientsIsolated(t *testing.T) {
	s := NewStore()
	s.Send(Message{To: "x", Body: "for-x", FromKind: SenderPeer})
	s.Send(Message{To: "y", Body: "for-y", FromKind: SenderPeer})
	s.Send(Message{To: "y", Body: "for-y-2", FromKind: SenderPeer})

	if s.Len("x") != 1 || s.Len("y") != 2 {
		t.Fatalf("lens x=%d y=%d; want 1,2", s.Len("x"), s.Len("y"))
	}
	mx, _ := s.Peek("x")
	if mx.Body != "for-x" {
		t.Fatalf("x got %q", mx.Body)
	}
	// Acking x leaves y untouched.
	s.Ack("x", mx.ID)
	if s.Len("x") != 0 || s.Len("y") != 2 {
		t.Fatalf("after ack x: x=%d y=%d", s.Len("x"), s.Len("y"))
	}
	if _, ok := s.Fetch("absent"); ok {
		t.Fatal("unknown recipient should report empty")
	}
}

// List returns a snapshot oldest-first and does not alias store state.
func TestListSnapshot(t *testing.T) {
	s := NewStore()
	s.Send(Message{To: "z", Body: "a", Attachments: []string{"@f"}, FromKind: SenderPeer})
	s.Send(Message{To: "z", Body: "b", FromKind: SenderPeer})

	got := s.List("z")
	if len(got) != 2 || got[0].Body != "a" || got[1].Body != "b" {
		t.Fatalf("list = %+v", got)
	}
	// Mutating the returned copy must not affect the store.
	got[0].Attachments[0] = "@mutated"
	again := s.List("z")
	if again[0].Attachments[0] != "@f" {
		t.Fatal("List returned an aliased Attachments slice")
	}
}

// ValidRecipient rejects multi-line and empty addresses.
func TestSendRejectsInvalidRecipient(t *testing.T) {
	s := NewStore()
	if r := s.Send(Message{To: "a\nb", Body: "x"}); r.Ok {
		t.Fatal("multi-line recipient should be rejected")
	}
	if r := s.Send(Message{To: "", Body: "x"}); r.Ok {
		t.Fatal("empty recipient should be rejected")
	}
}

// Summary longer than the cap is truncated rather than rejected (200 chars).
func TestSummaryTruncated(t *testing.T) {
	s := NewStore()
	long := make([]byte, SummaryMaxChars+50)
	for i := range long {
		long[i] = 'x'
	}
	res := s.Send(Message{To: "a", Body: "b", Summary: string(long)})
	if !res.Ok {
		t.Fatal("send with long summary should succeed")
	}
	m, _ := s.Peek("a")
	if len(m.Summary) != SummaryMaxChars {
		t.Fatalf("summary len = %d; want %d", len(m.Summary), SummaryMaxChars)
	}
}

// Concurrent Send/Fetch/Ack across recipients must be race-free and lose no
// messages.
func TestConcurrentSendFetchAck(t *testing.T) {
	s := NewStore()
	const writers, perWriter = 8, 200

	var wg sync.WaitGroup
	// Writers each target their own recipient.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			to := recipientName(w)
			for i := 0; i < perWriter; i++ {
				s.Send(Message{To: to, Body: "m", FromKind: SenderPeer})
			}
		}(w)
	}
	// Concurrent readers drain whichever recipients they can.
	var drained [writers]int
	var dmu sync.Mutex
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for w := 0; w < writers; w++ {
					to := recipientName(w)
					if m, ok := s.Fetch(to); ok {
						if s.Ack(to, m.ID) {
							dmu.Lock()
							drained[w]++
							dmu.Unlock()
						}
					}
				}
			}
		}()
	}

	wg.Wait()
	// Drain the remainder deterministically after all sends complete.
	for {
		total := 0
		for w := 0; w < writers; w++ {
			to := recipientName(w)
			for {
				m, ok := s.Fetch(to)
				if !ok {
					break
				}
				if s.Ack(to, m.ID) {
					dmu.Lock()
					drained[w]++
					dmu.Unlock()
				}
			}
			total += s.Len(to)
		}
		if total == 0 {
			break
		}
	}
	close(stop)
	readers.Wait()

	for w := 0; w < writers; w++ {
		if drained[w] != perWriter {
			t.Errorf("recipient %d drained %d; want %d", w, drained[w], perWriter)
		}
	}
}

func recipientName(w int) string { return "r" + string(rune('0'+w)) }
