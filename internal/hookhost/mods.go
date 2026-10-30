package hookhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/hooks"
	"github.com/umairge66/gocode/internal/mods"
)

// ModEvents is the seam of the plugin hooks-module engine (internal/mods) for
// the session lifecycle the host drives. Each method raises its event through
// the loaded modules' hooks; a module that fails is passed over by the engine,
// so none of these can break the session.
type ModEvents interface {
	// SessionStart raises session.start { cwd, surface, isInteractive }.
	SessionStart(ctx context.Context, cwd string, interactive bool)
	// PromptSubmit raises prompt.submit for a typed prompt, classic (the
	// UserPromptSubmit hooks) beneath its hooks on the prompt as they passed
	// it. A non-empty drop refuses the prompt; final is the prompt as it
	// stands (a hook may rewrite it); extra is added to what the model reads.
	PromptSubmit(ctx context.Context, text string, classic func(ctx context.Context, text string) (blocked string)) (final, drop string, extra []string)
	// CommandRun raises command.run around a slash command; run is the
	// command's own run with the arguments as the hooks passed them.
	CommandRun(ctx context.Context, command, args string, presentation map[string]any, run func(ctx context.Context, args string) (*string, error)) (mods.CommandOutcome, error)
	// TurnStart raises turn.start { text, turnId }.
	TurnStart(ctx context.Context, text, turnID string)
	// TurnComplete raises turn.complete { answer, durationMs, isAborted,
	// turnId, reason }.
	TurnComplete(ctx context.Context, answer, turnID string, took time.Duration, aborted bool)
}

// modsSessionStart raises session.start before the classic SessionStart hooks.
func (h *Host) modsSessionStart(ctx context.Context) {
	if h.mods == nil {
		return
	}
	h.mods.SessionStart(ctx, h.Session().Cwd, h.attended)
}

// userPromptSubmit runs the UserPromptSubmit hooks over prompt: under the
// hooks modules' prompt.submit when any is loaded, the classic hooks
// beneath them on the prompt as they passed it (prompt.submit's core
// runs the classic hooks over the arrived text, so a rewrite is screened
// again). A drop blocks the prompt as a blocking UserPromptSubmit hook
// does; the modules' context rides along. It answers the prompt as it
// stands.
func (h *Host) userPromptSubmit(ctx context.Context, prompt string) (*Result, string) {
	runClassic := func(ctx context.Context, text string) *Result {
		p := hooks.Payload{HookEventName: hooks.EventUserPromptSubmit, Prompt: text}
		return h.Run(ctx, p, "", false)
	}
	if h.mods == nil {
		return runClassic(ctx, prompt), prompt
	}
	var mu sync.Mutex
	var classic *Result
	final, drop, extra := h.mods.PromptSubmit(ctx, prompt, func(ctx context.Context, text string) string {
		r := runClassic(ctx, text)
		mu.Lock()
		classic = r
		mu.Unlock()
		if r.Blocked() || (r != nil && r.Ran > 0 && !r.Continue) {
			reason := strings.TrimSpace(r.Reason)
			if reason == "" {
				reason = strings.TrimSpace(r.StopReason)
			}
			if reason == "" {
				reason = "blocked by a UserPromptSubmit hook"
			}
			return reason
		}
		return ""
	})
	mu.Lock()
	defer mu.Unlock()
	if drop != "" {
		if classic != nil && (classic.Blocked() || (classic.Ran > 0 && !classic.Continue)) {
			// The classic block beneath stands as it was given.
			return classic, prompt
		}
		r := &Result{Ran: 1, Decision: hooks.Decision{Behavior: hooks.BehaviorDeny, Reason: drop}}
		r.Continue = true
		return r, prompt
	}
	return withModsContext(classic, extra), final
}

// CommandRun raises the hooks modules' command.run around a slash command
// (run directly when no module is loaded). run is the command's own run
// with the arguments as the hooks passed them; it answers its text, nil
// when it has none.
func (h *Host) CommandRun(ctx context.Context, command, args string, presentation map[string]any, run func(ctx context.Context, args string) (*string, error)) (mods.CommandOutcome, error) {
	if h == nil || h.mods == nil {
		t, err := run(ctx, args)
		out := mods.CommandOutcome{Ran: true, RanArgs: args}
		if t != nil {
			out.Text, out.HasText = *t, true
		}
		return out, err
	}
	return h.mods.CommandRun(ctx, command, args, presentation, run)
}

// withModsContext adds the modules' prompt.submit context to r's.
func withModsContext(r *Result, extra []string) *Result {
	if len(extra) == 0 {
		return r
	}
	if r == nil {
		r = &Result{}
		r.Continue = true
	}
	parts := append([]string{}, extra...)
	if r.AdditionalContext != "" {
		parts = append([]string{r.AdditionalContext}, parts...)
	}
	r.AdditionalContext = strings.Join(parts, "\n")
	return r
}

// modsTurn is one turn as the hooks modules saw it start.
type modsTurn struct {
	id      string
	started time.Time
}

// modsTurnStart raises turn.start with the turn's prompt text.
func (h *Host) modsTurnStart(ctx context.Context, initial []agent.Message) modsTurn {
	if h.mods == nil {
		return modsTurn{}
	}
	t := modsTurn{id: newTurnID(), started: time.Now()}
	h.mods.TurnStart(ctx, lastUserText(initial), t.id)
	return t
}

// modsTurnComplete raises turn.complete with the loop's last answer.
func (h *Host) modsTurnComplete(ctx context.Context, t modsTurn, res agent.Result) {
	if h.mods == nil || t.id == "" {
		return
	}
	aborted := ctx.Err() != nil || res.Reason == agent.ReasonAborted
	h.mods.TurnComplete(context.WithoutCancel(ctx), lastAssistantText(res.Messages), t.id, time.Since(t.started), aborted)
}

func newTurnID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func lastUserText(msgs []agent.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != agent.RoleUser {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if b.Type == agent.BlockText && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
