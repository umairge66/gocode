// Package inbox implements the inter-agent / teammate messaging subsystem:
// the Message record relayed between agents, the per-agent inbox queues it
// lands in, and the Send / Fetch / Ack lifecycle by which an agent delivers a
// message to a teammate and reads its next inbound delivery.
//
// It covers the FetchInboxMessage semantics (what a session inbox is, reading
// by file_id, the Message fields: message_id, body, sender_display,
// sender_kind, source, slack_permalink, received_at, attachments), the
// SendMessage {to, summary, message, notify_when_idle} input, the
// recipient-kind classifier and the send-failure reason taxonomy, the ingress
// origin classification (incl. the peer-send-message task-notification
// subkind) and the inbox pointer/ack tracking.
//
// SCOPE. Two delivery paths exist. Teammate/peer SendMessage
// delivers a model-authored message into another session's queue; Remote
// Control's session inbox relays third-party messages pulled by file_id over a
// network bridge. The Remote Control bridge (fetchInboxMessage, the remote
// fetch) is a hosted service and is not supported by design. SendMessage's
// delivery to other local gocode processes of the same OS user is supported
// (peeraddr.go, peerguard.go, peerlisten.go, peersend.go, peer_unix.go,
// peer_windows.go: the uds-messaging inbox socket / named pipe, the
// <cross-session-message> envelope, the ingress guard and sender pacing).
// Bridge, did and cloud delivery are not supported. The in-process core is a
// concurrency-safe per-agent FIFO queue.
package inbox

import (
	"regexp"
	"time"
)

// SenderKind classifies who an inbox Message is from. The value is self-chosen
// wire text for everything except the owner marker, which the relay server
// verifies. It covers the sender_kind field of the FetchInboxMessage result
// and the origin kinds produced by the ingress classifier.
type SenderKind string

const (

	// SenderUnknown is the demoted/unattested sender used when the bridge could
	// not attribute a message ({kind:"peer",from:"unknown"}, and the
	// senderKind:"unknown" downgrade in the fetch tool); it is read as
	// third-party peer text.
	SenderUnknown SenderKind = "unknown"

	// SenderPeer is a teammate peer (another agent).
	SenderPeer SenderKind = "peer"

	// SenderCoordinator is the coordinator teammate / a co-member SendMessage
	// delivery (the "coordinator" gating class for a peer-send-message
	// task-notification).
	SenderCoordinator SenderKind = "coordinator"
)

// SummaryMaxChars caps the SendMessage summary label; longer summaries are
// truncated rather than rejected.
const SummaryMaxChars = 200

// Message is one inbox record: a message sent from one agent/teammate to
// another, or relayed into a session. The field set is the FetchInboxMessage
// result shape joined with the SendMessage input (to/summary/message).
type Message struct {
	// ID is the server/queue message identifier (message_id).
	ID string

	// FileID is the pointer id a session-inbox notification announces and that
	// FetchInboxMessage reads by. Empty for a purely in-process delivery.
	FileID string

	// From is the sender's self-chosen display name (sender_display).
	From string

	// FromKind classifies the sender (and, for the owner marker, attests it).
	FromKind SenderKind

	// To is the recipient address: a teammate name, "main", or a background
	// agent's name or agentId (the SendMessage `to` field).
	To string

	// Summary is the optional 5-10 word label shown in the UI; not transmitted
	// to the recipient (the SendMessage `summary` field).
	Summary string

	// Body is the message text (the plain-text form; structured block content
	// is flattened to its text): body, or the SendMessage `message` field.
	Body string

	// Source is the origin surface of a relayed message.
	Source string

	// SlackPermalink is the untrusted permalink carried by a Slack-relayed
	// message (slack_permalink).
	SlackPermalink string

	// Attachments are @path references to files downloaded under the session's
	// uploads directory.
	Attachments []string

	// CreatedAt is when the message was received/created (received_at).
	CreatedAt time.Time

	// Fetched records that Fetch has handed this message to its recipient (the
	// tracker's `delivered` flag). Peek never sets it; Ack removes the message.
	Fetched bool
}

// MainRecipient is the reserved recipient naming the parent/main conversation.
const MainRecipient = "main"

// Recipient detectors for ClassifyRecipient.
var (
	// socketPathRe / pipePathRe are the bare local-socket address shapes
	// treated as "uds".
	socketPathRe = regexp.MustCompile(`^/\S*\.sock$`)
	pipePathRe   = regexp.MustCompile(`(?i)^[\\/]{2}[.?][\\/]pipe[\\/]`)
)

// ValidRecipient reports whether a recipient address is a single line
// (/^[^\n\r]*$/u). A max-length bound is not enforced here.
func ValidRecipient(to string) bool {
	if to == "" {
		return false
	}
	for i := 0; i < len(to); i++ {
		if to[i] == '\n' || to[i] == '\r' {
			return false
		}
	}
	return true
}
