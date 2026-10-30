package inbox

// The model-facing SendMessage and ListAgents tools for local cross-session
// messaging.
//
// Scope: delivery is to other gocode sessions of the SAME OS user on this
// machine (uds: addresses or registry names), to this session's own
// subagents by name or task id (agentmessage.go), and to "main" from a
// subagent. Teammates, Remote Control / bridge / cloud
// sessions, gocode Desktop, notify_when_idle subscriptions and delivery
// receipts are not supported: they need hosted services or the agent-teams
// runtime. Nothing here sends anything anywhere but a local socket.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Tool names.
const (
	SendMessageToolName = "SendMessage"
	ListAgentsToolName  = "ListAgents"
)

// mainAddress is the name a subagent uses for the main conversation.
const mainAddress = "main"

// recipientMax is the maximum recipient length (300).
const recipientMax = 300

const errNotAvailable = "Cross-session messaging is not available in this session."

const thisMachine = "another gocode session on this machine"

// PeerMessaging is one session's cross-session messaging state, shared by its
// SendMessage and ListAgents tools.
type PeerMessaging struct {
	// Sender delivers to other sessions' inboxes; nil disables cross-session
	// sends.
	Sender *PeerSender
	// Listener is this session's own inbox (nil: no inbox — sends are
	// one-way and replies cannot reach this session).
	Listener *Listener
	// RegistryDir is the local session registry (SessionsDir); "" disables
	// discovery by name.
	RegistryDir string
	// Enabled is the messaging kill switch (see MessagingEnabled).
	Enabled bool
	// SelfName returns this session's registered name ("" when none).
	SelfName func() string
	// SenderMode returns this session's permission-mode class for from-mode=.
	SenderMode func() string
	// QueueMain queues a message for the main conversation (the target of
	// "main" from a subagent): it is delivered at the main conversation's
	// next tool round, or wakes it when idle; nil when unavailable.
	QueueMain func(m AgentMessage)
	// Agents is this session's in-process agents (the Agent tool's spawns),
	// addressable by name or task id; nil when none can be spawned.
	Agents AgentDirectory
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

func (p *PeerMessaging) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *PeerMessaging) ownSock() string {
	if p.Listener == nil {
		return ""
	}
	return p.Listener.Path()
}

func (p *PeerMessaging) selfName() string {
	if p.SelfName == nil {
		return ""
	}
	return validSessionName(p.SelfName())
}

// --- names and refs -----------------------------------------------------------

// normName: NFKC, invisible characters dropped, trimmed, lower-cased,
// whitespace runs to "-".
func normName(s string) string {
	s = norm.NFKC.String(s)
	s = strings.Map(func(r rune) rune {
		if isControlOrFormat(r) && !isSpaceRune(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), "-")
}

// validSessionName returns the cleaned name when it is addressable — not an
// address, not containing "@", not "*", not a reserved name — else "".
func validSessionName(s string) string {
	if s == "" {
		return ""
	}
	n := cleanSessionName(s)
	if strings.Contains(n, "@") || n == "*" || ParseAddress(n).Scheme != SchemeOther ||
		ParseAddress(normName(n)).Scheme != SchemeOther || normName(n) == mainAddress {
		return ""
	}
	return n
}

// fullRef is the first 12 hex digits of
// sha256("session:<sock>").
func fullRef(sock string) string {
	sum := sha256.Sum256([]byte("session:" + sock))
	return hex.EncodeToString(sum[:])[:12]
}

// parseNameRef parses the "name [ref]" shape (ref = [0-9a-f]{6,12}).
func parseNameRef(s string) (name, ref string, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "]") {
		return "", "", false
	}
	i := strings.LastIndex(s, "[")
	if i < 0 {
		return "", "", false
	}
	ref = s[i+1 : len(s)-1]
	if len(ref) < 6 || len(ref) > 12 || strings.Trim(ref, "0123456789abcdef") != "" {
		return "", "", false
	}
	name = strings.TrimRight(s[:i], " \t")
	if name == "" {
		return "", "", false
	}
	return name, ref, true
}

// candidate is one addressable local session.
type candidate struct {
	rec  SessionRecord
	name string // "" when the session has no addressable name
	full string
	ref  string
}

func (c candidate) token() string { return c.name + " [" + c.ref + "]" }

