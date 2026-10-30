package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/inbox"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// Local cross-session messaging for a session: the inbox listener (other
// gocode sessions of this OS user deliver to it), the session-registry record
// that makes this session discoverable, and the SendMessage / ListAgents
// tools. Everything is local IPC; nothing is sent to any server.
//
// Lifetime: the inbox is bound at session
// start unless the session is bare (--bare / GOCODE_CODE_SIMPLE) or messaging
// is switched off (GOCODE_CODE_HARBOR_KITE=0); it is torn down at exit
// together with the registry record.
//
// Receiving (behind the crossSessionInbound gate): an admitted message waits
// in the inbox (at most 50) framed as <cross-session-message from="..."> —
// untrusted peer input — and rides the next model turn; an idle interactive
// session is woken to take it at once (as a priority "next" prompt).
//
// In-process: a
// subagent's message to "main" is queued for the main conversation and, like
// a peer message, delivered between two tool rounds of a running turn or at
// the next turn (waking an idle session); SendMessage reaches this session's
// own subagents by name or task id through the Agent tool's directory.

// peerMessaging is the session's messaging state.
type peerMessaging struct {
	pm         *inbox.PeerMessaging
	store      *inbox.Store
	mainQueue  *inbox.AgentQueue
	listener   *inbox.Listener
	unregister func()
}

// sessionKind returns GOCODE_CODE_SESSION_KIND when it is "bg" or "daemon",
// else "interactive".
func sessionKind(getenv func(string) string) string {
	switch k := getenv("GOCODE_CODE_SESSION_KIND"); k {
	case "bg", "daemon":
		return k
	}
	return "interactive"
}

// inboundSettingLayers reads crossSessionInbound from each settings source.
func inboundSettingLayers(env *Env) []inbox.SettingLayer {
	ordered := settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder)
	var layers []inbox.SettingLayer
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if v, ok := or.Result.Settings["crossSessionInbound"]; ok {
			layers = append(layers, inbox.SettingLayer{Source: string(or.Source), Value: v})
		}
	}
	return layers
}

// wirePeerMessaging binds the inbox, registers the session and installs the
// messaging tools on the main dispatcher. Called before the Agent tool is
// wired, so SendMessage is part of the roster subagents are filtered from.
func (r *sessionRuntime) wirePeerMessaging(env *Env, paths sessionPaths, sessionID string, cfg sessionConfig) {
	getenv := envGetenv(env)
	enabled := inbox.MessagingEnabled(getenv)
	pm := &inbox.PeerMessaging{Enabled: enabled}
	p := &peerMessaging{pm: pm, store: inbox.NewStore(), mainQueue: &inbox.AgentQueue{}}
	r.peer = p
	// The Agent tool is wired after this; resolve it at send time.
	pm.Agents = sessionAgents{r}
	if r.conv != nil {
		r.conv.AddContextFeed(&inbox.AgentFeed{Queue: p.mainQueue, CanSend: true})
	}

	modeClass := func() string {
		if r.dispatcher == nil {
			return ""
		}
		return inbox.ModeClass(string(r.dispatcher.CurrentMode()), false)
	}
	pm.SenderMode = modeClass
	pm.QueueMain = func(m inbox.AgentMessage) {
		p.mainQueue.Push(m)
		r.triggerResume()
	}

	name := ""
	nameSource := ""
	if n := strings.TrimSpace(getenv("GOCODE_CODE_SESSION_NAME")); n != "" {
		name, nameSource = n, "user"
	} else {
		name, nameSource = inbox.DerivedSessionName(paths.cwd), "derived"
	}

	bare := envutil.EnvTruthy(getenv("GOCODE_CODE_SIMPLE"))
	if enabled && !bare {
		l, err := inbox.Listen(inbox.ListenerConfig{
			Store:     p.store,
			SessionID: sessionID,
			Gate: func(from string, envl inbox.Envelope) inbox.InboundDecision {
				setting := inbox.ResolveInboundSetting(inboundSettingLayers(env))
				return inbox.DecideInbound(setting, modeClass(), envl.FromMode, inbox.MessagingEnabled(getenv))
			},
			OnQueued: func(inbox.Message) { r.triggerResume() },
			OnHeld: func(h inbox.HeldMessage, held int) {
				r.emit(inbox.HeldNotice(h, held))
				if cfg.interactive && h.Cause.IsParityHold() {
					go r.reviewHeld(h)
				}
			},
			OnReleased: func(rel []inbox.HeldMessage, reason string) {
				r.emit(inbox.ReleasedNotice(len(rel), reason))
			},
		})
		if err != nil {
			r.notices = append(r.notices, "Cross-session messaging inbox not started: "+err.Error())
		} else {
			p.listener = l
			pm.Listener = l
			if r.conv != nil {
				r.conv.AddContextFeed(&inbox.ContextFeed{Store: p.store})
			}
			if paths.configDir != "" {
				pm.RegistryDir = inbox.SessionsDir(paths.configDir)
				rec := inbox.SessionRecord{
					PID:                 os.Getpid(),
					SessionID:           sessionID,
					Cwd:                 paths.cwd,
					Version:             Version,
					Kind:                sessionKind(getenv),
					Entrypoint:          getenv("GOCODE_CODE_ENTRYPOINT"),
					MessagingSocketPath: l.Path(),
					Name:                name,
					NameSource:          nameSource,
				}
				if unreg, err := inbox.RegisterSession(pm.RegistryDir, rec); err == nil {
					p.unregister = unreg
				}
			}
		}
	} else if enabled && paths.configDir != "" {
		// A bare session has no inbox but can still discover and message others.
		pm.RegistryDir = inbox.SessionsDir(paths.configDir)
	}
	if enabled {
		pm.Sender = inbox.NewPeerSender(p.listener)
		pm.SelfName = func() string {
			if p.listener == nil {
				return ""
			}
			return name
		}
	}

	send := &inbox.SendMessageTool{P: pm, HasListTool: enabled}
	var list toolrun.ModelTool
	if enabled {
		list = &inbox.ListAgentsTool{P: pm, CallerCanSend: true}
	}
	r.dispatcher.SetPeerTools(send, list)

	// Held messages are re-checked when the permission mode changes.
	if r.conv != nil && p.listener != nil {
		prev := r.conv.OnModeChange
		r.conv.OnModeChange = func(m toolreg.Mode) {
			if prev != nil {
				prev(m)
			}
			p.listener.Reevaluate("mode-changed")
		}
	}
}

