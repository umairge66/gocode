package mcphost

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

const (
	// DefaultConnectTimeout bounds connect + initialize + tools/list per server.
	DefaultConnectTimeout = 30 * time.Second

	// DefaultToolTimeout is the per-call default when neither the server config
	// nor MCP_TOOL_TIMEOUT sets one: 1e8 ms (~27.8 h), i.e. effectively none.
	DefaultToolTimeout = 100_000_000 * time.Millisecond

	minToolTimeout = time.Second
	maxTimerMillis = 2147483647 // largest timer delay (2^31-1 ms)
	maxTimeout     = maxTimerMillis * time.Millisecond

	// DefaultMaxOutputTokens is the MCP tool-result budget in tokens.
	DefaultMaxOutputTokens = 25000

	// Environment variables that override the limits.
	EnvConnectTimeout  = "MCP_TIMEOUT"
	EnvToolTimeout     = "MCP_TOOL_TIMEOUT"
	EnvMaxOutputTokens = "MAX_MCP_OUTPUT_TOKENS"

	// charsPerToken is the rough token estimator: round(len/4).
	charsPerToken = 4
	// truncCheckRatio: results estimated at ≤ limit*0.5 tokens are never
	// truncated; above limit*4 they always are.
	truncSkipRatio = 0.5
)

func envInt(getenv func(string) string, key string) (int, bool) {
	if getenv == nil {
		return 0, false
	}
	s := strings.TrimSpace(getenv(key))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ConnectTimeoutFromEnv returns MCP_TIMEOUT (ms, when > 0, capped at 2^31-1)
// or DefaultConnectTimeout.
func ConnectTimeoutFromEnv(getenv func(string) string) time.Duration {
	if n, ok := envInt(getenv, EnvConnectTimeout); ok && n > 0 {
		return time.Duration(min(n, maxTimerMillis)) * time.Millisecond
	}
	return DefaultConnectTimeout
}

// ToolTimeout resolves the per-call timeout for one server: the server
// config's "timeout" (ms) when ≥ 1000, else envDefault (MCP_TOOL_TIMEOUT),
// else DefaultToolTimeout; the result is clamped to [1s, 2^31-1 ms].
func ToolTimeout(cfg mcp.ServerConfig, envDefault time.Duration) time.Duration {
	var d time.Duration
	switch {
	case cfg.Timeout >= 1000:
		d = time.Duration(cfg.Timeout) * time.Millisecond
	case envDefault > 0:
		d = envDefault
	default:
		d = DefaultToolTimeout
	}
	return min(max(d, minToolTimeout), maxTimeout)
}

// ToolTimeoutFromEnv reads MCP_TOOL_TIMEOUT (ms, ≥ 1); 0 means unset.
func ToolTimeoutFromEnv(getenv func(string) string) time.Duration {
	if n, ok := envInt(getenv, EnvToolTimeout); ok && n >= 1 {
		return time.Duration(n) * time.Millisecond
	}
	return 0
}

// MaxOutputTokensFromEnv reads MAX_MCP_OUTPUT_TOKENS (> 0) or returns
// DefaultMaxOutputTokens.
func MaxOutputTokensFromEnv(getenv func(string) string) int {
	if n, ok := envInt(getenv, EnvMaxOutputTokens); ok && n > 0 {
		return n
	}
	return DefaultMaxOutputTokens
}

// truncatedMarker is the prefix of the truncation notice.
const truncatedMarker = "[OUTPUT TRUNCATED - exceeded "

// truncationNotice is appended to truncated output.
func truncationNotice(limit int) string {
	return "\n\n" + truncatedMarker + strconv.Itoa(limit) + " token limit]\n\n" +
		"The tool output was truncated. If this MCP server provides pagination or filtering tools, " +
		"use them to retrieve specific portions of the data. If pagination is not available, " +
		"inform the user that you are working with truncated output and results may be incomplete."
}

func estimateTokens(s string) int {
	return int(math.Round(float64(len(s)) / charsPerToken))
}

// TruncateOutput applies the MCP output budget (in tokens) to a flattened
// result. Output estimated at ≤ half the budget is returned untouched; output
// over the budget is cut to budget*4 characters and the notice is appended.
// Between half and four times the budget the length/4 estimate decides.
func TruncateOutput(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}
	est := estimateTokens(text)
	// ≤ budget*0.5: never truncated; > budget*4: always; in between the
	// estimate decides.
	if float64(est) <= float64(maxTokens)*truncSkipRatio || est <= maxTokens {
		return text, false
	}
	limit := maxTokens * charsPerToken
	cut := text
	if len(cut) > limit {
		cut = strings.ToValidUTF8(cut[:limit], "")
	}
	return cut + truncationNotice(maxTokens), true
}

// FlattenContent renders MCP content blocks as plain text for the model: text
// blocks verbatim; image/audio blocks as a short placeholder; embedded
// resources as their text when they carry text, else a placeholder;
// resource_link blocks as a placeholder naming the URI. When there is no
// content but structuredContent is present, its JSON is returned.
func FlattenContent(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, len(res.Content))
	for _, b := range res.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, placeholder("Image", b.MimeType, ""))
		case "audio":
			parts = append(parts, placeholder("Audio", b.MimeType, ""))
		case "resource":
			var r struct {
				URI      string  `json:"uri"`
				MimeType string  `json:"mimeType"`
				Text     *string `json:"text"`
			}
			_ = json.Unmarshal(b.Resource, &r)
			if r.Text != nil {
				parts = append(parts, *r.Text)
			} else {
				parts = append(parts, placeholder("Resource", r.MimeType, r.URI))
			}
		case "resource_link":
			parts = append(parts, placeholder("Resource link", b.MimeType, ""))
		default:
			parts = append(parts, placeholder("Unsupported content: "+b.Type, "", ""))
		}
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		return string(res.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

func placeholder(kind, mime, uri string) string {
	var extra []string
	if mime != "" {
		extra = append(extra, mime)
	}
	if uri != "" {
		extra = append(extra, uri)
	}
	if len(extra) == 0 {
		return "[" + kind + "]"
	}
	return fmt.Sprintf("[%s: %s]", kind, strings.Join(extra, " "))
}