// assignRefs: each ref is the shortest prefix (at least 6) that differs
// from its sorted neighbours, this session's own ref included.
func assignRefs(cands []candidate, ownSock string) {
	all := make([]string, 0, len(cands)+1)
	for _, c := range cands {
		all = append(all, c.full)
	}
	if ownSock != "" {
		all = append(all, fullRef(ownSock))
	}
	sort.Strings(all)
	lcp := func(a, b string) int {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		return n
	}
	length := map[string]int{}
	for i, f := range all {
		m := 0
		if i > 0 && all[i-1] != f {
			m = lcp(f, all[i-1])
		}
		if i+1 < len(all) && all[i+1] != f {
			if l := lcp(f, all[i+1]); l > m {
				m = l
			}
		}
		n := m + 1
		if n < 6 {
			n = 6
		}
		if n > len(f) {
			n = len(f)
		}
		length[f] = n
	}
	for i := range cands {
		cands[i].ref = cands[i].full[:length[cands[i].full]]
	}
}

// listCandidates lists the live local sessions as addressable candidates.
func (p *PeerMessaging) listCandidates() ([]candidate, error) {
	if p.RegistryDir == "" {
		return nil, nil
	}
	recs, err := ListLiveSessions(p.RegistryDir, p.ownSock())
	out := make([]candidate, 0, len(recs))
	for _, r := range recs {
		out = append(out, candidate{rec: r, name: validSessionName(r.DisplayName()), full: fullRef(r.MessagingSocketPath)})
	}
	assignRefs(out, p.ownSock())
	return out, err
}

// selfToken is V/Y's "name [ref]" for this session ("" when it has no name or
// no inbox).
func (p *PeerMessaging) selfToken() string {
	n, sock := p.selfName(), p.ownSock()
	if n == "" || sock == "" {
		return ""
	}
	return n + " [" + fullRef(sock)[:6] + "]"
}

// --- formatting helpers -------------------------------------------------------

// ago formats a duration, most significant unit only.
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	if ms < 60000 {
		if ms == 0 {
			return "0s"
		}
		return fmt.Sprintf("%ds", ms/1000)
	}
	days, hours, mins := ms/86400000, ms%86400000/3600000, ms%3600000/60000
	switch {
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", mins)
}

// truncateWidth keeps at most n characters, the last replaced by "…".
func truncateWidth(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// jsonText is `_`: JSON without HTML escaping.
func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

// sendResult is the SendMessage data object the model sees (display omitted).
type sendResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	MsgID   string `json:"msg_id,omitempty"`
}

func resultText(ok bool, msg, id string) string {
	return jsonText(sendResult{Success: ok, Message: msg, MsgID: id})
}

// =============================================================================
// ListAgents
// =============================================================================

// ListAgentsTool is the ListAgents tool (alias ListPeers).
type ListAgentsTool struct {
	P *PeerMessaging
	// CallerIsSubagent selects the subagent variant of the self line.
	CallerIsSubagent bool
	// CallerCanSend is whether the caller also has SendMessage.
	CallerCanSend bool
}

// listAgentsDescription is for callers that can send;
// listAgentsDescriptionNoSend for callers that cannot.
const listAgentsDescription = "Lists agents you can SendMessage to — in-process subagents you spawned, the teammates on your team, other local gocode sessions on this machine, your gocode sessions running in the cloud (when this session has cloud access; a cloud session receives your message but cannot message any session back yet — do not ask it to reply, read its answer in its own transcript), and (when Remote Control is connected here) your account's other sessions — Remote Control sessions on other machines and cloud sessions, each row labeled by kind. Names are the address: send with `SendMessage({to: \"<name>\", message: \"...\"})`, copying the name exactly as a row prints it. Append a row's ` [ref]` only when the bare name is not enough — two rows share it, or an error asks you to disambiguate."

const listAgentsDescriptionNoSend = "Lists the agents and gocode sessions this session can see — in-process subagents you spawned, the teammates on your team, other local gocode sessions on this machine, your gocode sessions running in the cloud (when this session has cloud access), and (when Remote Control is connected here) your account's other sessions, each row labeled by kind — plus this session's own name, the one other sessions use to message it. This session has no SendMessage tool, so it cannot message them with it; other sessions can still message this one. To reply, use your host application's own messaging tool if it provides one — otherwise a reply from here is not possible, and if one is needed, tell your user."

// Name implements the model tool.
func (t *ListAgentsTool) Name() string { return ListAgentsToolName }

