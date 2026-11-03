package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/tools/browser"
)

// Transcript labels for the Chrome tools. The raw tool names (computer,
// read_page, …) and their raw results (the <untrusted-web-content> fence, the
// model-facing notices, "computer: type done.") are written for the model; the
// user sees a readable header such as Type("Umair Saeed") or Navigate(google.com)
// and a short result instead. The model still receives the full raw result.

// browserVerbs is the header word for each Chrome tool (computer is resolved
// per action by computerVerb).
var browserVerbs = map[string]string{
	"navigate":      "Navigate",
	"read_page":     "Read page",
	"get_page_text": "Page text",
	"find":          "Find",
	"form_input":    "Fill",
	"tabs_context":  "Tabs",
	"tabs_create":   "New tab",
	"tabs_close":    "Close tab",
	"screenshot":    "Screenshot",
	"browser_batch": "Browser",
	"computer":      "Computer",
}

// computerVerbs is the header word for each computer action.
var computerVerbs = map[string]string{
	"left_click":      "Click",
	"right_click":     "Right-click",
	"double_click":    "Double-click",
	"triple_click":    "Triple-click",
	"middle_click":    "Middle-click",
	"hover":           "Hover",
	"mouse_move":      "Hover",
	"left_click_drag": "Drag",
	"scroll":          "Scroll",
	"scroll_to":       "Scroll to",
	"type":            "Type",
	"key":             "Key",
	"keypress":        "Key",
	"screenshot":      "Screenshot",
	"zoom":            "Zoom",
	"wait":            "Wait",
}

// isBrowserTool reports whether name is one of the Chrome tools.
func isBrowserTool(name string) bool {
	_, ok := browserVerbs[name]
	return ok
}

// computerVerb is the header word for a computer action ("left_click" → "Click");
// an unknown action is humanized ("left_mouse_down" → "Left mouse down").
func computerVerb(action string) string {
	if v, ok := computerVerbs[action]; ok {
		return v
	}
	if action == "" {
		return "Computer"
	}
	s := strings.ReplaceAll(action, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// browserHeader returns the header verb and argument summary for a Chrome tool
// call.
func browserHeader(name string, input json.RawMessage) (verb, args string) {
	verb = browserVerbs[name]
	switch name {
	case "computer":
		var in browser.ComputerInput
		_ = json.Unmarshal(input, &in)
		return computerVerb(in.Action), computerArgs(in)
	case "navigate", "tabs_create":
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &in)
		return verb, shortURL(in.URL)
	case "find":
		var in browser.FindInput
		_ = json.Unmarshal(input, &in)
		return verb, quoteArg(in.Query)
	case "form_input":
		var in browser.FormInputInput
		_ = json.Unmarshal(input, &in)
		val := strings.Trim(string(in.Value), `"`)
		if val == "" {
			return verb, in.Ref
		}
		return verb, in.Ref + " = " + quoteArg(val)
	case "browser_batch":
		var in browser.BatchInput
		_ = json.Unmarshal(input, &in)
		steps := make([]string, 0, len(in.Actions))
		for _, a := range in.Actions {
			step := a.Name
			if a.Name == "computer" {
				var c browser.ComputerInput
				_ = json.Unmarshal(a.Input, &c)
				step = strings.ToLower(computerVerb(c.Action))
			}
			steps = append(steps, step)
		}
		if len(steps) == 0 {
			return verb, ""
		}
		noun := "steps"
		if len(steps) == 1 {
			noun = "step"
		}
		return verb, clip(fmt.Sprintf("%d %s: %s", len(steps), noun, strings.Join(steps, ", ")), 70)
	default:
		// read_page, get_page_text, tabs_context, screenshot, tabs_close: the
		// verb says it all.
		return verb, ""
	}
}

// computerArgs summarizes a computer gesture: the target (element ref or x, y),
// the typed text, the key, the scroll direction, or the wait time.
func computerArgs(in browser.ComputerInput) string {
	target := in.Ref
	if target == "" && len(in.Coordinate) == 2 {
		target = fmt.Sprintf("%d, %d", in.Coordinate[0], in.Coordinate[1])
	}
	switch in.Action {
	case "type":
		return quoteArg(in.Text)
	case "key", "keypress":
		return in.Text
	case "left_click_drag":
		if len(in.StartCoordinate) == 2 && target != "" {
			return fmt.Sprintf("%d, %d → %s", in.StartCoordinate[0], in.StartCoordinate[1], target)
		}
		return target
	case "scroll":
		s := in.ScrollDirection
		if in.ScrollAmount > 0 {
			s += " ×" + strconv.Itoa(int(in.ScrollAmount))
		}
		return strings.TrimSpace(s)
	case "wait":
		if in.Duration > 0 {
			return strconv.FormatFloat(in.Duration, 'f', -1, 64) + "s"
		}
		return ""
	case "screenshot", "zoom":
		return ""
	default:
		if in.Modifiers != "" && target != "" {
			return in.Modifiers + " " + target
		}
		return target
	}
}

// untrustedOpenRE matches the opening fence of page text in a tool result.
var untrustedOpenRE = regexp.MustCompile(`<untrusted-web-content(?:\s+source="([^"]*)")?>\n?`)

// browserResult is the transcript text for a Chrome tool result: the page-text
// fence and the model-facing notices removed, and terse acknowledgements
// replaced by what happened.
func browserResult(name string, input json.RawMessage, result string) string {
	if name == "computer" {
		var in browser.ComputerInput
		_ = json.Unmarshal(input, &in)
		if strings.HasPrefix(result, "computer: ") && strings.HasSuffix(strings.TrimSpace(result), "done.") {
			switch in.Action {
			case "type":
				n := len([]rune(in.Text))
				if n == 1 {
					return "Typed 1 character"
				}
				return fmt.Sprintf("Typed %d characters", n)
			case "key", "keypress":
				return "Pressed " + in.Text
			case "wait":
				return "Waited"
			default:
				return "Done"
			}
		}
	}
	out := strings.ReplaceAll(result, browser.UntrustedTextNotice, "")
	out = strings.ReplaceAll(out, browser.UntrustedImageNotice, "")
	var source string
	if m := untrustedOpenRE.FindStringSubmatch(out); m != nil {
		source = m[1]
	}
	out = untrustedOpenRE.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, "</untrusted-web-content>", "")
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "Screenshot of the active tab") || strings.HasPrefix(out, "Screenshot of the gocode tab") {
		return "Captured screenshot"
	}
	if source != "" && (name == "read_page" || name == "get_page_text") {
		out = "Page: " + shortURL(source) + "\n" + out
	}
	return out
}

// shortURL renders a URL compactly for a header: no scheme, no "www.", no query
// string or fragment, at most 60 characters.
func shortURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return clip(raw, 60)
	}
	s := strings.TrimPrefix(u.Host, "www.") + strings.TrimSuffix(u.Path, "/")
	return clip(s, 60)
}

// quoteArg quotes a free-text argument, clipped to keep the header to one line.
func quoteArg(s string) string {
	if s == "" {
		return ""
	}
	return `"` + clip(firstLine(s), 50) + `"`
}

// clip truncates s to max runes with a trailing "…".
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
