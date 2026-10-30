package plugineval

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall is one tool_use in a run's trace with its result.
type ToolCall struct {
	Name      string
	Input     any
	InputText string
	Output    string
	HasOutput bool
	IsError   bool
	// DeniedByChild is set when the child's result lists the call among its
	// permission_denials.
	DeniedByChild bool
}

// RunTrace is a parsed child run.
type RunTrace struct {
	Records           []map[string]any
	ToolCalls         []ToolCall
	LastAssistantText string
	NumTurns          int
	CostUSD           float64
	TimedOut          bool
	Error             string // "" when the run ended cleanly
	HasError          bool
	EndedOnAPIError   bool
	TracePath         string
}

// ChildExit is how the child process ended ({kind: ok | error | exit}).
type ChildExit struct {
	Kind       string // "ok", "error", "exit"
	Message    string // for "error"
	Code       string // for "exit": the exit code, or "signal X"
	StderrTail string
}

const noStderr = "(no stderr)"

// ParseTrace parses the child's stream-json records into a trace.
func ParseTrace(records []map[string]any, timedOut bool, exit ChildExit, tracePath string) RunTrace {
	t := RunTrace{Records: records, TimedOut: timedOut, TracePath: tracePath}
	byID := map[string]int{}
	seenMsg := map[string]bool{}
	assistantMsgs := 0
	var resultErr *string
	resultSeen := false
	for _, rec := range records {
		switch rec["type"] {
		case "assistant":
			msg, _ := rec["message"].(map[string]any)
			id, _ := msg["id"].(string)
			if id == "" || !seenMsg[id] {
				if id != "" {
					seenMsg[id] = true
				}
				assistantMsgs++
			}
			content, _ := msg["content"].([]any)
			var texts []string
			for _, b := range content {
				blk, _ := b.(map[string]any)
				switch blk["type"] {
				case "tool_use":
					tc := ToolCall{Name: fmt.Sprint(nz(blk["name"])), Input: blk["input"], InputText: stringify(blk["input"])}
					t.ToolCalls = append(t.ToolCalls, tc)
					if tid, ok := blk["id"].(string); ok {
						byID[tid] = len(t.ToolCalls) - 1
					}
				case "text":
					texts = append(texts, fmt.Sprint(nz(blk["text"])))
				}
			}
			if len(texts) > 0 {
				t.LastAssistantText = strings.Join(texts, "\n")
			}
		case "user":
			msg, _ := rec["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, b := range content {
				blk, _ := b.(map[string]any)
				if blk["type"] != "tool_result" {
					continue
				}
				tid, _ := blk["tool_use_id"].(string)
				if i, ok := byID[tid]; ok {
					t.ToolCalls[i].Output = toolResultText(blk["content"])
					t.ToolCalls[i].HasOutput = true
					t.ToolCalls[i].IsError = blk["is_error"] == true
				}
			}
		case "result":
			resultSeen = true
			if n, ok := toFloat(numberOf(rec["num_turns"])); ok {
				t.NumTurns = int(n)
			}
			if c, ok := toFloat(numberOf(rec["total_cost_usd"])); ok {
				t.CostUSD = c
				if t.CostUSD < 0 {
					t.CostUSD = 0
				}
			}
			_, apiStatus := toFloat(numberOf(rec["api_error_status"]))
			t.EndedOnAPIError = rec["is_error"] == true && apiStatus
			if dens, ok := rec["permission_denials"].([]any); ok {
				for _, d := range dens {
					dm, _ := d.(map[string]any)
					if tid, ok := dm["tool_use_id"].(string); ok {
						if i, ok := byID[tid]; ok {
							t.ToolCalls[i].DeniedByChild = true
						}
					}
				}
			}
			if rec["is_error"] == true {
				if s, ok := rec["result"].(string); ok {
					resultErr = &s
				} else if errs, ok := rec["errors"].([]any); ok && len(errs) > 0 {
					var parts []string
					for _, e := range errs {
						parts = append(parts, fmt.Sprint(e))
					}
					s := strings.Join(parts, " ")
					resultErr = &s
				}
			}
		}
	}
	switch exit.Kind {
	case "ok":
	case "error":
		t.Error, t.HasError = exit.Message, true
	case "exit":
		tail := exit.StderrTail
		if resultErr != nil {
			msg := fmt.Sprintf("exit %s: %s", exit.Code, truncateRunes(*resultErr, 2000))
			if tail != "" {
				msg += " · stderr: " + tail
			}
			t.Error = msg
		} else {
			if tail == "" {
				tail = noStderr
			}
			t.Error = fmt.Sprintf("exit %s: %s", exit.Code, tail)
		}
		t.HasError = true
	}
	if !resultSeen && t.NumTurns == 0 {
		t.NumTurns = assistantMsgs
	}
	return t
}

func nz(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func numberOf(v any) any {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		if err == nil {
			return f
		}
	}
	return v
}

// toolResultText: a tool_result's content as text.
func toolResultText(c any) string {
	switch t := c.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var parts []string
		for _, b := range t {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
					continue
				}
			}
			parts = append(parts, stringify(b))
		}
		return strings.Join(parts, "\n")
	default:
		return stringify(t)
	}
}

// stringify is JSON.stringify with Go's key order (sorted).
func stringify(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// elideTrace: at most 24 records, else the first and last 12 around a
// "[…N messages elided…]" marker.
func elideTrace(records []map[string]any) string {
	var lines []string
	if len(records) <= 24 {
		for _, r := range records {
			lines = append(lines, stringify(r))
		}
		return strings.Join(lines, "\n")
	}
	for _, r := range records[:12] {
		lines = append(lines, stringify(r))
	}
	lines = append(lines, fmt.Sprintf("[…%d messages elided…]", len(records)-24))
	for _, r := range records[len(records)-12:] {
		lines = append(lines, stringify(r))
	}
	return strings.Join(lines, "\n")
}
