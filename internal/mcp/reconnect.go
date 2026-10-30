package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// ReconnectPolicy controls how a remote transport re-opens a dropped
// event-stream. Delays grow geometrically from InitialDelay by GrowFactor and
// are capped at MaxDelay; a server-sent SSE "retry:" field overrides the
// computed delay. After MaxRetries failed attempts the transport gives up.
type ReconnectPolicy struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	GrowFactor   float64
	MaxRetries   int
}

// DefaultReconnectPolicy is the default reconnection policy.
var DefaultReconnectPolicy = ReconnectPolicy{
	InitialDelay: time.Second,
	MaxDelay:     30 * time.Second,
	GrowFactor:   1.5,
	MaxRetries:   2,
}

// delay returns the wait before reconnection attempt n (0-based). serverRetry,
// when positive, is the server's "retry:" hint and wins.
func (p ReconnectPolicy) delay(n int, serverRetry time.Duration) time.Duration {
	if serverRetry > 0 {
		return serverRetry
	}
	d := float64(p.InitialDelay) * math.Pow(p.GrowFactor, float64(n))
	if p.MaxDelay > 0 && d > float64(p.MaxDelay) {
		return p.MaxDelay
	}
	return time.Duration(d)
}

// maxAttemptsError is the give-up message.
func maxAttemptsError(n int) error {
	return fmt.Errorf("Maximum reconnection attempts (%d) exceeded.", n)
}

// ConnState is a remote transport's connection state as reported to the host.
type ConnState string

const (
	// ConnReconnecting: the stream dropped and a reconnection attempt is
	// scheduled or running (Attempt/MaxAttempts say which).
	ConnReconnecting ConnState = "reconnecting"
	// ConnConnected: a reconnection succeeded (the stream is live again).
	ConnConnected ConnState = "connected"
	// ConnDisconnected: reconnection was given up; the transport is closing.
	ConnDisconnected ConnState = "disconnected"
)

// ConnEvent is one connection-state change.
type ConnEvent struct {
	State       ConnState
	Attempt     int
	MaxAttempts int
	Err         error
}

// StateListener receives connection-state changes. It must not block.
type StateListener func(ConnEvent)

// stateNotifier is embedded by the remote transports. The host installs its
// listener through SetStateListener after the transport is built, so it works
// with injected transport factories too.
type stateNotifier struct {
	mu sync.Mutex
	fn StateListener
}

// SetStateListener installs (or, with nil, removes) the state listener.
func (s *stateNotifier) SetStateListener(fn StateListener) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *stateNotifier) emit(ev ConnEvent) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// StateReporter is implemented by transports that report reconnection
// progress (SSE, Streamable HTTP, WebSocket).
type StateReporter interface {
	SetStateListener(StateListener)
}

// sleepCtx waits d or until ctx is done (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isDialError reports whether err happened before any request byte reached
// the server (connection refused, DNS failure, ...), which makes a retry safe
// even for a non-idempotent JSON-RPC request.
func isDialError(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "dial"
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// connectionLostResponse builds the synthetic JSON-RPC error a transport
// delivers for an in-flight request whose reply can no longer arrive, so the
// caller fails cleanly instead of waiting forever.
func connectionLostResponse(id json.RawMessage, reason string) Message {
	return Message{Response: &Response{
		JSONRPC: JSONRPCVersion,
		ID:      append(json.RawMessage(nil), id...),
		Error:   &RPCError{Code: ErrConnectionClosed, Message: reason},
	}}
}

// inflightSet tracks request ids sent but not yet answered, so a transport can
// fail them when the session they were sent on is gone.
type inflightSet struct {
	mu  sync.Mutex
	ids map[string]json.RawMessage
}

func (s *inflightSet) add(id json.RawMessage) {
	s.mu.Lock()
	if s.ids == nil {
		s.ids = map[string]json.RawMessage{}
	}
	s.ids[string(id)] = append(json.RawMessage(nil), id...)
	s.mu.Unlock()
}

func (s *inflightSet) done(id json.RawMessage) {
	s.mu.Lock()
	delete(s.ids, string(id))
	s.mu.Unlock()
}

// drainAll removes and returns every outstanding id.
func (s *inflightSet) drainAll() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, 0, len(s.ids))
	for _, id := range s.ids {
		out = append(out, id)
	}
	s.ids = nil
	return out
}
