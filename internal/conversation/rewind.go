package conversation

import (
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
)

// RewindPoint is one user prompt the conversation can be rewound to: its
// index in History and its prompt text (the first text block).
type RewindPoint struct {
	Index int
	Text  string
}

// historyReplacer is the optional rewrite seam of a SessionRecorder
// (sessionstore.Session.ReplaceHistory).
type historyReplacer interface {
	ReplaceHistory(msgs []agent.Message) error
}

// RewindPoints lists the user prompts in history, oldest first: user messages
// carrying text and no tool results, excluding a compaction summary.
func RewindPoints(history []agent.Message) []RewindPoint {
	var out []RewindPoint
	for i, m := range history {
		if text, ok := PromptText(m); ok {
			if compaction.IsSummaryMessage(compaction.Message{Role: m.Role, Content: m.Content}) {
				continue
			}
			out = append(out, RewindPoint{Index: i, Text: text})
		}
	}
	return out
}

// PromptText returns a user prompt message's text (its first non-empty text
// block); ok=false for an assistant message, a tool-result turn or a user
// message with no text.
func PromptText(m agent.Message) (string, bool) {
	if m.Role != "user" {
		return "", false
	}
	text := ""
	for _, b := range m.Content {
		if b.Type == agent.BlockToolResult {
			return "", false
		}
		if b.Type == agent.BlockText && text == "" && strings.TrimSpace(b.Text) != "" {
			text = b.Text
		}
	}
	return text, text != ""
}

// RewindTo truncates the conversation to just before the user prompt at
// History[index] and returns that prompt's text (for the composer). The
// rewound conversation is FORKED: when the host can rotate to a
// fresh on-disk session (NewSessionStore) the truncated history is written
// there and continues there, leaving the full transcript resumable; otherwise
// an in-place rewritable store is rewritten, else only memory is truncated.
func (c *Conversation) RewindTo(index int) (string, error) {
	if index < 0 || index >= len(c.History) {
		return "", fmt.Errorf("message %d is no longer in the conversation", index)
	}
	text, ok := PromptText(c.History[index])
	if !ok {
		return "", fmt.Errorf("message %d is not a user prompt", index)
	}
	kept := append([]agent.Message(nil), c.History[:index]...)
	c.History = kept
	// A deferred round, if any, was in the dropped part (a prompt never
	// follows an unanswered round).
	c.dropDeferred()
	c.pendingBoundary = nil
	c.resetUsage()

	persisted := false
	if c.NewSessionStore != nil {
		if rec := c.NewSessionStore(); rec != nil {
			if len(kept) == 0 {
				// The fresh session stays lazy until the next turn.
				c.Store, persisted = rec, true
			} else if r, ok := rec.(historyReplacer); ok && r.ReplaceHistory(kept) == nil {
				c.Store, persisted = rec, true
			}
		}
	}
	if !persisted && c.Store != nil {
		if r, ok := c.Store.(historyReplacer); ok {
			_ = r.ReplaceHistory(kept)
		}
	}
	c.persistedLen = len(kept)
	return text, nil
}
