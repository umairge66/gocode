package inbox

import (
	"sync"
	"time"
)

// GuardLimits are the peer-messaging limits (including
// maxQueuedPeerMessages; there is no remote override).
type GuardLimits struct {
	BucketCapacity        float64
	RefillPerSecond       float64
	DedupWindow           time.Duration
	MaxSelfHops           int
	MaxChainLength        int
	MaxTrackedSenders     int
	MaxQueuedPeerMessages int
}

// DefaultGuardLimits returns the default limits.
func DefaultGuardLimits() GuardLimits {
	return GuardLimits{
		BucketCapacity:        30,
		RefillPerSecond:       0.5,
		DedupWindow:           30 * time.Second,
		MaxSelfHops:           10,
		MaxChainLength:        28,
		MaxTrackedSenders:     256,
		MaxQueuedPeerMessages: 50,
	}
}

// DropReason is why the ingress guard refused an inbound peer message.
type DropReason string

const (
	DropRateLimited DropReason = "rate-limited"
	DropDuplicate   DropReason = "duplicate"
	DropHopLoop     DropReason = "hop-loop"
	DropHopRunaway  DropReason = "hop-runaway"
	DropQueueFull   DropReason = "queue-full"
)

// Describe returns the human text for a drop reason.
func (r DropReason) Describe() string {
	switch r {
	case DropRateLimited:
		return "sender exceeded the peer message rate limit"
	case DropDuplicate:
		return "identical to the previous message from this sender"
	case DropHopLoop:
		return "message has already passed through this session (a peer messaging loop)"
	case DropHopRunaway:
		return "peer relay chain is too long (runaway forwarding)"
	case DropQueueFull:
		return "this session has too many undelivered peer messages queued"
	}
	return string(r)
}

// refill adds tokens refilled linearly since last, capped at capacity.
func refill(tokens float64, last, now time.Time, capacity, perSec float64) float64 {
	el := now.Sub(last).Seconds()
	if el < 0 {
		el = 0
	}
	t := tokens + el*perSec
	if t > capacity {
		t = capacity
	}
	return t
}

// lru is an insertion-ordered map (touch moves to the end).
type lru[V any] struct {
	keys []string
	m    map[string]V
}

func (l *lru[V]) get(k string) (V, bool) {
	v, ok := l.m[k]
	if ok {
		l.touch(k)
	}
	return v, ok
}

func (l *lru[V]) touch(k string) {
	for i, x := range l.keys {
		if x == k {
			l.keys = append(l.keys[:i], l.keys[i+1:]...)
			break
		}
	}
	l.keys = append(l.keys, k)
}

// put inserts k, first evicting (preferring entries evictable reports true,
// else the oldest) until there is room under max.
func (l *lru[V]) put(k string, v V, max int, evictable func(V) bool) {
	if l.m == nil {
		l.m = make(map[string]V)
	}
	if max < 1 {
		max = 1
	}
	for len(l.m) >= max && len(l.keys) > 0 {
		victim := -1
		if evictable != nil {
			for i, x := range l.keys {
				if evictable(l.m[x]) {
					victim = i
					break
				}
			}
		}
		if victim < 0 {
			victim = 0
		}
		delete(l.m, l.keys[victim])
		l.keys = append(l.keys[:victim], l.keys[victim+1:]...)
	}
	l.m[k] = v
	l.keys = append(l.keys, k)
}

func (l *lru[V]) size() int { return len(l.m) }

// IngressGuard admits inbound peer messages: hop-chain loop/runaway checks,
// per-sender duplicate suppression and a per-sender token bucket.
type IngressGuard struct {
	mu      sync.Mutex
	limits  GuardLimits
	now     func() time.Time
	senders lru[*senderState]
}

type senderState struct {
	tokens     float64
	lastRefill time.Time
	lastBody   *string
	lastBodyAt time.Time
}

