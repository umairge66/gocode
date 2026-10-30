package inbox

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxLineChars is the longest line (one JSON frame) an inbox accepts; a
// longer one drops the connection. Measured in bytes (never fewer than UTF-16
// units, so the cap is at least as strict as a UTF-16 count).
const MaxLineChars = 1 << 20

// DefaultFirstLineDeadline: a connection that sends no complete line
// within it is closed.
const DefaultFirstLineDeadline = 30 * time.Second

// msgIDRe: a msg_id is a UUID.
var msgIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ListenerConfig configures a cross-session inbox listener.
type ListenerConfig struct {
	// Path is the socket path (unix) or pipe name (Windows); "" uses
	// DefaultSocketPath and vets/creates its private directory. An explicit
	// path must sit in an existing private (0700) directory.
	Path string
	// Store receives admitted messages. Required.
	Store *Store
	// Recipient is the queue admitted messages land in (default "main").
	Recipient string
	// SessionID, when a frame carries session_id, must match it.
	SessionID string
	// Accept is a plain accept/refuse gate; nil accepts. A refused message is
	// dropped. Gate, when set, is consulted instead.
	Accept func(from string) bool
	// Gate is the crossSessionInbound / mode-parity gate (see DecideInbound):
	// accept queues the message, refuse drops it, hold parks it in the
	// listener's hold buffer until Reevaluate or Resolve releases it.
	Gate func(from string, env Envelope) InboundDecision
	// OnQueued is called (outside the listener's lock) after a message is
	// queued in Store — the wake-up hook an idle session uses to start a turn.
	OnQueued func(Message)
	// OnHeld is called when a message is parked (with the held count).
	OnHeld func(h HeldMessage, held int)
	// OnReleased is called when Reevaluate releases held messages to the
	// queue, with the reason ("mode-changed", "policy-accepts").
	OnReleased func(released []HeldMessage, reason string)
	// Limits overrides DefaultGuardLimits.
	Limits *GuardLimits
	// FirstLineDeadline overrides DefaultFirstLineDeadline.
	FirstLineDeadline time.Duration
	// Logf receives debug lines (nil discards). Never sent anywhere.
	Logf func(format string, args ...any)

	// peerCheck overrides checkInboundPeer (tests only).
	peerCheck func(net.Conn) error
}

// Listener is a session's cross-session inbox: other local gocode sessions of
// the same OS user connect and write newline-delimited JSON frames; admitted
// "user" frames are queued in the Store as untrusted peer messages framed in
// <cross-session-message>.
type Listener struct {
	cfg      ListenerConfig
	ln       net.Listener
	path     string
	address  string
	hopToken string
	guard    *IngressGuard
	limits   GuardLimits
	// checkPeer authorizes an accepted connection (checkInboundPeer; a
	// test seam).
	checkPeer func(net.Conn) error

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup

	// held is the hold buffer, oldest first.
	heldMu  sync.Mutex
	held    []*HeldMessage
	heldSeq uint64
}

// Listen binds the inbox and starts serving it.
func Listen(cfg ListenerConfig) (*Listener, error) {
	if cfg.Store == nil {
		return nil, errors.New("inbox: Listen needs a Store")
	}
	if cfg.Recipient == "" {
		cfg.Recipient = MainRecipient
	}
	if cfg.FirstLineDeadline <= 0 {
		cfg.FirstLineDeadline = DefaultFirstLineDeadline
	}
	limits := DefaultGuardLimits()
	if cfg.Limits != nil {
		limits = *cfg.Limits
	}
	path, explicit := cfg.Path, cfg.Path != ""
	if !explicit {
		path = DefaultSocketPath()
	}
	if !isLocalIPCPath(path) {
		return nil, errors.New("not a usable local socket address (a remote/UNC path, or a pipe name with extra segments or a trailing dot/space): " + path)
	}
	ln, bound, err := listenLocal(path, explicit)
	if err != nil {
		return nil, err
	}
	l := &Listener{
		cfg:     cfg,
		ln:      ln,
		path:    bound,
		address: UDSAddress(bound),
		guard:   NewIngressGuard(limits, nil),
		limits:  limits,
		conns:   make(map[net.Conn]struct{}),

		checkPeer: checkInboundPeer,
	}
	if cfg.peerCheck != nil {
		l.checkPeer = cfg.peerCheck
	}
	l.hopToken = HopToken(l.address)
	l.wg.Add(1)
	go l.serve()
	l.logf("[uds-messaging] Listening: %s", bound)
	return l, nil
}