// Description returns the tool description.
func (t *ListAgentsTool) Description() string {
	if t.CallerCanSend {
		return listAgentsDescription
	}
	return listAgentsDescriptionNoSend
}

// InputSchema declares channel and q, both unavailable in this build.
func (t *ListAgentsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"channel":{"type":"string","maxLength":256,"description":"Not available in this build; leave unset."},` +
		`"q":{"type":"string","maxLength":256,"description":"Not available in this build; leave unset."}` +
		`},"additionalProperties":false}`)
}

// Execute returns the listing text (the tool result content is the listing).
func (t *ListAgentsTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(input)) > 0 {
		var in struct {
			Channel *string `json:"channel"`
			Q       *string `json:"q"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %v", err)
		}
	}
	return t.P.Listing(t.CallerIsSubagent, t.CallerCanSend), nil
}

const noteMessagingDisabled = "Cross-session messaging is switched off in this session right now — no sessions were listed, and this session is not reachable by name until it is switched back on."

const noteCannotSend = "None of the agents below can be messaged with SendMessage from here: it is not among the tools available to you."

const noteLocalListFailed = "(the session list on this machine could not be read just now — other local sessions may be missing from this listing; a later listing retries)"

// Listing renders the local-session section of the peer listing.
func (p *PeerMessaging) Listing(callerIsSubagent, callerCanSend bool) string {
	if !p.Enabled {
		return noteMessagingDisabled
	}
	cands, err := p.listCandidates()
	now := p.now()
	var lines []string
	for _, c := range cands {
		printed := c.name
		if printed == "" {
			printed = cleanSessionName(c.rec.DisplayName())
		} else {
			printed = c.token()
		}
		parts := []string{printed}
		if c.rec.Kind != "" {
			parts = append(parts, c.rec.Kind)
		}
		if c.rec.Status != "" {
			parts = append(parts, c.rec.Status)
		}
		if c.rec.StartedAt > 0 {
			parts = append(parts, "started "+ago(now.Sub(time.UnixMilli(c.rec.StartedAt)))+" ago")
		}
		lines = append(lines, "  "+strings.Join(parts, "  ·  "))
	}
	var sections []string
	if len(lines) > 0 || err != nil {
		text := fmt.Sprintf("Peer sessions (%d):\n%s", len(lines), strings.Join(lines, "\n"))
		if err != nil {
			if len(lines) > 0 {
				text += "\n  " + noteLocalListFailed
			} else {
				text = noteLocalListFailed
			}
		}
		sections = append(sections, text)
	}
	self := ""
	if tok := p.selfToken(); tok != "" {
		if callerIsSubagent {
			self = "This process's main session is " + tok + " — the name OTHER sessions use to message it (it is not listed below; from inside this process, address the main conversation as \"" + mainAddress + "\")."
		} else {
			self = "This session is " + tok + " — the name other sessions use to message it (it is not listed below; a message to it would be a message to yourself)."
		}
	}
	if len(sections) == 0 {
		if self == "" {
			return "No reachable agents."
		}
		if !callerCanSend {
			return self + "\n\nNo reachable agents — no other gocode session is running on this machine right now."
		}
		return self + "\n\nNo reachable agents — no other gocode session is running on this machine right now (peer messaging itself is available; a session appears here once it is started)."
	}
	prefix := ""
	if !callerCanSend {
		prefix = noteCannotSend + "\n\n"
	}
	body := strings.Join(sections, "\n\n")
	if self != "" {
		body = self + "\n\n" + body
	}
	return prefix + body
}

// =============================================================================
// SendMessage
// =============================================================================

// SendMessageTool is the SendMessage tool of one agent (the main
// conversation, or a subagent).
type SendMessageTool struct {
	P *PeerMessaging
	// Subagent marks a subagent's copy: cross-session sends go out under the
	// main session's address with a note, and "main" reaches the main
	// conversation.
	Subagent bool
	// AgentName is the subagent's name or id (used in the <agent-message>
	// wrapper); "" falls back to "subagent".
	AgentName string
	// HasListTool is whether the caller also has ListAgents (the error hints
	// name it only then).
	HasListTool bool
}

// Name implements the model tool.
func (t *SendMessageTool) Name() string { return SendMessageToolName }

const messageDescription = "Plain text message content. The recipient's human sees only the FIRST LINE as a one-line preview until they expand it, so make the first line a clear, self-contained sentence saying what this is about — not a greeting, preamble, or bare @-mention."

