package ui

// DefaultBannerHint is the standard hint line shown in a fresh-session welcome
// banner.
const DefaultBannerHint = "/help for commands, /login to set a key"

// Banner is the welcome box shown at the top of a fresh session: the app name
// and version, the working directory, the active provider and model, a hint
// line, and an optional warning (e.g. a missing API key). The host fills the
// fields and pushes it in; the UI only renders it.
//
// It PERSISTS at the top of scrollback for the whole session: the welcome
// box renders as a sibling ABOVE the transcript list, NOT gated on an
// empty transcript — so it heads the scrollback and scrolls up as history grows
// rather than vanishing after the first message. Both renderers draw it as the
// first transcript element whenever it is set; pass nil to clear it.
type Banner struct {
	// AppName is the product name (e.g. "gocode"). "" renders as "gocode".
	AppName string
	// Version is the installed version string shown after the name (e.g.
	// "1.4.0"). "" omits the version suffix.
	Version string
	// Cwd is the working directory shown on its own row. "" omits the row.
	Cwd string
	// Provider is the active model provider (e.g. "anthropic"). "" omits the
	// provider from the model row.
	Provider string
	// Model is the active model id/name shown on the model row. "" omits the
	// model row entirely.
	Model string
	// Hint is the hint line. "" falls back to DefaultBannerHint.
	Hint string
	// Greeting is the short message the start screen shows under the cwd in
	// the modern and cards styles (see Greetings). "" omits it.
	Greeting string
	// Warning is an optional caution line rendered in the warning color (e.g.
	// "No API key for anthropic — run /login"). "" omits the line.
	Warning string
}

// Lines returns the banner's content rows (without any border/frame), in
// display order. The renderers frame these: the lipgloss renderer in a rounded
// box with a colored warning row, the plain renderer in an ASCII frame. The
// warning row, when present, is always the last line so a renderer can style it.
func (b *Banner) Lines() []string {
	if b == nil {
		return nil
	}
	name := b.AppName
	if name == "" {
		name = "gocode"
	}
	// Title row: "gocode" (bold) + " v<version>"
	// (dim). The sigil ✻ is drawn by the renderer as the mascot column, not part of
	// this string. The welcome box has no "Welcome to …" text.
	title := name
	if b.Version != "" {
		title += " " + versionLabel(b.Version)
	}
	out := []string{title}
	// Model/billing row ("model · billing"), then the cwd row ("@agent · cwd").
	// Kept on separate dim rows.
	if b.Model != "" {
		model := b.Model
		if b.Provider != "" {
			model = b.Provider + " · " + b.Model
		}
		out = append(out, model)
	}
	if b.Cwd != "" {
		out = append(out, b.Cwd)
	}
	hint := b.Hint
	if hint == "" {
		hint = DefaultBannerHint
	}
	out = append(out, hint)
	if b.Warning != "" {
		out = append(out, b.Warning)
	}
	return out
}

// HasWarning reports whether the banner carries a warning row (the last line).
func (b *Banner) HasWarning() bool { return b != nil && b.Warning != "" }
