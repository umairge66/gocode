package browser

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Num is a whole-number tool parameter (a pixel coordinate, a count, a tab
// id) that the schema declares as a JSON number. Models do not always send
// one: a fractional value such as 120.5 is rounded to the nearest integer,
// and a number sent as a string ("1527560224") is read as that number,
// instead of failing the call.
type Num int

func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal([]byte(s), &str); err != nil {
			return fmt.Errorf("browser: %s is not a number", s)
		}
		s = strings.TrimSpace(str)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1<<53 {
		return fmt.Errorf("browser: %s is not a number", s)
	}
	*n = Num(math.Round(f))
	return nil
}

// Nums converts ints to Num values (for building inputs in code).
func Nums(v ...int) []Num {
	out := make([]Num, len(v))
	for i, x := range v {
		out[i] = Num(x)
	}
	return out
}

// pageNotes renders the notes the extension attaches to a result (a dialog it
// dismissed, a capped wait). Dialog text comes from the page, so the notes are
// fenced; notice adds the untrusted-content notice.
func pageNotes(notes []string, notice bool) string {
	if len(notes) == 0 {
		return ""
	}
	body := "- " + strings.Join(notes, "\n- ")
	if notice {
		return "\n\nNotes:\n" + wrapUntrusted("", body)
	}
	return "\n\nNotes:\n" + fenceUntrusted("", body)
}

// inputDelivery is what the extension reports about how a computer input
// action reached the page (extension/background.js, "background tabs").
// Delivery is "input" (the page received real input), "dom" (Chrome did not
// deliver it, so the extension sent the same events from inside the page,
// where they are untrusted) or "unconfirmed" (sent, but the page could not be
// checked). Input that reached the page neither way is an error, never a
// result.
type inputDelivery struct {
	Drag          string `json:"drag"`
	Delivery      string `json:"delivery"`
	DomReason     string `json:"domReason"`
	Background    bool   `json:"background"`
	Scrolled      *bool  `json:"scrolled"`
	KeysCancelled bool   `json:"keysCancelled"`
}

// outcome is the action's short status: "done", "done with DOM events, not
// real input" or "sent, not confirmed", with the kind of drag it was.
func (d inputDelivery) outcome() string {
	s := "done"
	switch d.Delivery {
	case "dom":
		s = "done with DOM events, not real input"
	case "unconfirmed":
		s = "sent, not confirmed"
	}
	if d.Drag != "" {
		s += " (as a " + d.Drag + ")"
	}
	return s
}

// note explains an outcome that is not plain real input ("" when it is).
// Every word is the extension's own, none comes from the page.
func (d inputDelivery) note() string {
	switch d.Delivery {
	case "dom":
		if d.DomReason == "background-scroll" {
			s := "the tab is in the background, where a real mouse wheel can make Chrome switch the user's tabs, so gocode scrolled from inside the page (an untrusted wheel event, then a scroll of the element under the point)."
			if d.Scrolled != nil && !*d.Scrolled {
				s += " The page cancelled the wheel event, so nothing scrolled."
			}
			return s
		}
		s := "Chrome did not deliver real input to the page"
		if d.Background {
			s += " (the tab is in the background)"
		}
		s += ", so gocode sent the same events from inside the page. They are untrusted (event.isTrusted is false): a page that ignores untrusted events, or a browser default action such as text selection or :hover styles, may not respond. Take a screenshot to check the result."
		if d.KeysCancelled {
			s += " The page cancelled some of the key presses."
		}
		return s
	case "unconfirmed":
		return "gocode could not check that the page received it (extension scripts cannot run on this page). Take a screenshot to see whether it worked."
	}
	return ""
}

// captureInfo is what the extension reports with a screenshot or zoom.
type captureInfo struct {
	DataURL     string    `json:"dataUrl"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Region      []float64 `json:"region"`
	Zoom        float64   `json:"zoom"`
	Scale       float64   `json:"scale"`
	ImageWidth  int       `json:"imageWidth"`
	ImageHeight int       `json:"imageHeight"`
}

// frameNote says how the image maps to click coordinates when it is not the
// plain viewport (a zoomed region or a scaled-down capture).
func (ci captureInfo) frameNote() string {
	frame := ""
	if ci.Width > 0 && ci.Height > 0 {
		frame = fmt.Sprintf(" %dx%d", ci.Width, ci.Height)
	}
	switch {
	case len(ci.Region) == 4:
		return fmt.Sprintf("Zoomed region (%g, %g)-(%g, %g) at %gx (%dx%d image). Coordinates for clicks stay in the full%s viewport frame, not this image's pixels.",
			ci.Region[0], ci.Region[1], ci.Region[2], ci.Region[3], ci.Zoom, ci.ImageWidth, ci.ImageHeight, frame)
	case ci.Scale > 0 && ci.Scale < 1:
		return fmt.Sprintf("Scaled to %g (%dx%d image). Coordinates for clicks are in the full%s viewport frame, not this image's pixels.",
			ci.Scale, ci.ImageWidth, ci.ImageHeight, frame)
	}
	return ""
}

// match is one element find located: a ref usable with computer/form_input,
// its accessible name and role, the center to click, and what it is.
type match struct {
	Ref       string  `json:"ref"`
	Text      string  `json:"text"`
	Role      string  `json:"role"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	Tag       string  `json:"tag"`
	Type      string  `json:"type"`
	Name      string  `json:"name"`
	Value     *string `json:"value"` // nil: the element has no value to show
	Disabled  bool    `json:"disabled"`
	Hidden    bool    `json:"hidden"`
	Offscreen bool    `json:"offscreen"`
}

