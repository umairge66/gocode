package ui

import (
	"image/color"
	"strings"
	"sync"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"
)

// keyMap is the modern style's key map (Bubbles key bindings). It drives both
// the new shortcuts the Model matches with key.Matches (palette, model
// picker, sidebar) and the context-sensitive help bar at the bottom of the
// screen, which the Bubbles help component renders from the bindings that
// apply in the current state.
type keyMap struct {
	Send, Newline, Commands, Files, Palette, Model, Mode key.Binding
	Sidebar, Expand, Scroll, Interrupt, Quit, History    key.Binding
	Select, Confirm, Cancel, Toggle, Filter, Complete    key.Binding
	Stop, Close, Main, Agents, Preview                   key.Binding
}

// keys is the key map.
var keys = keyMap{
	Send:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
	Newline:   key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"), key.WithHelp("⇧enter", "newline")),
	Commands:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "commands")),
	Files:     key.NewBinding(key.WithKeys("@"), key.WithHelp("@", "files")),
	Palette:   key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "palette")),
	Model:     key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("ctrl+l", "model")),
	Mode:      key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧tab", "mode")),
	Sidebar:   key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "sidebar")),
	Expand:    key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("ctrl+o", "expand")),
	Scroll:    key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/dn", "scroll")),
	Interrupt: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "interrupt")),
	Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	History:   key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "history")),
	Select:    key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "select")),
	Confirm:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
	Cancel:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	Toggle:    key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")),
	Filter:    key.NewBinding(key.WithKeys("type"), key.WithHelp("type", "filter")),
	Complete:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "complete")),
	Stop:      key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "stop")),
	Close:     key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "close")),
	Main:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "main")),
	Agents:    key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "agents")),
	Preview:   key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "preview")),
}

// helpBindings returns the shortcuts that apply in the current state, most
// useful first (the help bar drops items from the right when narrow).
func helpBindings(vs ViewState) []key.Binding {
	expand := keys.Expand
	if vs.ExpandResults {
		expand.SetHelp("ctrl+o", "collapse")
	}
	switch {
	case vs.Palette != nil:
		if vs.Palette.Preview {
			return []key.Binding{keys.Preview, keys.Confirm, keys.Cancel, keys.Filter}
		}
		return []key.Binding{keys.Select, keys.Confirm, keys.Cancel, keys.Filter}
	case vs.Dialog != nil:
		d := vs.Dialog
		switch {
		case d.AwaitingFeedback:
			return []key.Binding{withHelp(keys.Confirm, "send"), keys.Cancel}
		case d.Kind == DialogSecret:
			return []key.Binding{withHelp(keys.Confirm, "save"), keys.Cancel}
		case d.Kind == DialogList:
			return []key.Binding{keys.Select, keys.Confirm, keys.Cancel, keys.Filter}
		case d.MultiSelect:
			return []key.Binding{keys.Select, keys.Toggle, keys.Confirm, keys.Cancel}
		}
		return []key.Binding{keys.Select, keys.Confirm, keys.Cancel}
	case vs.CommandMenuActive() || vs.FileMenuActive():
		return []key.Binding{keys.Select, keys.Complete, withHelp(keys.Send, "run"), withHelp(keys.Cancel, "close")}
	case vs.ViewAgentID != "":
		bs := []key.Binding{}
		if vs.ViewAgentStatus == AgentViewRunning {
			bs = append(bs, keys.Stop)
		}
		return append(bs, keys.Close, keys.Main, expand, keys.Scroll)
	case vs.AgentListFocused:
		return []key.Binding{keys.Select, withHelp(keys.Confirm, "view"), withHelp(keys.Cancel, "back")}
	case vs.Status.IsActive():
		return []key.Binding{keys.Interrupt, expand, keys.Scroll, keys.Palette, keys.Sidebar}
	}
	newline := keys.Newline
	if !vs.ShiftEnterNewline {
		// Without key disambiguation Shift+Enter reads as Enter; a trailing
		// "\" before Enter is the line break that works everywhere.
		newline.SetHelp("\\⏎", "newline")
	}
	bs := []key.Binding{keys.Send, newline, keys.Commands, keys.Files, keys.Palette, keys.Model, keys.Mode, expand}
	if len(vs.ActiveAgents) > 0 {
		bs = append(bs, keys.Agents)
	}
	return append(bs, keys.Sidebar, keys.Scroll)
}

// withHelp returns a copy of b with a different help description.
func withHelp(b key.Binding, desc string) key.Binding {
	b.SetHelp(b.Help().Key, desc)
	return b
}

// helpView renders the bindings as one line no wider than width. The help
// bar is redrawn every frame but changes only with the context, so its last
// render is reused.
func helpView(ms mstyle, bindings []key.Binding, width int) string {
	var kb strings.Builder
	for _, b := range bindings {
		kb.WriteString(b.Help().Key + "\x00" + b.Help().Desc + "\x00")
	}
	hk := helpKey{bindings: kb.String(), width: width, a: ms.textC, b: ms.mutedC, c: ms.ruleC, ellipsis: ms.ic.Ellipsis}
	helpMu.Lock()
	if helpLast.key == hk && helpLast.ok {
		v := helpLast.view
		helpMu.Unlock()
		return v
	}
	helpMu.Unlock()
	if ms.ic.Ellipsis == "..." { // the ASCII icon level: spell the arrows out
		ascii := make([]key.Binding, len(bindings))
		r := strings.NewReplacer("⇧", "shift+", "↑↓", "up/dn", "↓", "down", "↑", "up", "⏎", "+enter")
		for i, b := range bindings {
			ascii[i] = b
			ascii[i].SetHelp(r.Replace(b.Help().Key), b.Help().Desc)
		}
		bindings = ascii
	}
	v := helpViewUncached(ms, bindings, width)
	helpMu.Lock()
	helpLast.key, helpLast.view, helpLast.ok = hk, v, true
	helpMu.Unlock()
	return v
}

// helpKey identifies a help bar render.
type helpKey struct {
	bindings string
	width    int
	a, b, c  color.Color
	ellipsis string
}

var (
	helpMu   sync.Mutex
	helpLast struct {
		key  helpKey
		view string
		ok   bool
	}
)

// helpViewUncached renders helpView.
func helpViewUncached(ms mstyle, bindings []key.Binding, width int) string {
	h := help.New()
	h.ShortSeparator = " · "
	h.Ellipsis = ms.ic.Ellipsis
	h.Styles.ShortKey = ms.text
	h.Styles.ShortDesc = ms.muted
	h.Styles.ShortSeparator = ms.rule
	h.Styles.Ellipsis = ms.muted
	h.SetWidth(width)
	// The help view drops items that do not fit only while its ellipsis still
	// fits, so keep the longest prefix that renders within the width.
	for n := len(bindings); n > 0; n-- {
		if v := h.ShortHelpView(bindings[:n]); ansi.StringWidth(v) <= width {
			return v
		}
	}
	return ""
}
