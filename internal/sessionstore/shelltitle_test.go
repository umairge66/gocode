package sessionstore

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// A session started from shell mode is titled "! <command>": the
// <bash-input> tag and the command's output never appear in the title,
// whether or not the model was asked to respond.
func TestShellCommandTitle(t *testing.T) {
	const caveat = "<local-command-caveat>The command below was run directly in gocode, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"
	input := agent.TextBlock("<bash-input>ls src && cat notes.txt</bash-input>")
	output := agent.TextBlock("<bash-stdout>calc.py\ntodo: add div</bash-stdout><bash-stderr></bash-stderr>")
	st := New(t.TempDir())
	cwd := "/work/shell"

	respond, _ := st.Create(cwd, "respond")
	_ = respond.AppendMessages([]agent.Message{agent.UserMessage(input, output), asstMsg("src holds calc.py")})
	quiet, _ := st.Create(cwd, "quiet")
	_ = quiet.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock(caveat), input, output)})
	_ = quiet.AppendMessages([]agent.Message{userMsg("add div"), asstMsg("ok")})
	reminder, _ := st.Create(cwd, "reminder")
	_ = reminder.AppendMessages([]agent.Message{agent.UserMessage(agent.TextBlock("<system-reminder>\nnote\n</system-reminder>"), agent.TextBlock("fix the tests")), asstMsg("ok")})

	metas, _ := st.List(cwd)
	titles := map[string]string{}
	for _, m := range metas {
		titles[m.ID] = m.Title
	}
	for id, want := range map[string]string{
		"respond":  "! ls src && cat notes.txt",
		"quiet":    "! ls src && cat notes.txt",
		"reminder": "fix the tests",
	} {
		if titles[id] != want {
			t.Errorf("%s title = %q, want %q", id, titles[id], want)
		}
		if strings.Contains(titles[id], "<") {
			t.Errorf("%s title shows a tag: %q", id, titles[id])
		}
	}
}