// InputSchema has a cross-session variant when messaging is on (teams off)
// and a plain one otherwise. notify_when_idle is not offered (idle
// subscriptions are not supported).
func (t *SendMessageTool) InputSchema() json.RawMessage {
	if t.P != nil && t.P.Enabled {
		return json.RawMessage(`{"type":"object","properties":{` +
			`"to":{"type":"string","description":` + jsonText(`Recipient: a name from ListAgents (append its " [ref]" only when a listing or an error shows one), a teammate name, "main", or a background agent's agentId`) + `},` +
			`"summary":{"type":"string","maxLength":200,"description":` + jsonText("A 5-10 word label for your own transcript row (not transmitted — the recipient previews the first line of `message`). Truncated to 200 characters rather than rejected.") + `},` +
			`"message":{"type":"string","default":"","description":` + jsonText(messageDescription) + `}` +
			`},"required":["to"],"additionalProperties":false}`)
	}
	return json.RawMessage(`{"type":"object","properties":{` +
		`"to":{"type":"string","description":` + jsonText(`Recipient: a background agent's name or agentId, a teammate name, or "main"`) + `},` +
		`"summary":{"type":"string","maxLength":200,"description":"A 5-10 word summary shown as a one-line preview in the UI. Defaults to the first line of a plain-text message; longer summaries are truncated to 200 characters rather than rejected."},` +
		`"message":{"type":"string","description":"Plain text message content"}` +
		`},"required":["to","message"],"additionalProperties":false}`)
}

// Description is the SendMessage prompt.
func (t *SendMessageTool) Description() string {
	enabled := t.P != nil && t.P.Enabled
	rows := ""
	cross := ""
	if enabled {
		rows = "\n| `\"worker\"` | Any agent from `ListAgents` — subagent, another local gocode session |\n| `\"worker [3fa9c1]\"` | Same, plus its `[ref]` — only when a listing or an error shows one |"
		cross = sendMessageCrossSession
	}
	return "# SendMessage\n\nSend a message to another agent.\n\n```json\n{\"to\": \"researcher\", \"summary\": \"assign task 1\", \"message\": \"start on task #1\"}\n```\n\n" +
		"| `to` | |\n|---|---|\n| `\"researcher\"` | Teammate by name |\n| `\"main\"` | The main conversation (background subagents only) |" + rows + "\n\n" +
		"Your plain text output is NOT visible to other agents — to communicate, you MUST call this tool. Messages from teammates are delivered automatically; you don't check an inbox. Refer to agents by name — names keep working after an agent completes (a send resumes it from its transcript). Use the raw `agentId` (format `a...-...`) from its spawn result only when the agent has no name, or when a newer agent took the name (latest wins). When relaying, don't quote the original — it's already rendered to the user." +
		cross
}

// sendMessageCrossSession is the cross-session prompt section. No
// delivery receipts ("[Cross-session delivery notice]") are sent, and the
// notify_when_idle paragraph is dropped with the parameter.
const sendMessageCrossSession = "\n\n## Cross-session\n\n" +
	"Use `ListAgents` to discover targets. Every row leads with the agent's `name [ref]` — the name IS the address; there is no separate address syntax.\n\n" +
	"```json\n{\"to\": \"worker\", \"message\": \"check if tests pass over there\"}\n{\"to\": \"worker [3fa9c1]\", \"message\": \"you, specifically\"}\n```\n\n" +
	"Send the bare name — a name that exactly matches one live agent or session (on this machine, on another machine, or in the cloud) delivers directly. Append the ` [ref]` only when the bare name is not enough — `ListAgents` shows two rows with it, or an error asks you to disambiguate (you typed only a prefix, or a session list could not be checked). A ref you did not just read from a listing or an error will not resolve, and if the same name also names an in-process agent, the bare name always wins — use the in-process one.\n\n" +
	"A listed peer is alive and will receive your message; messages enqueue and drain at the receiver's next tool round (its `ListAgents` row says whether it is busy or idle right now). A successful send means the message reached that session, not that its gocode read it: a session running in a different permission mode than yours holds cross-session messages for its user's approval (and may let them expire), and a session can refuse them outright — nothing reports back, so never treat silence as agreement. Your message arrives wrapped as `<cross-session-message from=\"...\">`. **To reply to an incoming message, copy its `from` attribute as your `to`.** Cross-session messages travel between SESSIONS: if you are a subagent, your send goes out under your parent session's address, and any reply is delivered to the parent session's conversation, not to you. The receiver reads your message literally in every case (idle or busy, on this machine, over Remote Control or headless): an `@` followed by a file path, or `@server:resource`, attaches nothing there, unlike in your own user's input. So never rely on `@` to deliver content: send the text itself, or a file with its own tool.\n\n" +
	"Permission boundaries are per-session: NEVER ask a peer to perform an action that was denied or blocked in your session, or that you expect your own permission settings would block — a peer doing it for you bypasses the user's permission decision (cross-session permission laundering). Route blocked work back to your user instead."