// Path is the bound socket path / pipe name.
func (l *Listener) Path() string { return l.path }

// Address is the "uds:" address peers send to.
func (l *Listener) Address() string { return l.address }

// HopToken is this session's own hop token.
func (l *Listener) HopToken() string { return l.hopToken }

// Close stops accepting, drops open connections and removes the socket.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	for c := range l.conns {
		_ = c.Close()
	}
	l.mu.Unlock()
	err := l.ln.Close()
	l.wg.Wait()
	return err
}

func (l *Listener) logf(format string, args ...any) {
	if l.cfg.Logf != nil {
		l.cfg.Logf(format, args...)
	}
}

func (l *Listener) serve() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if perr := l.checkPeer(conn); perr != nil {
			l.logf("[uds-messaging] rejecting connection: %v", perr)
			_ = conn.Close()
			continue
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return
		}
		l.conns[conn] = struct{}{}
		l.wg.Add(1)
		l.mu.Unlock()
		go func() {
			defer l.wg.Done()
			l.handleConn(conn)
			l.mu.Lock()
			delete(l.conns, conn)
			l.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

// handleConn reads newline-delimited frames: a first-line deadline, the line
// cap, unparseable lines skipped, and a final unterminated fragment parsed at
// EOF.
func (l *Listener) handleConn(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(l.cfg.FirstLineDeadline))
	gotLine := false
	r := bufio.NewReaderSize(conn, 64*1024)
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(bytes.TrimSuffix(buf, []byte("\n"))) > MaxLineChars {
			l.logf("[uds-messaging] Line exceeded %d chars; dropping connection", MaxLineChars)
			return
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == nil {
			line := bytes.TrimSuffix(buf, []byte("\n"))
			buf = nil
			if !gotLine {
				gotLine = true
				_ = conn.SetReadDeadline(time.Time{})
			}
			l.handleLine(line)
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(bytes.TrimSpace(buf)) > 0 {
				l.handleLine(buf)
			}
			return
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && !gotLine {
			l.logf("[uds-messaging] Closing a connection that sent no complete line within %s", l.cfg.FirstLineDeadline)
		}
		return
	}
}

// wireFrame is the JSON line shape ({type, message:{role,content},
// priority, from, msg_id, session_id, ...}).
type wireFrame struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	From      json.RawMessage `json:"from,omitempty"`
	MsgID     json.RawMessage `json:"msg_id,omitempty"`
	SessionID json.RawMessage `json:"session_id,omitempty"`
	Action    string          `json:"action,omitempty"`
}

func rawString(r json.RawMessage) (string, bool) {
	if len(r) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return "", false
	}
	return s, true
}

func (l *Listener) handleLine(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var f wireFrame
	if err := json.Unmarshal(line, &f); err != nil || f.Type == "" {
		l.logf("[uds-messaging] Ignoring message without valid type field")
		return
	}
	switch f.Type {
	case "auth":
		// The per-session token frame. Peers are
		// authenticated by OS user here; a token frame is accepted and ignored.
	case "user":
		l.deliverUser(f)
	case "control":
		// peer_message_status / notify_when_idle / rename / artifact replies
		// are not supported (no receipts or idle subscriptions).
		l.logf("[uds-messaging] Unhandled control action: %s", f.Action)
	default:
		l.logf("[uds-messaging] Received unhandled message type: %s", f.Type)
	}
}

