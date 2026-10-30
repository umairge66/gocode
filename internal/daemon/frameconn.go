package daemon

import (
	"context"
	"net"
	"sync"
)

// frameConn is a net.Conn with an asynchronous, bounded write queue. A peer
// that stops reading is destroyed instead of stalling the producer.
type frameConn struct {
	conn net.Conn

	mu      sync.Mutex
	queue   [][]byte
	queued  int
	maxB    int
	closing bool // End called: flush then close
	dead    bool
	wake    chan struct{}
	done    chan struct{}
}

// maxQueuedBytes is the backpressure bound.
const maxQueuedBytes = 1 << 20

func newFrameConn(c net.Conn) *frameConn {
	fc := &frameConn{conn: c, maxB: maxQueuedBytes, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go fc.writer()
	return fc
}

// Send queues b; it reports false when the connection is gone or was destroyed
// for exceeding the backpressure bound.
func (fc *frameConn) Send(b []byte) bool {
	fc.mu.Lock()
	if fc.dead || fc.closing {
		fc.mu.Unlock()
		return false
	}
	if fc.queued+len(b) > fc.maxB {
		fc.dead = true
		fc.mu.Unlock()
		_ = fc.conn.Close()
		return false
	}
	fc.queue = append(fc.queue, b)
	fc.queued += len(b)
	fc.mu.Unlock()
	select {
	case fc.wake <- struct{}{}:
	default:
	}
	return true
}

// End flushes the queue then closes the connection.
func (fc *frameConn) End() {
	fc.mu.Lock()
	fc.closing = true
	fc.mu.Unlock()
	select {
	case fc.wake <- struct{}{}:
	default:
	}
}

// Destroy closes immediately, dropping anything queued.
func (fc *frameConn) Destroy() {
	fc.mu.Lock()
	fc.dead = true
	fc.queue = nil
	fc.mu.Unlock()
	_ = fc.conn.Close()
	select {
	case fc.wake <- struct{}{}:
	default:
	}
}

// Done is closed once the writer has exited (the connection is closed).
func (fc *frameConn) Done() <-chan struct{} { return fc.done }

func (fc *frameConn) writer() {
	defer close(fc.done)
	for {
		fc.mu.Lock()
		if fc.dead {
			fc.mu.Unlock()
			_ = fc.conn.Close()
			return
		}
		if len(fc.queue) == 0 {
			if fc.closing {
				fc.mu.Unlock()
				_ = fc.conn.Close()
				return
			}
			fc.mu.Unlock()
			<-fc.wake
			continue
		}
		b := fc.queue[0]
		fc.queue = fc.queue[1:]
		fc.queued -= len(b)
		fc.mu.Unlock()
		if _, err := fc.conn.Write(b); err != nil {
			fc.mu.Lock()
			fc.dead = true
			fc.queue = nil
			fc.mu.Unlock()
			_ = fc.conn.Close()
			return
		}
	}
}

// ListenEndpoint binds a local endpoint (unix socket or Windows named pipe) for
// the background-session sockets, with the same ownership guard and same-user
// access control as the control socket.
func ListenEndpoint(ep ControlEndpoint) (net.Listener, error) { return listenEndpoint(ep) }

// DialEndpoint connects to a local endpoint (unix socket or Windows named
// pipe; the pipe's server user is verified).
func DialEndpoint(ctx context.Context, ep ControlEndpoint) (net.Conn, error) {
	return dialEndpoint(ctx, ep)
}