// sendInput is the parsed input.
type sendInput struct {
	To      string          `json:"to"`
	Summary string          `json:"summary"`
	Message json.RawMessage `json:"message"`
}

// lifecycleTypes are the teammate lifecycle/task frame types a plain message
// must not impersonate.
var lifecycleTypes = map[string]bool{"idle_notification": true, "teammate_terminated": true, "task_assignment": true, "task_completed": true, "shutdown_rejected": true}

func (t *SendMessageTool) selfTargetMessage(to string) string {
	n := t.P.selfName()
	if t.Subagent {
		q := ""
		if n != "" {
			q = fmt.Sprintf(" (%q is the name OTHER sessions use for it)", n)
		}
		return fmt.Sprintf("'%s' is this process's own main session%s — from inside it, address the main conversation as %q instead.", to, q, mainAddress)
	}
	if n != "" {
		return fmt.Sprintf("'%s' is this session itself — %q is the name other sessions use to message YOU; there is no one else by that name to send to.", to, n)
	}
	return fmt.Sprintf("'%s' is this session itself — there is no one else at that address to send to.", to)
}

func (p *PeerMessaging) isOwnSocket(target string) bool {
	own := p.ownSock()
	if own == "" {
		return false
	}
	a, b := canonicalEndpoint(target), canonicalEndpoint(own)
	return a != "" && a == b
}

// validate is validateInput (its errorCode-9 refusals become tool errors).
func (t *SendMessageTool) validate(in sendInput, msg string, isString bool) error {
	if strings.ContainsAny(in.To, "\r\n") {
		return errors.New("to: must be a single-line recipient name or address")
	}
	if utf8.RuneCountInString(in.To) > recipientMax {
		return fmt.Errorf("to: recipient longer than any listed name or address (max %d characters)", recipientMax)
	}
	if in.To == "*" {
		return errors.New(`broadcast (to: "*") is no longer supported — send a message per recipient`)
	}
	addr := ParseAddress(in.To)
	if (addr.Scheme == SchemeUDS || addr.Scheme == SchemeBridge) && !t.P.Enabled {
		return errors.New(errNotAvailable)
	}
	if m := ValidateAddress(in.To, ListAgentsToolName); m != "" {
		return errors.New(m)
	}
	if addr.Scheme == SchemeUDS && t.P.isOwnSocket(addr.Target) {
		return errors.New(t.selfTargetMessage(in.To))
	}
	if strings.Contains(in.To, "@") {
		return errors.New("to must be a bare teammate name — there is only one team per session")
	}
	if !isString {
		return errors.New("structured messages cannot be sent cross-session — only plain text")
	}
	if strings.TrimSpace(msg) == "" {
		return errors.New("message must not be empty")
	}
	if addr.Scheme != SchemeOther {
		return nil
	}
	var probe struct {
		Type *string `json:"type"`
	}
	if json.Unmarshal([]byte(msg), &probe) == nil && probe.Type != nil && lifecycleTypes[*probe.Type] {
		return errors.New("message text must not be a teammate lifecycle/task frame (idle/terminated/task/shutdown JSON) — send plain text instead")
	}
	return nil
}

