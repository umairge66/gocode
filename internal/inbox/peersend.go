package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Error classes of a failed cross-session send.
const (
	errClassTooLarge = "message_too_large"
	errClassPaced    = "sender_paced"
	errClassRefused  = "refused" // UdsSendRefusedError
	errClassSelf     = "self_target"
	errClassInvalid  = "invalid_target"
)

// DefaultListTool is the tool name the address errors point at. The
// list tool is "ListAgents"; "ListPeers" survives only as its legacy alias.
const DefaultListTool = "ListAgents"

// sendTimeout is the 5s connect/write timeout.
const sendTimeout = 5 * time.Second

// authLineLen is the length of a full auth line
// `{"type":"auth","token":"<32 hex>"}\n`, reserved in the size check.
var authLineLen = len(`{"type":"auth","token":"`+"00000000000000000000000000000000"+`"}`) + 1

// PeerSendError is a refused or failed cross-session send with its
// message text.
type PeerSendError struct {
	Class string
	Kind  string
	Msg   string
}

func (e *PeerSendError) Error() string { return e.Msg }

// commaInt formats an int like toLocaleString("en-US").
func commaInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// errTooLarge is the message-too-large error for size and limit.
func errTooLarge(size, limit int) error {
	return &PeerSendError{Class: errClassTooLarge, Msg: fmt.Sprintf("Message too large for cross-session delivery: the serialized message is %s characters and the limit is %s. Shorten the message text — put bulk content in a file the recipient can read rather than in the message — or split it into smaller messages.", commaInt(size), commaInt(limit))}
}

// errPaced is the sender-paced error for the number sent in the burst.
func errPaced(n int) error {
	return &PeerSendError{Class: errClassPaced, Msg: fmt.Sprintf("Too many messages to this session just now: %d were sent recently and more would be dropped by its rate limit, so this one was not sent. Batch what remains into one message, or wait a little before sending more.", n)}
}

// PeerSender delivers SendMessage text to other local gocode sessions'
// inboxes (uds: addresses).
type PeerSender struct {
	// OwnPath is this session's own inbox path ("" when it has none: the
	// envelope then carries no from= and replies are impossible).
	OwnPath string
	// ListTool is named in address errors (default DefaultListTool).
	ListTool string
	// Pacer mirrors the recipients' rate limits (nil: a default pacer).
	Pacer *Pacer
	// Timeout overrides the 5s send timeout.
	Timeout time.Duration
}

// NewPeerSender returns a sender whose replies route to l (nil l: no inbox).
func NewPeerSender(l *Listener) *PeerSender {
	s := &PeerSender{ListTool: DefaultListTool, Pacer: NewPacer(DefaultGuardLimits(), nil)}
	if l != nil {
		s.OwnPath = l.Path()
	}
	return s
}

// PeerSendOptions are the optional envelope attributes.
type PeerSendOptions struct {
	// FromName is the sender's display name (from-name=).
	FromName string
	// HopChain is the chain inherited from the peer message being answered
	//; nil when this send is not a relay.
	HopChain []string
	// FromPlugin names a plugin hook caller (from-plugin=).
	FromPlugin string
	// FromMode is the sender's permission-mode class ("bypass" or
	// "prompting", from-mode=) the receiver's mode-parity gate compares
	// against its own; "" asserts none.
	FromMode string
}

// userFrame is the wire line, keys in wire order
// ({msgV,msg_id,type,message,priority,from}).
type userFrame struct {
	MsgV    int    `json:"msgV"`
	MsgID   string `json:"msg_id"`
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Priority   string `json:"priority"`
	From       string `json:"from,omitempty"`
	FromPlugin string `json:"from_plugin,omitempty"`
}

// Send delivers body to the inbox at `to`, returning the frame's msg_id.
func (s *PeerSender) Send(ctx context.Context, to, body string, opts PeerSendOptions) (string, error) {
	listTool := s.ListTool
	if listTool == "" {
		listTool = DefaultListTool
	}
	if msg := ValidateAddress(to, listTool); msg != "" {
		return "", &PeerSendError{Class: errClassInvalid, Msg: msg}
	}
	p := ParseAddress(to)
	if p.Scheme != SchemeUDS {
		return "", &PeerSendError{Class: errClassInvalid, Msg: "Only local gocode sessions (uds: addresses) are reachable from this session; bridge, did and cloud delivery are not available."}
	}
	target := p.Target
	if s.OwnPath != "" && canonicalEndpoint(target) != "" && canonicalEndpoint(target) == canonicalEndpoint(s.OwnPath) {
		return "", &PeerSendError{Class: errClassSelf, Msg: fmt.Sprintf("'%s' is this session itself — there is no one else at that address to send to.", to)}
	}

	from := ""
	ownToken := ""
	if s.OwnPath != "" {
		from = UDSAddress(s.OwnPath)
		ownToken = HopToken(from)
	}
	f := userFrame{MsgV: 1, MsgID: newMsgID(), Type: "user", Priority: "next", From: from, FromPlugin: opts.FromPlugin}
	f.Message.Role = "user"
	f.Message.Content = FrameEnvelope(Envelope{
		From:       from,
		FromName:   opts.FromName,
		Body:       body,
		HopChain:   appendHop(opts.HopChain, ownToken),
		FromMode:   modeClassAttr(opts.FromMode),
		FromPlugin: opts.FromPlugin,
	})
	line, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	if n := authLineLen + len(line) + 1; n > MaxLineChars {
		return "", errTooLarge(n, MaxLineChars)
	}

	pacer := s.Pacer
	if pacer == nil {
		pacer = NewPacer(DefaultGuardLimits(), nil)
		s.Pacer = pacer
	}
	key := canonicalEndpoint(target)
	if key == "" {
		key = target
	}
	ok, burst, refund := pacer.Reserve(key)
	if !ok {
		return "", errPaced(burst)
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = sendTimeout
	}
	conn, err := dialLocal(ctx, target, timeout)
	if err != nil {
		if refundable(err) {
			refund()
		}
		return "", err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		if refundable(err) {
			refund()
		}
		return "", err
	}
	return f.MsgID, nil
}

// refundable reports a send that certainly did not reach the inbox gives its
// pacer token back.
func refundable(err error) bool {
	var pe *PeerSendError
	return errors.As(err, &pe) || isGoneError(err) || isBusyError(err)
}

// FailureMessage renders the SendMessage tool's failure text for a send error:
// `Failed to send to <to>: <error><hint>`, the hint naming a stale socket
// (gone) or a busy pipe.
func FailureMessage(to string, err error, listTool string) string {
	if listTool == "" {
		listTool = DefaultListTool
	}
	hint := ""
	switch {
	case isGoneError(err):
		hint = " — the peer process may have restarted, so this socket path is stale. Call " + listTool + " to get the current address."
	case isBusyError(err):
		hint = " — the peer is alive but its pipe is momentarily busy. Retry the same address shortly."
	}
	return "Failed to send to " + to + ": " + err.Error() + hint
}

// sendReasonFor maps a send error onto the SendReason taxonomy.
func sendReasonFor(err error) SendReason {
	var pe *PeerSendError
	if errors.As(err, &pe) {
		switch pe.Class {
		case errClassSelf:
			return ReasonSelfTarget
		case errClassInvalid:
			return ReasonUnreachableNamespace
		}
		return ReasonSendFailed
	}
	if isGoneError(err) {
		return ReasonPeerGone
	}
	return ReasonSendFailed
}

// modeClassAttr keeps only the two known from-mode values.
func modeClassAttr(m string) string {
	if m == ModeClassBypass || m == ModeClassPrompting {
		return m
	}
	return ""
}
