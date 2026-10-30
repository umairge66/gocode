package compaction

import (
	"strings"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/apiclient"
)

// Token accounting for the compaction trigger.
//
//	tokens(msgs) = anchor found ? anchor.tokens + estimate(msgs after anchor) : estimate(msgs)
//
// where the anchor search walks back from the end and stops at the most recent
// assistant message carrying a non-zero API usage (its tokens are
// input_tokens + cache_creation_input_tokens + cache_read_input_tokens +
// output_tokens), or at a compact_boundary (tokens 0, anchor at the boundary).
// The estimate is chars/4 (rounded) over every content block: text, tool_use
// (name + JSON input), tool_result (its content), thinking, redacted_thinking
// (its data); image/document blocks count a flat 2000. ContextUsageTokens is
// the anchor's token count; EstimateTokens is the estimate. The anchor bookkeeping
// lives with the conversation (it owns the history and sees each response's
// usage), see conversation.ContextTokens.

// ImageTokenEstimate is the flat per-image/document estimate.
const ImageTokenEstimate = 2000

// ContextUsageTokens is the context size implied by one API response's usage:
// every input-side token (fresh, cache-written, cache-read) plus the response's
// own output tokens, which are now part of the conversation.
func ContextUsageTokens(u apiclient.Usage) int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
}

// UsageHasInput reports whether a usage carries any input-side tokens. A usage
// with none is ignored as an anchor.
func UsageHasInput(u apiclient.Usage) bool {
	return u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens > 0
}

// EstimateTokens is the chars/4 estimate over a message slice.
func EstimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += EstimateMessageTokens(m)
	}
	return n
}

// EstimateMessageTokens estimates one message (its content blocks).
func EstimateMessageTokens(m Message) int {
	n := 0
	for _, b := range m.Content {
		n += EstimateBlockTokens(b)
	}
	return n
}

// EstimateBlockTokens estimates one content block.
func EstimateBlockTokens(b ContentBlock) int {
	switch b.Type {
	case apiclient.BlockText:
		return charsToTokens(b.Text)
	case apiclient.BlockImage:
		return ImageTokenEstimate
	case apiclient.BlockToolResult:
		return toolResultTokens(b)
	case apiclient.BlockToolUse, apiclient.BlockServerToolUse, apiclient.BlockMCPToolUse:
		input := string(b.Input)
		if strings.TrimSpace(input) == "" {
			input = "{}" // a missing input counts as {}
		}
		return charsToTokens(b.Name + input)
	case apiclient.BlockThinking:
		return charsToTokens(b.Thinking)
	case apiclient.BlockRedactedThinking:
		return charsToTokens(b.Data)
	default:
		// The serialized block is approximated by its textual fields.
		return charsToTokens(b.Text + b.Content + string(b.Input))
	}
}

// toolResultTokens is the tool_result estimate: its text content plus a flat
// estimate per attached image.
func toolResultTokens(b ContentBlock) int {
	return charsToTokens(b.Content) + len(b.Images)*ImageTokenEstimate
}

// charsToTokens is round(len/4), where len counts UTF-16 code units.
func charsToTokens(s string) int {
	if s == "" {
		return 0
	}
	units := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	// Round-half-up for non-negative x/4: floor((x*2+4)/8) == floor(x/4 + 0.5).
	return (units*2 + 4) / 8
}
