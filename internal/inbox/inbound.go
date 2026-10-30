package inbox

// The receive-side gate for cross-session peer messages: the
// "crossSessionInbound" setting (accept / hold / refuse) and, when it is
// unset, permission-mode parity (a message auto-delivers only when the
// sending session's permission-mode class matches this one's; otherwise it is
// held for the user's review).

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/envutil"
)

// Inbound policies (the crossSessionInbound values).
const (
	InboundAccept = "accept"
	InboundHold   = "hold"
	InboundRefuse = "refuse"
)

// Mode classes carried in from-mode=.
const (
	ModeClassBypass    = "bypass"
	ModeClassPrompting = "prompting"
)

// HoldCause is why a message was parked.
type HoldCause string

const (
	HoldExplicitSetting HoldCause = "explicit-setting"
	HoldManagedSetting  HoldCause = "managed-setting"
	HoldRepoSetting     HoldCause = "repo-setting"
	HoldInvalidSetting  HoldCause = "invalid-setting"
	HoldModeUnknown     HoldCause = "mode-unknown"
	HoldModeMismatch    HoldCause = "mode-mismatch"
	HoldNoModeAsserted  HoldCause = "no-mode-asserted"
)

// Describe returns the user-facing explanation of a hold.
func (c HoldCause) Describe() string {
	switch c {
	case HoldExplicitSetting:
		return `Your "crossSessionInbound" setting is "hold"; set it to "accept" to deliver held messages.`
	case HoldManagedSetting:
		return `Your organization's managed settings set "crossSessionInbound" to "hold" (your own "accept" cannot override managed policy); ask your admin to change it.`
	case HoldRepoSetting:
		return `This repository's settings set "crossSessionInbound" to "hold" (a repo may only tighten, so your own "accept" cannot override it); remove the repo setting or exclude it with --setting-sources.`
	case HoldInvalidSetting:
		return `A settings file has an unrecognized "crossSessionInbound" value (the settings warning names the file); messages are held while it is present — set it to "accept", "hold", or "refuse".`
	case HoldModeUnknown:
		return "It will be delivered once the session finishes starting up."
	case HoldModeMismatch:
		return `The sending session's permission mode class doesn't match this session's. Review it below, or set "crossSessionInbound" to "accept".`
	case HoldNoModeAsserted:
		return `The sender did not attest its permission mode and this session bypasses prompts. Review it below, or set "crossSessionInbound" to "accept".`
	}
	return ""
}

// IsParityHold reports whether the hold is a permission-mode parity hold —
// the only causes the interactive host offers an approve/deny review for.
func (c HoldCause) IsParityHold() bool { return c == HoldModeMismatch || c == HoldNoModeAsserted }

// InboundDecision is the gate's verdict for one message.
type InboundDecision struct {
	Policy      string    // InboundAccept / InboundHold / InboundRefuse
	HoldCause   HoldCause // set for InboundHold
	RefuseCause string    // "opt-out" or "kill-switch" for InboundRefuse
}

// inboundRank orders accept < hold < refuse.
var inboundRank = map[string]int{InboundAccept: 0, InboundHold: 1, InboundRefuse: 2}

// SettingLayer is one settings source's crossSessionInbound value.
type SettingLayer struct {
	// Source is the settings source id ("policySettings", "flagSettings",
	// "userSettings", "localSettings", "projectSettings"; "managedSettings"
	// counts as policy).
	Source string
	// Value is the raw setting value (nil when the source does not set it).
	Value any
}

// InboundSetting is the resolved setting.
type InboundSetting struct {
	// Value is "accept", "hold", "refuse", or "" when unset (mode parity).
	Value string
	// DecidedBy is the source class that decided it: "policySettings",
	// "flagSettings", "userSettings", "repoSettings", "invalidSetting", or "".
	DecidedBy string
}

// ResolveInboundSetting: the first of policy, flag and user settings
// that sets the value wins; local and project (repo) settings may only
// tighten it; an unrecognized value anywhere holds at least.
func ResolveInboundSetting(layers []SettingLayer) InboundSetting {
	get := func(src string) (string, bool, bool) {
		for _, l := range layers {
			if l.Source != src && !(src == "policySettings" && l.Source == "managedSettings") {
				continue
			}
			if l.Value == nil {
				continue
			}
			s, ok := l.Value.(string)
			if !ok || inboundRank[s] == 0 && s != InboundAccept {
				return "", true, true // present but invalid
			}
			return s, true, false
		}
		return "", false, false
	}
	var out InboundSetting
	invalid := false
	for _, src := range []string{"policySettings", "flagSettings", "userSettings"} {
		v, present, bad := get(src)
		if bad {
			invalid = true
			continue
		}
		if present {
			out = InboundSetting{Value: v, DecidedBy: src}
			break
		}
	}
	for _, src := range []string{"localSettings", "projectSettings"} {
		v, present, bad := get(src)
		if bad {
			invalid = true
			continue
		}
		if !present {
			continue
		}
		cur := out.Value
		if cur == "" {
			cur = InboundAccept
		}
		if inboundRank[v] > inboundRank[cur] {
			out = InboundSetting{Value: v, DecidedBy: "repoSettings"}
		} else if v != InboundAccept && out.Value != "" && inboundRank[v] == inboundRank[out.Value] && out.DecidedBy != "policySettings" {
			out.DecidedBy = "repoSettings"
		}
	}
	cur := out.Value
	if cur == "" {
		cur = InboundAccept
	}
	if invalid && inboundRank[cur] < inboundRank[InboundHold] {
		out = InboundSetting{Value: InboundHold, DecidedBy: "invalidSetting"}
	}
	return out
}