// NewIngressGuard builds a guard; a nil now uses time.Now.
func NewIngressGuard(l GuardLimits, now func() time.Time) *IngressGuard {
	if now == nil {
		now = time.Now
	}
	return &IngressGuard{limits: l, now: now}
}

// CheckHopChain: a chain longer than MaxChainLength is a
// runaway; MaxSelfHops or more of our own tokens is a loop.
func (g *IngressGuard) CheckHopChain(chain []string, own map[string]bool) DropReason {
	if chain != nil && len(chain) > g.limits.MaxChainLength {
		return DropHopRunaway
	}
	if len(own) > 0 {
		n := 0
		for _, t := range chain {
			if own[t] {
				n++
			}
		}
		if n >= g.limits.MaxSelfHops {
			return DropHopLoop
		}
	}
	return ""
}

// Admit returns "" to admit the message (consuming one token).
func (g *IngressGuard) Admit(senderKey, body string, chain []string, own map[string]bool) DropReason {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r := g.CheckHopChain(chain, own); r != "" {
		return r
	}
	now := g.now()
	st, ok := g.senders.get(senderKey)
	if !ok {
		st = &senderState{tokens: g.limits.BucketCapacity, lastRefill: now}
		g.senders.put(senderKey, st, g.limits.MaxTrackedSenders, nil)
	}
	if st.lastBody != nil && *st.lastBody == body && now.Sub(st.lastBodyAt) < g.limits.DedupWindow {
		return DropDuplicate
	}
	st.tokens = refill(st.tokens, st.lastRefill, now, g.limits.BucketCapacity, g.limits.RefillPerSecond)
	st.lastRefill = now
	if st.tokens < 1 {
		return DropRateLimited
	}
	st.tokens--
	b := body
	st.lastBody = &b
	st.lastBodyAt = now
	return ""
}

// TrackedSenders reports how many senders the guard remembers.
func (g *IngressGuard) TrackedSenders() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.senders.size()
}

// Pacer is the sender-side mirror of the recipient's bucket: it refuses to
// send a message the target's inbox would drop as rate-limited.
type Pacer struct {
	mu      sync.Mutex
	limits  GuardLimits
	now     func() time.Time
	targets lru[*paceState]
}

type paceState struct {
	tokens         float64
	updatedAt      time.Time
	sentInBurst    int
	burstStartedAt time.Time
}

// NewPacer builds a pacer; a nil now uses time.Now.
func NewPacer(l GuardLimits, now func() time.Time) *Pacer {
	if now == nil {
		now = time.Now
	}
	return &Pacer{limits: l, now: now}
}

func (p *Pacer) state(key string, now time.Time) *paceState {
	capacity, rate := p.limits.BucketCapacity, p.limits.RefillPerSecond
	st, ok := p.targets.get(key)
	if !ok {
		st = &paceState{tokens: capacity, updatedAt: now, burstStartedAt: now}
		p.targets.put(key, st, p.limits.MaxTrackedSenders, func(s *paceState) bool {
			return refill(s.tokens, s.updatedAt, now, capacity, rate) >= capacity
		})
		return st
	}
	st.tokens = refill(st.tokens, st.updatedAt, now, capacity, rate)
	st.updatedAt = now
	if rate < 1e-9 {
		rate = 1e-9
	}
	burst := time.Duration(capacity / rate * float64(time.Second))
	if st.tokens >= capacity || now.Sub(st.burstStartedAt) > burst {
		st.sentInBurst = 0
		st.burstStartedAt = now
	}
	return st
}

// Reserve takes one token for target. ok=false carries the burst count for
// the error; on ok, refund returns the token (a send that failed).
func (p *Pacer) Reserve(target string) (ok bool, sentInBurst int, refund func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(target, p.now())
	if st.tokens < 1 {
		return false, st.sentInBurst, nil
	}
	st.tokens--
	st.sentInBurst++
	done := false
	return true, 0, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if done {
			return
		}
		done = true
		st.tokens = min(p.limits.BucketCapacity, st.tokens+1)
		st.sentInBurst = max(0, st.sentInBurst-1)
	}
}
