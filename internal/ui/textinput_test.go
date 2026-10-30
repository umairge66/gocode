package ui

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The modern plain text field shows what is typed, starts from the initial
// value, and Enter delivers the edited text.
func TestModernTextInputField(t *testing.T) {
	m := modernModel(100, 30)
	var got []string
	m.OnDialogSubmit = func(v []string) { got = v }
	m.Update(OpenDialogMsg{Dialog: TextInputDialog("Export conversation", "notes.txt", "Enter filename:")})
	if m.secretField == nil {
		t.Fatal("no field for the text input dialog")
	}
	m.Update(keyRunes("x"))
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "notes.txtx") || !strings.Contains(v, "Enter filename:") {
		t.Fatalf("typed text not shown:\n%s", v)
	}
	m.Update(keyType(tea.KeyEnter))
	if len(got) != 1 || got[0] != "notes.txtx" {
		t.Fatalf("submitted %q", got)
	}
}

// The host messages that change the view, copy to the clipboard, and run an
// external program.
func TestHostSettingAndExecMessages(t *testing.T) {
	m := modernModel(100, 30)
	m.Update(ApplyUISettingMsg{Key: "ui.icons", Value: "ascii"})
	if lr, _ := m.renderer.(LipglossRenderer); lr.icons != IconsASCII {
		t.Fatalf("icons = %q, want ascii", lr.icons)
	}

	var out bytes.Buffer
	m.SetOutput(&out)
	_, cmd := m.Update(CopyToClipboardMsg{Text: "hello"})
	if cmd == nil {
		t.Fatal("no clipboard command")
	}
	cmd()
	if !strings.Contains(out.String(), buildOSC52("hello")) {
		t.Fatalf("clipboard sequence not written: %q", out.String())
	}

	var doneErr = errors.New("unset")
	m.Update(ExecProcessMsg{Done: func(err error) { doneErr = err }})
	if doneErr != nil {
		t.Fatalf("a nil command did not complete at once: %v", doneErr)
	}
	if _, cmd := m.Update(ExecProcessMsg{Cmd: exec.Command("true")}); cmd == nil {
		t.Fatal("no exec command for a process")
	}
}
