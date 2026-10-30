package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/themes"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the full-screen UI settings and the info the UI shows
// around the input: the `ui` settings object — `ui.style` ("modern" |
// "cards" | "classic"), `ui.icons` ("unicode" | "nerd" | "ascii",
// overridden by GOCODE_ICONS), `ui.animations` (true | false), `ui.sidebar`
// ("auto" | "on" | "off") and `ui.theme` (a theme name, overriding the
// top-level `theme`) — plus the cwd and git branch, the context-window usage
// and the MCP server status. Everything is read locally (settings files, git,
// the conversation); nothing is sent.

// uiSettings is the resolved `ui` settings object.
type uiSettings struct {
	style      ui.UIStyle
	icons      ui.IconLevel
	animations bool
	sidebar    ui.SidebarMode
	// theme is `ui.theme` ("" when unset; the top-level `theme` applies).
	theme string
}

// resolveUISettings reads the `ui` settings object from the effective settings
// sources (low→high precedence, last wins) and applies the GOCODE_ICONS
// override. Unknown or missing values fall back to the defaults (the modern
// style, Unicode icons, animations on, the sidebar on auto); it never fails.
func resolveUISettings(env *Env) uiSettings {
	s := uiSettings{style: ui.StyleModern, animations: true, sidebar: ui.SidebarAuto}
	var iconSetting string
	ctx := settingsContext(env)
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		obj, _ := or.Result.Settings["ui"].(map[string]any)
		if v, ok := obj["style"].(string); ok {
			if st, ok := ui.ParseUIStyle(v); ok {
				s.style = st
			}
		}
		if v, ok := obj["icons"].(string); ok {
			if _, ok := ui.ParseIconLevel(v); ok {
				iconSetting = v
			}
		}
		switch v := obj["animations"].(type) {
		case bool:
			s.animations = v
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "false", "off", "no":
				s.animations = false
			case "true", "on", "yes":
				s.animations = true
			}
		}
		if v, ok := obj["sidebar"].(string); ok {
			if m, ok := ui.ParseSidebarMode(v); ok {
				s.sidebar = m
			}
		}
		if v, ok := obj["theme"].(string); ok && strings.TrimSpace(v) != "" {
			s.theme = strings.TrimSpace(v)
		}
	}
	s.icons = ui.ResolveIconLevel(iconSetting, envGetenv(env)(ui.IconsEnv))
	return s
}

// themeChoice is how the session picks its palette: a fixed theme, or a
// dark/light pair chosen from the terminal background.
type themeChoice struct {
	name      string // the theme to start with
	autoDark  string // with autoLight: pick by the terminal background
	autoLight string
}

// chooseTheme resolves the palette from `ui.theme` (if set) or `theme`. A
// concrete theme name is used as-is; "auto", unset and unknown names follow
// the terminal background — between the Prompty pair (prompty/prompty-light)
// in the modern style, the classic pair (dark/light) for "auto" otherwise,
// and plain dark for an unset/unknown theme in the older styles.
func chooseTheme(setting, uiTheme string, style ui.UIStyle) themeChoice {
	name := setting
	if uiTheme != "" {
		name = uiTheme
	}
	for _, known := range themes.ThemeNames {
		if name == known {
			return themeChoice{name: name}
		}
	}
	if style == ui.StyleModern {
		return themeChoice{name: themes.ThemePrompty, autoDark: themes.ThemePrompty, autoLight: themes.ThemePromptyLight}
	}
	if name == themes.ThemeAuto {
		return themeChoice{name: themes.ThemeDark, autoDark: themes.ThemeDark, autoLight: themes.ThemeLight}
	}
	return themeChoice{name: themes.ThemeDark}
}

// noColorEnv reports the NO_COLOR convention (any non-empty value).
func noColorEnv(env *Env) bool { return envGetenv(env)("NO_COLOR") != "" }

// persistUISetting saves a UI setting changed from the command palette or the
// theme picker to the user settings (`ui.<key>`), so it applies in every
// project. Best-effort: a failed write keeps the change for this session.
func persistUISetting(env *Env, key, value string) {
	key = strings.TrimPrefix(key, "ui.")
	var v any = value
	if key == "animations" {
		v = value == "on" || value == "true"
	}
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser)
	if !ok {
		return
	}
	obj, err := readLocalSettingsObject(path)
	if err != nil {
		return
	}
	sub, _ := obj["ui"].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
	}
	sub[key] = v
	obj["ui"] = sub
	_ = writeLocalSettingsObject(path, obj)
}

// footerCwd shortens the working directory for the info footer: the home
// directory becomes "~".
func footerCwd(env *Env, cwd string) string {
	home := envGetenv(env)("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" && home != string(filepath.Separator) {
		if cwd == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(cwd, home+string(filepath.Separator)); ok {
			return "~" + string(filepath.Separator) + rest
		}
	}
	return cwd
}

// footerInfo returns the info-footer message: the shortened cwd and the
// current git branch (a bounded, best-effort probe; "" when not a repository).
// Call it off the UI goroutine.
func footerInfo(env *Env) ui.SetFooterInfoMsg {
	cwd := sessionCwd(env)
	return ui.SetFooterInfoMsg{Cwd: footerCwd(env, cwd), Branch: cheapGitBranch(context.Background(), cwd)}
}

// contextPercent is the share of the active model's context window the
// conversation uses: the last measured response plus an estimate of what came
// after it, or — right after /compact, /clear or /resume dropped that
// measurement — an estimate of the current history; -1 when unknown (nothing
// to show yet, or no known window). See contextSnapshot.
func (d *turnDriver) contextPercent() int {
	pct, _ := d.contextSnapshot()
	return pct
}

// contextUsage is the context usage in tokens for the sidebar gauge caption
// (Window 0 when unknown).
func (d *turnDriver) contextUsage() ui.SetContextUsageMsg {
	_, u := d.contextSnapshot()
	return u
}

// mcpServersMsg is the MCP server status for the sidebar.
func mcpServersMsg(h *mcphost.Host) ui.SetMCPServersMsg {
	if h == nil {
		return ui.SetMCPServersMsg{}
	}
	var out []ui.MCPServer
	for _, st := range h.Status() {
		out = append(out, ui.MCPServer{Name: st.Name, State: string(st.State), Tools: st.Tools})
	}
	return ui.SetMCPServersMsg{Servers: out}
}
