package ui

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// The /login key entry in the modern style is a Huh form field: a password
// Input themed from the active palette and drawn inside the dialog's floating
// panel. The Model owns the field while the secret dialog is open, feeds it
// the keys, mirrors its value into the dialog (so the host contract — the
// value delivered on Enter, the masked length — is unchanged) and puts its
// rendered view in the ViewState for the renderer.
//
// The other dialogs (permission prompts with their feedback stage,
// AskUserQuestion's marked options, the pickers) keep the dialog model the
// host drives directly; Huh's form lifecycle (fields advancing through
// commands) does not fit their synchronous contracts.

// huhTheme builds a Huh theme from the modern style's palette: no frame of its
// own (the dialog panel is the frame), the accent prompt and caret, the text
// and placeholder colors.
func huhTheme(ms mstyle) huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase(isDark)
		t.Focused.Base = lipgloss.NewStyle()
		t.Focused.Title = ms.bold
		t.Focused.Description = ms.muted
		t.Focused.TextInput.Prompt = ms.accent.Bold(true)
		t.Focused.TextInput.Text = ms.text
		t.Focused.TextInput.Placeholder = ms.faint
		t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(ms.accentC)
		t.Blurred = t.Focused
		return t
	})
}

// openSecretField starts the Huh password field for a secret dialog.
func (m *Model) openSecretField(lr LipglossRenderer, d *Dialog) {
	ms := lr.mstyles()
	f := huh.NewInput().
		EchoMode(huh.EchoModePassword).
		Prompt(ms.ic.Prompt + " ").
		Placeholder("paste the key, then enter")
	if d.Reveal {
		// A plain text field: the text is shown and starts from the
		// dialog's initial value.
		initial := d.secret
		f = huh.NewInput().
			EchoMode(huh.EchoModeNormal).
			Prompt(ms.ic.Prompt + " ").
			Value(&initial)
	}
	f.WithTheme(huhTheme(ms))
	f.WithWidth(secretFieldWidth(lr, m.state, d))
	_ = f.Focus() // the caret does not blink: no blink ticks while waiting
	m.secretField = f
	m.syncSecretField()
}

// secretFieldWidth is the field's width inside the dialog panel.
func secretFieldWidth(lr LipglossRenderer, vs ViewState, d *Dialog) int {
	return max(modalWidth(lr.modernLayout(vs).mainW, d)-6, 8)
}

// syncSecretField mirrors the field's value into the dialog and its view into
// the ViewState.
func (m *Model) syncSecretField() {
	d := m.state.Dialog
	if m.secretField == nil || d == nil || d.Kind != DialogSecret {
		m.secretField = nil
		m.state.DialogField = ""
		return
	}
	if v, ok := m.secretField.GetValue().(string); ok {
		d.secret = v
	}
	if lr, ok := m.modernRenderer(); ok {
		m.secretField.WithWidth(secretFieldWidth(lr, m.state, d))
	}
	m.state.DialogField = m.secretField.View()
}

// secretFieldKey feeds a key to the Huh field (Enter and Esc are handled by
// the dialog itself).
func (m *Model) secretFieldKey(msg any) {
	if m.secretField == nil {
		return
	}
	f, _ := m.secretField.Update(msg)
	if in, ok := f.(*huh.Input); ok {
		m.secretField = in
	}
	m.syncSecretField()
}
