package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// /export [filename]: write the conversation as plain text. With a file name
// it is saved straight away (".txt" is added when the name has no
// extension; a relative name is resolved against the working directory).
// Without one a dialog offers "Copy to clipboard" (through the terminal, OSC
// 52) or "Save to file" with an editable default name built from the date
// and the first prompt.

// Values of the export-method picker.
const (
	exportClipboard = "clipboard"
	exportFile      = "file"
)

// systemReminderRE matches the system-reminder blocks the session adds to
// user turns; they are not part of what the user typed.
var systemReminderRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// visibleText returns a text block without its system reminders.
func visibleText(s string) string {
	return strings.TrimSpace(systemReminderRE.ReplaceAllString(s, ""))
}

// exportTranscript renders the conversation as plain text: the user's
// prompts ("> "), the assistant's replies ("⏺ "), each tool call as
// "⏺ Verb(args)" with its result indented under "⎿".
func exportTranscript(hist []agent.Message) string {
	var b strings.Builder
	calls := map[string]agent.ContentBlock{}
	section := func(s string) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
		b.WriteString("\n")
	}
	for _, m := range hist {
		for _, blk := range m.Content {
			switch blk.Type {
			case agent.BlockText:
				t := visibleText(blk.Text)
				if t == "" {
					continue
				}
				if m.Role == agent.RoleUser {
					if st, ok := conversation.ParseShellText(t); ok {
						switch st.Kind {
						case conversation.ShellTextInput:
							section("! " + st.Command)
						case conversation.ShellTextOutput:
							res := joinNonEmpty(st.Stdout, st.Stderr)
							if res == "" {
								res = "(No output)"
							}
							b.WriteString(prefixLines("  ⎿  ", "     ", res))
							b.WriteString("\n")
						case conversation.ShellTextInterrupted:
							b.WriteString("  ⎿  Interrupted\n")
						}
						continue
					}
					section(prefixLines("> ", "  ", t))
				} else {
					section(prefixLines("⏺ ", "  ", t))
				}
			case agent.BlockToolUse:
				calls[blk.ID] = blk
				verb, args := toolHeader(blk.Name, blk.Input)
				section("⏺ " + verb + "(" + args + ")")
			case agent.BlockToolResult:
				call := calls[blk.ToolUseID]
				res := strings.TrimRight(displayToolResult(call.Name, call.Input, blk.Content), "\n")
				if strings.TrimSpace(res) == "" {
					res = "(no output)"
				}
				b.WriteString(prefixLines("  ⎿  ", "     ", res))
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// prefixLines prefixes the first line of s with first and the rest with rest.
func prefixLines(first, rest, s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// firstPromptText is the text of the first user prompt, for the file name.
func firstPromptText(hist []agent.Message) string {
	for _, m := range hist {
		if m.Role != agent.RoleUser {
			continue
		}
		var last string
		for _, blk := range m.Content {
			if blk.Type == agent.BlockText {
				t := visibleText(blk.Text)
				if st, ok := conversation.ParseShellText(t); ok {
					if st.Kind == conversation.ShellTextInput && last == "" {
						last = "! " + st.Command
					}
					continue
				}
				if t != "" {
					last = t
				}
			}
		}
		if last == "" {
			continue
		}
		if r := []rune(last); len(r) > 50 {
			last = string(r[:49])
		}
		return last
	}
	return ""
}

var (
	slugDropRE  = regexp.MustCompile(`[^a-z0-9\s-]`)
	slugSpaceRE = regexp.MustCompile(`\s+`)
	slugDashRE  = regexp.MustCompile(`-+`)
)

// exportSlug turns a prompt into a file-name fragment.
func exportSlug(s string) string {
	s = slugDropRE.ReplaceAllString(strings.ToLower(s), "")
	s = slugSpaceRE.ReplaceAllString(s, "-")
	s = slugDashRE.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// exportFileName is the default export file name:
// "2026-01-02-150405-<first-prompt-slug>.txt", or
// "conversation-2026-01-02-150405.txt" when there is no prompt text.
func exportFileName(hist []agent.Message, now time.Time) string {
	ts := now.Format("2006-01-02-150405")
	if slug := exportSlug(firstPromptText(hist)); slug != "" {
		return ts + "-" + slug + ".txt"
	}
	return "conversation-" + ts + ".txt"
}

// exportPath resolves a typed file name: ".txt" is added when it has no
// extension, and a relative name is taken from cwd.
func exportPath(cwd, name string) string {
	name = strings.TrimSpace(name)
	if filepath.Ext(name) == "" {
		name += ".txt"
	}
	if strings.HasPrefix(name, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			name = filepath.Join(home, name[2:])
		}
	}
	if !filepath.IsAbs(name) && cwd != "" {
		name = filepath.Join(cwd, name)
	}
	return name
}

// writeExport writes content to the resolved path, creating its directory.
func writeExport(cwd, name, content string) (string, error) {
	path := exportPath(cwd, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}
	return path, os.WriteFile(path, []byte(content), 0o644)
}

// export runs /export.
func (d *interactiveDialogs) export(args string) {
	hist := d.conv.History
	if len(hist) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "Nothing to export: the conversation is empty."})
		return
	}
	content := exportTranscript(hist)
	cwd := settingsContext(d.env).Cwd
	if name := strings.TrimSpace(args); name != "" {
		d.sendMsg(ui.AppendSystemMsg{Text: exportResult(writeExport(cwd, name, content))})
		return
	}
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	defaultName := exportFileName(hist, now())
	for {
		items := []ui.ListItem{
			{Label: "Copy to clipboard", Value: exportClipboard, Detail: "Copy the conversation to your system clipboard"},
			{Label: "Save to file", Value: exportFile, Detail: "Save the conversation to a file in the current directory"},
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog("Export conversation", items, 0, "Select export method"))
		if !ok || r.cancelled {
			d.sendMsg(ui.AppendSystemMsg{Text: "Export cancelled"})
			return
		}
		switch firstChosen(r.values) {
		case exportClipboard:
			d.sendMsg(ui.CopyToClipboardMsg{Text: content})
			d.sendMsg(ui.AppendSystemMsg{Text: "Conversation copied to clipboard"})
			return
		case exportFile:
			in, ok := d.openAndWait(context.Background(), ui.TextInputDialog("Export conversation", defaultName, "Enter filename:"))
			if !ok {
				return
			}
			if in.cancelled {
				continue // back to the method choice
			}
			name := strings.TrimSpace(firstChosen(in.values))
			if name == "" {
				name = defaultName
			}
			d.sendMsg(ui.AppendSystemMsg{Text: exportResult(writeExport(cwd, name, content))})
			return
		default:
			return
		}
	}
}

// exportResult is the message for a finished file export.
func exportResult(path string, err error) string {
	if err != nil {
		return fmt.Sprintf("Failed to export conversation: %v", err)
	}
	return "Conversation exported to: " + path
}
