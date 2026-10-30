package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Icon sets for the card style. A terminal UI cannot pick the font (the
// terminal does), so the glyphs come in three levels the user selects with the
// `ui.icons` setting or the GOCODE_ICONS environment variable:
//
//   - IconsNerd: Nerd Font glyphs (file, terminal, search, globe, robot…) for a
//     terminal running a patched font such as JetBrainsMono Nerd Font.
//   - IconsUnicode (default): plain Unicode symbols every modern font has
//     (✓ ✗ ✦ ❯ ● ⎿), with simple symbols for the tools.
//   - IconsASCII: 7-bit fallback, which also switches card borders to ASCII.
//
// The classic style keeps its own glyphs regardless of the level.

// IconLevel names an icon set.
type IconLevel string

const (
	// IconsUnicode is the default set: Unicode symbols, no special font needed.
	IconsUnicode IconLevel = "unicode"
	// IconsNerd uses Nerd Font private-use glyphs for tools and chrome.
	IconsNerd IconLevel = "nerd"
	// IconsASCII is the 7-bit fallback (ASCII glyphs and borders).
	IconsASCII IconLevel = "ascii"
)

// IconsEnv is the environment variable that overrides the `ui.icons` setting.
const IconsEnv = "GOCODE_ICONS"

// ParseIconLevel maps a setting/env value to an icon level (case-insensitive,
// surrounding space ignored). ok is false for an empty or unknown value.
func ParseIconLevel(s string) (IconLevel, bool) {
	switch IconLevel(strings.ToLower(strings.TrimSpace(s))) {
	case IconsNerd:
		return IconsNerd, true
	case IconsUnicode:
		return IconsUnicode, true
	case IconsASCII:
		return IconsASCII, true
	}
	return "", false
}

// ResolveIconLevel picks the icon level from the GOCODE_ICONS value (highest
// precedence), then the `ui.icons` setting, then the Unicode default. Unknown
// values are ignored.
func ResolveIconLevel(setting, env string) IconLevel {
	if l, ok := ParseIconLevel(env); ok {
		return l
	}
	if l, ok := ParseIconLevel(setting); ok {
		return l
	}
	return IconsUnicode
}

// IconSet is the glyph table for one level. Every glyph is one display cell
// wide (Nerd Font glyphs are single-cell private-use code points).
type IconSet struct {
	Brand    string // header/brand accent glyph
	Prompt   string // composer and user-message pointer
	Bullet   string // assistant message bullet; running tool status
	Success  string // tool finished
	Failure  string // tool failed / error notice
	Denied   string // tool use rejected by the user
	Notice   string // system notice bullet
	Pointer  string // selected option in menus and dialogs
	Sigil    string // finished-turn summary glyph
	Warning  string // key/setup warning in the header
	Branch   string // git branch prefix in the footer
	Folder   string // cwd prefix in the footer
	Ellipsis string // overflow marker

	// ModePause and ModePlay replace the permission-mode symbols (⏸ / ⏵⏵);
	// Spinner replaces the working-indicator frames (nil keeps SpinnerFrames).
	ModePause string
	ModePlay  string
	Spinner   []string

	// Tool icons, chosen by toolIcon from the tool name.
	File     string
	Edit     string
	Terminal string
	Search   string
	Globe    string
	Agent    string
	Todo     string
	Plug     string
	Tool     string

	// Border is the card/frame border.
	Border lipgloss.Border

	// The modern style's chrome: the user-message bar, a thin gutter for
	// tool output and diffs, the corner leading an error line, a notice dot,
	// and the todo states.
	Bar, Gutter, Corner, Dot       string
	TodoDone, TodoActive, TodoOpen string
	Online, Offline                string
}

var iconSets = map[IconLevel]IconSet{
	IconsUnicode: {
		Brand: "✦", Prompt: "❯", Bullet: messageBullet, Success: "✓", Failure: "✗",
		Denied: "⊘", Notice: "·", Pointer: "❯", Sigil: "✻", Warning: "⚠",
		Branch: "⎇", Folder: "", Ellipsis: "…",
		ModePause: modeSymbolPause, ModePlay: modeSymbolPlay,
		File: "≡", Edit: "✎", Terminal: "$", Search: "⌕", Globe: "◎",
		Agent: "◈", Todo: "▤", Plug: "◆", Tool: "◆",
		Border: lipgloss.RoundedBorder(),
		Bar:    "▌", Gutter: "│", Corner: "└", Dot: "·",
		TodoDone: "✓", TodoActive: "◐", TodoOpen: "○", Online: "●", Offline: "○",
	},
	IconsNerd: {
		Brand: "✦", Prompt: "❯", Bullet: messageBullet, Success: "", Failure: "",
		Denied: "", Notice: "", Pointer: "❯", Sigil: "✻", Warning: "",
		Branch: "", Folder: "", Ellipsis: "…",
		File: "", Edit: "", Terminal: "", Search: "", Globe: "",
		Agent: "\U000F06A9", Todo: "", Plug: "", Tool: "",
		Border: lipgloss.RoundedBorder(),
		Bar:    "▌", Gutter: "│", Corner: "└", Dot: "·",
		TodoDone: "✓", TodoActive: "◐", TodoOpen: "○", Online: "●", Offline: "○",
	},
	IconsASCII: {
		Brand: "*", Prompt: ">", Bullet: "*", Success: "+", Failure: "x",
		Denied: "-", Notice: "-", Pointer: ">", Sigil: "*", Warning: "!",
		Branch: "@", Folder: "", Ellipsis: "...",
		ModePause: "||", ModePlay: ">>", Spinner: []string{"-", "\\", "|", "/"},
		File: "", Edit: "", Terminal: "", Search: "", Globe: "",
		Agent: "", Todo: "", Plug: "", Tool: "",
		Border: lipgloss.ASCIIBorder(),
		Bar:    "|", Gutter: "|", Corner: "`", Dot: "-",
		TodoDone: "[x]", TodoActive: "[~]", TodoOpen: "[ ]", Online: "+", Offline: "-",
	},
}

// Icons returns the glyph table for a level (an unknown level yields the
// Unicode set).
func Icons(l IconLevel) IconSet {
	if s, ok := iconSets[l]; ok {
		return s
	}
	return iconSets[IconsUnicode]
}

// modeSymbol maps a permission-mode descriptor symbol to this set's glyph.
func (s IconSet) modeSymbol(sym string) string {
	switch sym {
	case modeSymbolPause:
		return s.ModePause
	case modeSymbolPlay:
		return s.ModePlay
	}
	return sym
}

// spinnerGlyph returns the working-indicator glyph for a status frame.
func (s IconSet) spinnerGlyph(st Status) string {
	if len(s.Spinner) == 0 || !st.IsActive() {
		return st.Glyph()
	}
	n := len(s.Spinner)
	return s.Spinner[((st.Frame%n)+n)%n]
}

// toolIcon picks the icon for a tool by name: file tools, shell, search, web,
// subagents, todos, MCP tools, and a generic fallback. The ASCII set has no
// tool icons ("").
func (s IconSet) toolIcon(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "mcp__"):
		return s.Plug
	case n == "read" || n == "ls" || n == "notebookread":
		return s.File
	case n == "write" || n == "edit" || n == "multiedit" || n == "notebookedit" ||
		n == "update" || n == "create":
		return s.Edit
	case n == "bash" || n == "powershell" || strings.HasPrefix(n, "bash") || n == "killshell":
		return s.Terminal
	case n == "grep" || n == "glob" || n == "toolsearch":
		return s.Search
	case n == "webfetch" || n == "websearch":
		return s.Globe
	case n == "agent" || n == "task":
		return s.Agent
	case n == "todowrite" || strings.HasPrefix(n, "task"):
		return s.Todo
	}
	return s.Tool
}