// deliverUser handles an inbound user message: validate, gate, guard, frame and queue.
func (l *Listener) deliverUser(f wireFrame) {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if len(f.Message) > 0 {
		_ = json.Unmarshal(f.Message, &msg)
	}
	content, ok := rawString(msg.Content)
	if !ok || content == "" {
		l.logf("[uds-messaging] Ignoring user message with missing or non-string content")
		return
	}
	if len(f.SessionID) > 0 {
		if sid, _ := rawString(f.SessionID); sid != l.cfg.SessionID || l.cfg.SessionID == "" {
			l.logf("[uds-messaging] Dropping user message: session_id mismatch")
			return
		}
	}
	from := "unknown"
	if s, ok := rawString(f.From); ok && IsPeerAddress(s) {
		from = s
	}
	// Framing: the sender wraps its text in <cross-session-message>; text that
	// is not exactly a canonical envelope is wrapped here, so the receiving
	// model always sees peer text inside the untrusted wrapper.
	env, isEnv := ParseEnvelope(content)
	if !isEnv {
		e := Envelope{Body: content}
		if from != "unknown" {
			e.From = from
		}
		content = FrameEnvelope(e)
		env, _ = ParseEnvelope(content)
	}
	msgID := ""
	if id, ok := rawString(f.MsgID); ok && msgIDRe.MatchString(id) {
		msgID = id
	}

	decision := InboundDecision{Policy: InboundAccept}
	switch {
	case l.cfg.Gate != nil:
		decision = l.cfg.Gate(from, env)
	case l.cfg.Accept != nil && !l.cfg.Accept(from):
		decision = InboundDecision{Policy: InboundRefuse, RefuseCause: "opt-out"}
	}
	switch decision.Policy {
	case InboundRefuse:
		if decision.RefuseCause == "kill-switch" {
			l.logf("[cross-session-inbound] refused inbound peer message — cross-session messaging disabled (kill switch)")
		} else {
			l.logf("[cross-session-inbound] refused inbound peer message (crossSessionInbound=refuse)")
		}
		return
	case InboundHold:
		l.hold(HeldMessage{From: from, FromName: env.FromName, FromMode: env.FromMode, Content: content, Body: env.Body, Cause: decision.HoldCause, msgID: msgID})
		return
	}
	if m, ok := l.admit(from, content, env.HopChain, msgID); ok && l.cfg.OnQueued != nil {
		l.cfg.OnQueued(m)
	}
}

// admit runs the ingress guard (rate limit, duplicate, hop loop) and the
// queue cap, then queues the framed message.
func (l *Listener) admit(from, content string, chain []string, msgID string) (Message, bool) {
	senderKey := "from:" + from
	own := map[string]bool{l.hopToken: true}
	if r := l.guard.Admit(senderKey, content, chain, own); r != "" {
		l.logf("[peer-guard] Dropped peer message from %s: %s", from, r.Describe())
		return Message{}, false
	}
	if l.cfg.Store.Len(l.cfg.Recipient) >= l.limits.MaxQueuedPeerMessages {
		l.logf("[peer-guard] Dropped peer message from %s: %s", from, DropQueueFull.Describe())
		return Message{}, false
	}
	m := Message{
		ID:       msgID,
		From:     from,
		FromKind: SenderPeer,
		To:       l.cfg.Recipient,
		Body:     content,
		Source:   "uds",
	}
	res := l.cfg.Store.enqueueLocal(m)
	m.ID = res.ID
	l.logf("[uds-messaging] Routed user message to queue")
	return m, true
}