// holdCauseFor returns which setting-class cause a "hold" value carries.
func holdCauseFor(decidedBy string) HoldCause {
	switch decidedBy {
	case "policySettings":
		return HoldManagedSetting
	case "repoSettings":
		return HoldRepoSetting
	case "invalidSetting":
		return HoldInvalidSetting
	}
	return HoldExplicitSetting
}

// ModeClass is "bypass" for bypassPermissions (or plan mode when bypass
// is available), "prompting" otherwise; "" for an unrecognized mode.
func ModeClass(mode string, bypassAvailable bool) string {
	switch mode {
	case "bypassPermissions":
		return ModeClassBypass
	case "plan":
		if bypassAvailable {
			return ModeClassBypass
		}
		return ModeClassPrompting
	case "default", "acceptEdits", "auto", "dontAsk", "bubble":
		return ModeClassPrompting
	}
	return ""
}

// MessagingEnabled reports that cross-session messaging is on unless
// GOCODE_CODE_HARBOR_KITE is set to a falsy value (the default is on).
func MessagingEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return true
	}
	if v := getenv("GOCODE_CODE_HARBOR_KITE"); v != "" {
		return envutil.EnvTruthy(v)
	}
	return true
}

// DecideInbound returns the verdict for a message from a sender that asserted
// fromMode ("" when none) arriving at a session whose own mode class is
// receiverClass ("" when unknown). enabled=false is the kill switch.
func DecideInbound(setting InboundSetting, receiverClass, fromMode string, enabled bool) InboundDecision {
	if !enabled {
		return InboundDecision{Policy: InboundRefuse, RefuseCause: "kill-switch"}
	}
	switch setting.Value {
	case InboundAccept:
		return InboundDecision{Policy: InboundAccept}
	case InboundHold:
		return InboundDecision{Policy: InboundHold, HoldCause: holdCauseFor(setting.DecidedBy)}
	case InboundRefuse:
		return InboundDecision{Policy: InboundRefuse, RefuseCause: "opt-out"}
	}
	if receiverClass == "" {
		return InboundDecision{Policy: InboundHold, HoldCause: HoldModeUnknown}
	}
	if fromMode != "" {
		if fromMode == receiverClass {
			return InboundDecision{Policy: InboundAccept}
		}
		return InboundDecision{Policy: InboundHold, HoldCause: HoldModeMismatch}
	}
	if receiverClass == ModeClassBypass {
		return InboundDecision{Policy: InboundHold, HoldCause: HoldNoModeAsserted}
	}
	return InboundDecision{Policy: InboundAccept}
}

// HeldMessage is a peer message parked by the gate.
type HeldMessage struct {
	ID       string
	From     string // the sender's uds: address ("unknown" when unattributed)
	FromName string // the sender's claimed name (unverified)
	FromMode string
	Content  string // the framed <cross-session-message> text
	Body     string // the unframed body, for the preview
	Cause    HoldCause
	msgID    string
}

// maxHeld: the hold buffer keeps at most 100 messages (the oldest is
// evicted as expired).
const maxHeld = 100

// previewLineMax caps the one-line preview.
const previewLineMax = 120

// HeldNotice is the transcript line announcing a held message (the
// onPeerHeld handler).
func HeldNotice(h HeldMessage, heldCount int) string {
	addr := "an unidentified session"
	if IsPeerAddress(h.From) {
		addr = h.From
	}
	claimed := ""
	if n := cleanLabel(h.FromName); n != "" {
		claimed = " (peer claims name: " + n + ")"
	}
	preview, summary := previewOf(h.Body)
	p := ""
	if preview != "" || summary != "" {
		p = "; preview: «" + preview + "»" + summary
	}
	return "Held peer message — from " + addr + claimed + p + " — not delivered to gocode (" + strconv.Itoa(heldCount) + " held). " + h.Cause.Describe()
}

// ReleasedNotice is the transcript line for released messages.
func ReleasedNotice(n int, reason string) string {
	why := reason
	switch reason {
	case "mode-changed":
		why = "permissions are prompting again"
	case "policy-accepts":
		why = "crossSessionInbound now accepts"
	case "approved":
		why = "you approved it"
	}
	noun := "messages"
	if n == 1 {
		noun = "message"
	}
	return "Released " + strconv.Itoa(n) + " held cross-session " + noun + " to gocode's queue (" + why + ")."
}

// previewOf returns the one-line preview and its "…[N lines, M chars]" summary.
func previewOf(body string) (string, string) {
	if body == "" {
		return "", ""
	}
	lines := strings.Split(body, "\n")
	first := ""
	for _, l := range lines {
		if f := strings.Join(strings.Fields(l), " "); f != "" {
			first = f
			break
		}
	}
	shown := first
	if utf8.RuneCountInString(shown) > previewLineMax {
		shown = string([]rune(shown)[:previewLineMax-1]) + "…"
	}
	summary := ""
	if len(lines) > 1 || shown != first {
		summary = " …[" + strconv.Itoa(len(lines)) + " " + plural(len(lines), "line") + ", " + strconv.Itoa(utf8.RuneCountInString(body)) + " chars total — expand to review before approving]"
	}
	return shown, summary
}

func plural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}