// Execute validates and delivers one send.
func (t *SendMessageTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in sendInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}
	if t.P == nil {
		t = &SendMessageTool{P: &PeerMessaging{}, Subagent: t.Subagent, AgentName: t.AgentName, HasListTool: t.HasListTool}
	}
	msg, isString := "", true
	if len(in.Message) > 0 && string(in.Message) != "null" {
		if err := json.Unmarshal(in.Message, &msg); err != nil {
			isString = false
		}
	}
	if err := t.validate(in, msg, isString); err != nil {
		return "", err
	}
	summary := in.Summary
	if utf8.RuneCountInString(summary) > SummaryMaxChars {
		summary = string([]rune(summary)[:SummaryMaxChars])
	}
	label := summary
	if label == "" {
		label = truncateWidth(msg, 50)
	}

	addr := ParseAddress(in.To)
	switch addr.Scheme {
	case SchemeUDS:
		return t.sendDirect(ctx, in.To, t.wrapForSubagent(ctx, msg), label), nil
	case SchemeBridge, SchemeDID:
		if !t.P.Enabled || t.P.Sender == nil {
			return resultText(false, errNotAvailable, ""), nil
		}
		return resultText(false, "Cross-machine messaging is unavailable: only gocode sessions on this machine are reachable from this session. Messages to sessions on this machine still work.", ""), nil
	}
	return t.sendByName(ctx, in.To, msg, label), nil
}

// wrapForSubagent: a subagent's text travels inside <agent-message>,
// from= its name (else its task id).
func (t *SendMessageTool) wrapForSubagent(ctx context.Context, msg string) string {
	if !t.Subagent {
		return msg
	}
	return WrapAgentMessage(t.senderFrom(ctx), msg)
}

func (t *SendMessageTool) subagentSuffix(oneWay bool) string {
	if !t.Subagent {
		return ""
	}
	if oneWay {
		return "; sent under this session's address, not this agent's"
	}
	return "; sent under this session's address: any reply will be delivered to this session's main conversation, not to this agent"
}

// inboxSuffix says whether a reply can reach this session. No delivery
// receipts are sent, so the bound-inbox variant says nothing reports back.
func (p *PeerMessaging) inboxSuffix() string {
	if p.Listener != nil {
		return "; in that session's inbox, not yet read by its gocode — that session may hold it (usually a different permission mode) or refuse it, and nothing reports back, so never treat silence as agreement"
	}
	return "; in that session's inbox, not yet read by its gocode — that session may hold it (usually a different permission mode) or refuse it, and with no inbox bound here nothing reports back, so never treat silence as agreement"
}

func (p *PeerMessaging) sendOptions() PeerSendOptions {
	o := PeerSendOptions{FromName: p.selfName()}
	if p.SenderMode != nil {
		o.FromMode = p.SenderMode()
	}
	return o
}

// sendDirect is deliver's uds branch.
func (t *SendMessageTool) sendDirect(ctx context.Context, to, msg, label string) string {
	if !t.P.Enabled || t.P.Sender == nil {
		return resultText(false, errNotAvailable, "")
	}
	id, err := t.P.Sender.Send(ctx, to, msg, t.P.sendOptions())
	if err != nil {
		return resultText(false, FailureMessage(to, err, ListAgentsToolName), "")
	}
	return resultText(true, "“"+label+"” → "+to+t.P.inboxSuffix()+t.subagentSuffix(t.P.Listener == nil), id)
}