// hold parks h: the buffer keeps maxHeld messages,
// evicting the oldest as expired.
func (l *Listener) hold(h HeldMessage) {
	l.heldMu.Lock()
	l.heldSeq++
	h.ID = "held_" + strconv.FormatUint(l.heldSeq, 10)
	if len(l.held) >= maxHeld {
		l.logf("[cross-session-inbound] hold buffer full — evicted oldest as expired")
		l.held = l.held[1:]
	}
	hp := h
	l.held = append(l.held, &hp)
	n := len(l.held)
	l.heldMu.Unlock()
	l.logf("[cross-session-inbound] held inbound peer message (%d held, cause=%s)", n, h.Cause)
	if l.cfg.OnHeld != nil {
		l.cfg.OnHeld(h, n)
	}
}

// Held returns a snapshot of the parked messages, oldest first.
func (l *Listener) Held() []HeldMessage {
	l.heldMu.Lock()
	defer l.heldMu.Unlock()
	out := make([]HeldMessage, len(l.held))
	for i, h := range l.held {
		out[i] = *h
	}
	return out
}

// Resolve approves or denies one held message. It returns
// "delivered", "dropped" (approved but the gate now refuses), "dropped-by-guard"
// (approved but the ingress guard had no room), or "gone" (already resolved).
func (l *Listener) Resolve(id string, approve bool) string {
	l.heldMu.Lock()
	var h *HeldMessage
	for i, x := range l.held {
		if x.ID == id {
			h = x
			l.held = append(l.held[:i:i], l.held[i+1:]...)
			break
		}
	}
	l.heldMu.Unlock()
	if h == nil {
		return "gone"
	}
	if !approve {
		l.logf("[cross-session-inbound] held peer message DENIED — dropped")
		return "dropped"
	}
	if l.cfg.Gate != nil {
		env, _ := ParseEnvelope(h.Content)
		if l.cfg.Gate(h.From, env).Policy == InboundRefuse {
			l.logf("[cross-session-inbound] held peer message approved but policy is now refuse — dropped")
			return "dropped"
		}
	}
	env, _ := ParseEnvelope(h.Content)
	m, ok := l.admit(h.From, h.Content, env.HopChain, h.msgID)
	if !ok {
		return "dropped-by-guard"
	}
	l.logf("[cross-session-inbound] held peer message APPROVED — released to queue")
	if l.cfg.OnQueued != nil {
		l.cfg.OnQueued(m)
	}
	return "delivered"
}

// Reevaluate re-runs the gate over every held message after a mode or
// setting change — accepted ones are released to the queue, refused ones
// dropped, the rest stay held. It returns how many were released.
func (l *Listener) Reevaluate(reason string) int {
	if l.cfg.Gate == nil {
		return 0
	}
	l.heldMu.Lock()
	var keep, release []*HeldMessage
	for _, h := range l.held {
		env, _ := ParseEnvelope(h.Content)
		switch d := l.cfg.Gate(h.From, env); d.Policy {
		case InboundAccept:
			release = append(release, h)
		case InboundRefuse:
		default:
			h.Cause = d.HoldCause
			keep = append(keep, h)
		}
	}
	l.held = keep
	l.heldMu.Unlock()
	if len(release) == 0 {
		return 0
	}
	var admitted []HeldMessage
	var queued []Message
	for _, h := range release {
		env, _ := ParseEnvelope(h.Content)
		if m, ok := l.admit(h.From, h.Content, env.HopChain, h.msgID); ok {
			admitted = append(admitted, *h)
			queued = append(queued, m)
		}
	}
	l.logf("[cross-session-inbound] released %d held peer message(s) (%s) — %d admitted by the ingress guard", len(release), reason, len(admitted))
	if len(admitted) > 0 && l.cfg.OnReleased != nil {
		l.cfg.OnReleased(admitted, reason)
	}
	if l.cfg.OnQueued != nil {
		for _, m := range queued {
			l.cfg.OnQueued(m)
		}
	}
	return len(release)
}

// randomHex returns 2n random lowercase hex characters.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newMsgID mints a v4 UUID.
func newMsgID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// hasDotDot reports a ".." path segment.
func hasDotDot(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}
