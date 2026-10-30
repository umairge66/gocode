package compaction

import (
	"encoding/json"
	"strings"

	"github.com/umairge66/gocode/internal/apiclient"
)

// Microcompaction: clear the content of OLD bulky tool results before (or
// instead of) a full summary.
//
//   - "[Old tool result content cleared]" is the placeholder that replaces a
//     cleared tool_result's content.
//   - {Read, Bash, PowerShell, Grep, Glob, WebSearch, WebFetch, Edit, Write}
//     (also accepted behind the "mcp__container__" prefix) are the tools whose
//     results are eligible.
//   - Collect eligible tool_use ids in order, keep the last max(1,keepRecent),
//     and sum the estimated tokens of every not-yet-cleared tool_result
//     answering one of the others.
//   - If tokensSaved < 20000 nothing happens; otherwise replace those results'
//     content and report tokensSaved. keepRecent defaults to 5.
//   - The loop rewrites the in-memory message list and drops the
//     readFileState entries of cleared Read results. The clearing is
//     transient (never persisted): the transcript keeps the full tool
//     results, so a resumed session reloads them uncleared.
//
// Trigger. With provider API keys (OpenRouter et al.) there is no server-side
// context-hint signal, so the keep-recent clear runs at two points: (1) when
// the pre-turn autocompact
// check says the context is at/over the autocompact threshold — microcompact
// first, then re-check and run the full summary only if still needed — and
// (2) when the model stops with model_context_window_exceeded — microcompact
// and retry before falling back to an emergency summary. Cleared results are
// never persisted to a file; the plain placeholder is always used.

// ClearedToolResultPlaceholder replaces a cleared tool_result's content.
const ClearedToolResultPlaceholder = "[Old tool result content cleared]"

// persistedOutputPrefix marks a tool result already replaced by a persisted-
// output pointer; such results count as cleared.
const persistedOutputPrefix = "<persisted-output>"

// MicrocompactKeepRecent is how many of the most recent eligible tool results
// are kept verbatim.
const MicrocompactKeepRecent = 5

// MicrocompactMinSavings is the minimum estimated saving (tokens) for a
// microcompaction to apply.
const MicrocompactMinSavings = 20000

// mcpContainerPrefix is the alternate prefix under which the eligible tools are
// also recognized.
const mcpContainerPrefix = "mcp__container__"

// microcompactTools is the eligible set.
var microcompactTools = map[string]bool{
	"Read":       true,
	"Bash":       true,
	"PowerShell": true,
	"Grep":       true,
	"Glob":       true,
	"WebSearch":  true,
	"WebFetch":   true,
	"Edit":       true,
	"Write":      true,
}

// MicrocompactEligible reports whether results of the named tool may be
// cleared.
func MicrocompactEligible(name string) bool {
	if microcompactTools[name] {
		return true
	}
	return strings.HasPrefix(name, mcpContainerPrefix) && microcompactTools[strings.TrimPrefix(name, mcpContainerPrefix)]
}

// MicrocompactResult is the outcome of a microcompaction that applied.
type MicrocompactResult struct {
	// Messages is the rewritten message list (same length and order; only the
	// cleared tool_result blocks differ).
	Messages []Message
	// TokensSaved is the estimated tokens freed.
	TokensSaved int
	// ClearedIDs are the tool_use ids whose results were cleared.
	ClearedIDs []string
	// Kept is how many eligible tool uses were kept verbatim.
	Kept int
	// ClearedReadPaths are the file_path inputs of cleared Read calls, so the
	// host can forget their read state.
	ClearedReadPaths []string
}

// Microcompact clears the content of every eligible tool_result older than
// the keepRecent most recent eligible tool uses, provided that frees at least
// MicrocompactMinSavings estimated tokens. ok is false (and msgs is returned
// unchanged by the caller) when nothing qualifies. tool_use/tool_result pairing
// is preserved: only the result's content is replaced, never the block itself.
func Microcompact(msgs []Message, keepRecent int) (MicrocompactResult, bool) {
	return microcompact(msgs, keepRecent, MicrocompactMinSavings)
}

func microcompact(msgs []Message, keepRecent, minSavings int) (MicrocompactResult, bool) {
	// Eligible tool_use ids in order.
	var ids []string
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == apiclient.BlockToolUse && MicrocompactEligible(b.Name) {
				ids = append(ids, b.ID)
			}
		}
	}
	if keepRecent < 1 {
		keepRecent = 1 // at least one is kept
	}
	if len(ids) <= keepRecent {
		return MicrocompactResult{}, false
	}
	keep := len(ids) - keepRecent
	clear := make(map[string]bool, keep)
	for _, id := range ids[:keep] {
		clear[id] = true
	}

	saved := 0
	candidates := map[string]bool{}
	var order []string
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type != apiclient.BlockToolResult || !clear[b.ToolUseID] || alreadyCleared(b) {
				continue
			}
			saved += toolResultTokens(b)
			if !candidates[b.ToolUseID] {
				candidates[b.ToolUseID] = true
				order = append(order, b.ToolUseID)
			}
		}
	}
	if len(candidates) == 0 || saved < minSavings {
		return MicrocompactResult{}, false
	}

	// Replace the candidates' content, copying only the touched messages.
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if m.Role != RoleUser {
			continue
		}
		var blocks []ContentBlock
		for j, b := range m.Content {
			if b.Type != apiclient.BlockToolResult || !candidates[b.ToolUseID] {
				continue
			}
			if blocks == nil {
				blocks = append([]ContentBlock(nil), m.Content...)
			}
			nb := b
			nb.Content = ClearedToolResultPlaceholder
			nb.Images = nil
			blocks[j] = nb
		}
		if blocks != nil {
			out[i] = Message{Role: m.Role, Content: blocks}
		}
	}

	res := MicrocompactResult{
		Messages:    out,
		TokensSaved: saved,
		ClearedIDs:  order,
		Kept:        keepRecent,
	}
	// File paths of the cleared Read calls.
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type != apiclient.BlockToolUse || b.Name != "Read" || !candidates[b.ID] {
				continue
			}
			var in struct {
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.FilePath != "" {
				res.ClearedReadPaths = append(res.ClearedReadPaths, in.FilePath)
			}
		}
	}
	return res, true
}

// alreadyCleared reports whether a tool_result was cleared earlier.
func alreadyCleared(b ContentBlock) bool {
	return len(b.Images) == 0 && (b.Content == ClearedToolResultPlaceholder || strings.HasPrefix(b.Content, persistedOutputPrefix))
}
