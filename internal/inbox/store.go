package inbox

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// SendReason is a send-failure cause. Most of these are produced by
// the cross-process transport that actually delivers to a recipient (the
// SendMessage dispatch, a seam here); the local in-process Store surfaces only
// the ones it can decide (self-target). The full taxonomy is kept so the
// result mapping stays complete.
type SendReason string

const (
	ReasonUnreachableNamespace SendReason = "unreachable-namespace" // -> not_reachable
	ReasonSelfTarget           SendReason = "self-target"           // -> invalid_target
	ReasonPeerGone             SendReason = "peer-gone"             // -> stale_socket
	ReasonSendFailed           SendReason = "send-failed"           // -> other
)

// SendResult is the outcome of a Send. Ok delivers (and carries the stored
// message ID); otherwise Reason explains why (the {ok,reason,...} union the
// SendMessage dispatch returns).
type SendResult struct {
	Ok     bool
	ID     string
	Reason SendReason
	// Detail is the tool-facing failure text for a cross-session
	// send (FailureMessage / the address-validation messages); "" otherwise.
	Detail string
}

// entry is a queued message plus its store-side bookkeeping.
type entry struct {
	msg Message
}

// Store holds the per-agent in-process inbox queues. Each recipient address has
// its own FIFO queue; a message is read (Fetch/Peek) non-destructively and
// removed only by Ack — the read-then-ack lifecycle the pointer tracker
// implements (a pointer is tracked until noteFetchAttempt removes it).
//
// All methods are safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	queues map[string][]*entry
	seq    uint64
	peer   *PeerSender
}

// SetPeerSender attaches (or, with nil, detaches) the cross-session transport
// Send uses for uds: addresses (other local gocode processes).
func (s *Store) SetPeerSender(p *PeerSender) {
	s.mu.Lock()
	s.peer = p
	s.mu.Unlock()
}

// NewStore returns an empty inbox store.
func NewStore() *Store {
	return &Store{queues: make(map[string][]*entry)}
}

// nextID mints a monotonic message id. Must be called with mu held.
func (s *Store) nextID() string {
	s.seq++
	return "msg_" + strconv.FormatUint(s.seq, 10)
}

// Send delivers m. A uds: address (or bare *.sock / \\.\pipe\ path) names
// another local gocode session: the message is handed to the attached
// PeerSender (see SetPeerSender) and written to that process's inbox socket;
// without one the result is ReasonUnreachableNamespace with the
// "not available" text. Any other recipient is queued in-process: an empty
// m.ID is assigned a fresh monotonic id, an empty m.CreatedAt is stamped now,
// and the stored id is returned in the result.
//
// Bridge (bridge:/did:), Remote Control and cloud-session delivery go through
// hosted services and are not supported; such addresses fail as unreachable.
func (s *Store) Send(m Message) SendResult {
	if !ValidRecipient(m.To) {
		return SendResult{Reason: ReasonSendFailed}
	}
	switch ParseAddress(m.To).Scheme {
	case SchemeUDS:
		s.mu.Lock()
		p := s.peer
		s.mu.Unlock()
		if p == nil {
			return SendResult{Reason: ReasonUnreachableNamespace, Detail: "Cross-session messaging is not available in this session."}
		}
		id, err := p.Send(context.Background(), m.To, m.Body, PeerSendOptions{FromName: m.From})
		if err != nil {
			return SendResult{Reason: sendReasonFor(err), Detail: FailureMessage(m.To, err, p.ListTool)}
		}
		return SendResult{Ok: true, ID: id}
	case SchemeBridge, SchemeDID:
		return SendResult{Reason: ReasonUnreachableNamespace, Detail: "Cross-session messaging is not available in this session."}
	}
	return s.enqueueLocal(m)
}

// enqueueLocal queues m in its recipient's in-process queue.
func (s *Store) enqueueLocal(m Message) SendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = s.nextID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if len(m.Summary) > SummaryMaxChars {
		m.Summary = m.Summary[:SummaryMaxChars] // truncated, not rejected
	}
	m.Fetched = false
	s.queues[m.To] = append(s.queues[m.To], &entry{msg: m})
	return SendResult{Ok: true, ID: m.ID}
}

// Peek returns the oldest pending message for agent without mutating anything
// (it does not mark the message fetched). The pointer map is read without
// being consumed; removal is a separate ack.
func (s *Store) Peek(agent string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[agent]
	if len(q) == 0 {
		return Message{}, false
	}
	return cloneMessage(q[0].msg), true
}

// Fetch returns the oldest pending message for agent and marks it fetched (the
// tracker's `delivered` bit). It does NOT remove the message; the recipient
// acknowledges it with Ack once handled. Repeated Fetch without an intervening
// Ack returns the same front message, now flagged Fetched.
func (s *Store) Fetch(agent string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[agent]
	if len(q) == 0 {
		return Message{}, false
	}
	q[0].msg.Fetched = true
	return cloneMessage(q[0].msg), true
}

// Ack removes the message with the given id from agent's queue (the pointer's
// noteFetchAttempt removal). It reports whether a message was removed; an id
// not present (already acked, or never queued) reports false.
func (s *Store) Ack(agent, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[agent]
	for i, e := range q {
		if e.msg.ID == id {
			s.queues[agent] = append(q[:i:i], q[i+1:]...)
			if len(s.queues[agent]) == 0 {
				delete(s.queues, agent)
			}
			return true
		}
	}
	return false
}

// List returns a snapshot of every pending message for agent, oldest first.
func (s *Store) List(agent string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[agent]
	if len(q) == 0 {
		return nil
	}
	out := make([]Message, len(q))
	for i, e := range q {
		out[i] = cloneMessage(e.msg)
	}
	return out
}

// Len returns the number of pending messages for agent.
func (s *Store) Len(agent string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queues[agent])
}

// cloneMessage returns a copy whose Attachments slice is independent, so a
// caller cannot mutate stored state through the returned value.
func cloneMessage(m Message) Message {
	if m.Attachments != nil {
		a := make([]string, len(m.Attachments))
		copy(a, m.Attachments)
		m.Attachments = a
	}
	return m
}