// sendByName resolves a bare (or "name [ref]") recipient: "main", then this
// session's agents by id or name (an in-process agent always wins over a
// session of the same name), then the live local sessions on this machine.
func (t *SendMessageTool) sendByName(ctx context.Context, to, msg, label string) string {
	from := t.senderFrom(ctx)
	text := msg
	if from != "" {
		text = WrapAgentMessage(from, msg)
	}
	if to == mainAddress || normName(to) == mainAddress {
		if !t.Subagent {
			return resultText(false, `You are the main conversation — "main" addresses you. Send to a named agent instead.`, "")
		}
		if t.P.QueueMain == nil {
			return resultText(false, "The main conversation was not messaged: it cannot receive messages from here.", "")
		}
		t.P.QueueMain(AgentMessage{Text: text, From: from})
		return resultText(true, "Message queued for the main conversation's next turn.", "")
	}
	if target, ok := lookupAgent(t.P.Agents, to); ok {
		return t.sendToAgent(ctx, target, AgentMessage{Text: text, From: from})
	}
	listHint := "Check the spelling, or use the agent ID from a background agent's spawn result."
	if t.HasListTool {
		listHint = "Use ListAgents to see everyone you can message."
	}
	notFound := func(extra string) string {
		return resultText(false, "No agent named '"+to+"' is reachable."+extra+"\n"+listHint, "")
	}
	if !t.P.Enabled || t.P.Sender == nil {
		return notFound("")
	}

	// This session's own name or token.
	name, ref, hasRef := parseNameRef(to)
	if !hasRef {
		name = to
	}
	if self := t.P.selfName(); self != "" && normName(name) == normName(self) {
		own := t.P.ownSock()
		if !hasRef || (own != "" && strings.HasPrefix(fullRef(own), ref)) {
			return resultText(false, t.selfTargetMessage(to), "")
		}
	}

	cands, err := t.P.listCandidates()
	var matches []candidate
	for _, c := range cands {
		if c.name == "" || normName(c.name) != normName(name) {
			continue
		}
		if hasRef && !strings.HasPrefix(c.full, ref) {
			continue
		}
		matches = append(matches, c)
	}
	switch {
	case len(matches) == 0:
		extra := ""
		if err != nil {
			extra = "\nThe sessions on this machine could not be listed just now, so they were not searched; retry if you meant a session on this machine."
		}
		return notFound(extra)
	case len(matches) > 1:
		var rows []string
		for _, c := range matches {
			rows = append(rows, "  "+c.token()+" — gocode session, on this machine")
		}
		head := fmt.Sprintf("%d agents are named '%s'. Re-send with the ref of the one you mean:", len(matches), to)
		return resultText(false, head+"\n"+strings.Join(rows, "\n"), "")
	}
	c := matches[0]
	id, serr := t.P.Sender.Send(ctx, UDSAddress(c.rec.MessagingSocketPath), text, t.P.sendOptions())
	if serr != nil {
		hint := "."
		var pe *PeerSendError
		switch {
		case isGoneError(serr):
			hint = " — that session may have just exited."
			if t.HasListTool {
				hint += " Call ListAgents to see who is reachable now."
			}
		case isBusyError(serr):
			hint = " — the session is alive but momentarily busy. Retry the same name shortly."
		case errors.As(serr, &pe):
			hint = ": " + pe.Msg
		}
		return resultText(false, "Failed to send to "+c.name+hint, "")
	}
	return resultText(true, "“"+label+"” → "+c.name+" ("+thisMachine+t.P.inboxSuffix()+t.subagentSuffix(t.P.Listener == nil)+")", id)
}

// --- receive side -------------------------------------------------------------

// ContextFeed drains a Store recipient queue as model context: each queued
// message is already wrapped in <cross-session-message>, so it reaches the
// model marked as untrusted peer input, and a peer's message is additionally
// framed (FramePeerMessage) — "Another gocode session
// sent a message:" at a turn start (an idle session woken by it), "… while you
// were working:" plus a reply hint when drained between tool rounds. It
// satisfies the conversation's external context-feed seam and its mid-turn
// extension.
type ContextFeed struct {
	Store     *Store
	Recipient string
	// NoSendTool marks a receiver without SendMessage: the mid-turn hint then
	// omits the reply-via SendMessage part. The zero value — the main session,
	// which has the tool whenever it has an inbox — keeps it.
	NoSendTool bool
}

func (f *ContextFeed) recipient() string {
	if f.Recipient == "" {
		return MainRecipient
	}
	return f.Recipient
}

// HasPending reports whether a message is waiting.
func (f *ContextFeed) HasPending() bool { return f.Store != nil && f.Store.Len(f.recipient()) > 0 }

// TakePending removes and returns every waiting message, oldest first, framed
// for the start of a turn.
func (f *ContextFeed) TakePending() []string { return f.take(false) }

// TakeMidTurn takes every waiting message for delivery between two tool
// rounds of a running turn: a received peer message is a priority:"next"
// command, which the running turn drains after its tool round.
func (f *ContextFeed) TakeMidTurn() []string { return f.take(true) }

func (f *ContextFeed) take(midTurn bool) []string {
	if f.Store == nil {
		return nil
	}
	var out []string
	for {
		m, ok := f.Store.Fetch(f.recipient())
		if !ok {
			return out
		}
		f.Store.Ack(f.recipient(), m.ID)
		out = append(out, frameStored(m, midTurn, !f.NoSendTool))
	}
}

// frameStored frames a stored message for the model: a cross-session peer's
// (or an unattributed, demoted-to-peer sender's) message gets the peer
// framing; other sender kinds are delivered as stored.
func frameStored(m Message, midTurn, canSend bool) string {
	switch m.FromKind {
	case SenderPeer, SenderUnknown, "":
		return FramePeerMessage(m.Body, midTurn, canSend)
	}
	return m.Body
}