// renderMatches lists find matches, one per line.
func renderMatches(ms []match) string {
	var b strings.Builder
	for _, m := range ms {
		role := m.Role
		if role == "" {
			role = "element"
		}
		fmt.Fprintf(&b, "- %s [%s] %q at (%d, %d)", m.Ref, role, m.Text, m.X, m.Y)
		var what []string
		if m.Tag != "" {
			w := m.Tag
			if m.Type != "" {
				w += " type=" + m.Type
			}
			what = append(what, w)
		}
		if m.Name != "" {
			what = append(what, fmt.Sprintf("name=%q", m.Name))
		}
		if m.Value != nil {
			what = append(what, fmt.Sprintf("value=%q", *m.Value))
		}
		if m.Disabled {
			what = append(what, "disabled")
		}
		if m.Hidden {
			what = append(what, "hidden")
		} else if m.Offscreen {
			what = append(what, "off-screen (a click by ref scrolls it into view)")
		}
		if len(what) > 0 {
			b.WriteString(" — " + strings.Join(what, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// maxBatchStepText bounds the page text one browser_batch step shows.
const maxBatchStepText = 20000

func clipText(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]), true
}

// stepDetail renders what one browser_batch step returned (its result as the
// extension sent it), and reports whether it holds page-derived text. "" when
// the step has nothing to say beyond "ok".
func stepDetail(name string, raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	switch name {
	case "find":
		var r struct {
			Matches   []match `json:"matches"`
			Truncated bool    `json:"truncated"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return "", false
		}
		if len(r.Matches) == 0 {
			return "no elements matched", false
		}
		out := fenceUntrusted("", renderMatches(r.Matches))
		if r.Truncated {
			out += "\n[more matches exist — refine the query]"
		}
		return out, true
	case "read_page", "get_page_text":
		var r struct {
			TabID     int    `json:"tabId"`
			URL       string `json:"url"`
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return "", false
		}
		if r.Text == "" {
			return "(the page has no readable text)", false
		}
		text, cut := clipText(r.Text, maxBatchStepText)
		out := fenceUntrusted(tabSource(r.URL, r.TabID), text)
		if cut || r.Truncated {
			out += "\n[page text truncated; read_page on its own returns more]"
		}
		return out, true
	case "javascript_tool":
		var r struct {
			TabID     int     `json:"tabId"`
			URL       string  `json:"url"`
			Value     *string `json:"value"`
			Exception string  `json:"exception"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return "", false
		}
		if r.Exception != "" {
			return fenceUntrusted(tabSource(r.URL, r.TabID), "JavaScript error: "+r.Exception), true
		}
		v := "undefined"
		if r.Value != nil && *r.Value != "" {
			v = *r.Value
		}
		v, cut := clipText(v, maxBatchStepText)
		out := fenceUntrusted(tabSource(r.URL, r.TabID), v)
		if cut {
			out += "\n[value truncated]"
		}
		return out, true
	case "navigate", "tabs_create", "tabs_create_mcp":
		var r struct {
			TabID int    `json:"tabId"`
			URL   string `json:"url"`
		}
		if json.Unmarshal(raw, &r) != nil || r.TabID == 0 {
			return "", false
		}
		if r.URL == "" {
			return fmt.Sprintf("tab %d", r.TabID), false
		}
		return fmt.Sprintf("tab %d at %s", r.TabID, sanitizePageText(r.URL)), false
	case "tabs_context", "tabs_context_mcp":
		var r struct {
			Tabs []tabInfo `json:"tabs"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return "", false
		}
		if len(r.Tabs) == 0 {
			return "no tabs", false
		}
		var b strings.Builder
		for _, tb := range r.Tabs {
			fmt.Fprintf(&b, "[%d] %s — %s\n", tb.ID, tb.Title, tb.URL)
		}
		return fenceUntrusted("", strings.TrimRight(b.String(), "\n")), true
	case "form_input":
		var r struct {
			Ref   string `json:"ref"`
			Value string `json:"value"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Ref == "" {
			return "", false
		}
		return fenceUntrusted("", fmt.Sprintf("%s now holds %q", r.Ref, r.Value)), true
	case "computer":
		var r inputDelivery
		if json.Unmarshal(raw, &r) != nil {
			return "", false
		}
		if note := r.note(); note != "" {
			return r.outcome() + ": " + note, false
		}
		if r.Drag != "" {
			return "done as a " + r.Drag, false
		}
	case "resize_window":
		var r struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Width > 0 {
			return fmt.Sprintf("window is %dx%d", r.Width, r.Height), false
		}
	}
	return "", false
}