// sessionAgents is the session's Agent-tool directory, resolved when a
// message is sent (the Agent tool is wired after the messaging tools).
type sessionAgents struct{ r *sessionRuntime }

func (a sessionAgents) dir() inbox.AgentDirectory {
	if a.r.agentTool == nil {
		return nil
	}
	return a.r.agentTool.MessageDirectory()
}

func (a sessionAgents) LookupAgent(key string) (inbox.AgentTarget, bool) {
	if d := a.dir(); d != nil {
		return d.LookupAgent(key)
	}
	return inbox.AgentTarget{}, false
}

func (a sessionAgents) AgentNames() []string {
	if d := a.dir(); d != nil {
		return d.AgentNames()
	}
	return nil
}

func (a sessionAgents) QueueAgentMessage(id string, m inbox.AgentMessage) (bool, bool) {
	if d := a.dir(); d != nil {
		return d.QueueAgentMessage(id, m)
	}
	return false, false
}

func (a sessionAgents) ResumeAgent(ctx context.Context, id string, m inbox.AgentMessage) error {
	if d := a.dir(); d != nil {
		return d.ResumeAgent(ctx, id, m)
	}
	return inbox.ErrNoTranscript
}

// subagentPeerTools is the subagent's copy of SendMessage (subagents get no
// ListAgents: the async-agent tool set has SendMessage only).
func (r *sessionRuntime) subagentPeerTools() toolrun.Option {
	if r.peer == nil {
		return func(*toolrun.Dispatcher) {}
	}
	return toolrun.WithPeerTools(&inbox.SendMessageTool{P: r.peer.pm, Subagent: true}, nil)
}

// reviewHeld offers the user an approve/deny review of a mode-parity hold
// through the session's permission dialog. A dismissed dialog denies.
func (r *sessionRuntime) reviewHeld(h inbox.HeldMessage) {
	if r.dispatcher == nil || r.dispatcher.Prompter == nil || r.peer == nil || r.peer.listener == nil {
		return
	}
	from := h.From
	if !inbox.IsPeerAddress(from) {
		from = "an unidentified session"
	}
	input, _ := json.Marshal(map[string]string{"from": from, "claimed_name": h.FromName, "message": h.Body})
	resp, err := r.dispatcher.Prompter.Prompt(context.Background(), toolrun.PermissionRequest{
		Tool:     protocol.ToolSendMessage,
		ToolName: string(protocol.ToolSendMessage),
		Input:    input,
		Reason:   "Deliver a held cross-session message from " + from + " to gocode? " + h.Cause.Describe(),
		Origin:   "cross-session message",
	})
	approve := err == nil && resp.Kind != toolrun.PermissionDenied
	switch r.peer.listener.Resolve(h.ID, approve) {
	case "dropped":
		if approve {
			r.emit("That held message was NOT delivered: cross-session messaging was turned off (or set to refuse) while the prompt was open, so your approval could not apply — it was dropped.")
		}
	case "dropped-by-guard":
		r.emit("That held message was NOT delivered: this session's inbox guard (per-sender rate limit or queue capacity) had no room for it when you approved, so it was dropped. A sending gocode session on this machine is normally told it was dropped; if the message mattered, ask the sender to send it again later.")
	case "gone":
		w := "denial"
		if approve {
			w = "approval"
		}
		r.emit("That held message was already resolved before your " + w + " (released or dropped by a mode/setting change) — no action taken.")
	}
}

// closePeer tears down the inbox and removes the registry record.
func (r *sessionRuntime) closePeer() {
	if r.peer == nil {
		return
	}
	if r.peer.listener != nil {
		_ = r.peer.listener.Close()
	}
	if r.peer.unregister != nil {
		r.peer.unregister()
	}
}
