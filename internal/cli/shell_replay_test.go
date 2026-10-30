package cli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/ui"
)

// shellHistory is a session with a shell-mode command the model responded
// to, then one recorded only as context (and a failing one).
func shellHistory() []agent.Message {
	sc := conversation.ShellCommand{Command: "ls src && cat notes.txt", Stdout: "calc.py\n<div> & co"}
	quiet := conversation.ShellCommand{Command: "false", Stderr: "Exit code 1"}
	return []agent.Message{
		agent.UserMessage(agent.TextBlock(conversation.ShellInputBlock(sc.Command)), agent.TextBlock(conversation.ShellOutputBlock(sc))),
		agent.AssistantMessage(agent.TextBlock("src holds calc.py")),
		agent.UserMessage(
			agent.TextBlock("<local-command-caveat>The command below was run directly in gocode, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"),
			agent.TextBlock(conversation.ShellInputBlock(quiet.Command)),
			agent.TextBlock(conversation.ShellOutputBlock(quiet)),
		),
	}
}

// A resumed shell-mode exchange replays as the "! <command>" block with its
// output, as it showed when it ran, never as raw tags.
func TestReplayShellExchange(t *testing.T) {
	var got []tea.Msg
	replayHistory(func(m tea.Msg) { got = append(got, m) }, shellHistory())

	var calls []ui.ToolCall
	for _, m := range got {
		switch v := m.(type) {
		case ui.AppendUserMsg:
			t.Fatalf("shell exchange replayed as a user message: %q", v.Text)
		case ui.AppendToolCallMsg:
			calls = append(calls, v.Tool)
		case ui.UpdateToolCallMsg:
			calls = append(calls, v.Tool)
		}
	}
	if len(calls) != 4 {
		t.Fatalf("tool call messages = %+v", calls)
	}
	first, firstDone := calls[0], calls[1]
	if first.Name != "!" || !first.Shell || first.Args != "ls src && cat notes.txt" {
		t.Fatalf("shell block = %+v", first)
	}
	if firstDone.ID != first.ID || firstDone.ResultText != "calc.py\n<div> & co" || firstDone.IsError {
		t.Fatalf("shell result = %+v", firstDone)
	}
	second, secondDone := calls[2], calls[3]
	if second.Args != "false" || secondDone.ResultText != "Exit code 1" || !secondDone.IsError || second.ID == first.ID {
		t.Fatalf("failed command = %+v / %+v", second, secondDone)
	}
}

// An export shows a shell-mode exchange as "! <command>" with its output.
func TestExportShellExchange(t *testing.T) {
	out := exportTranscript(shellHistory())
	if strings.Contains(out, "<bash-") || strings.Contains(out, "local-command-caveat") {
		t.Fatalf("export shows raw tags:\n%s", out)
	}
	for _, want := range []string{"! ls src && cat notes.txt\n  ⎿  calc.py\n     <div> & co", "! false\n  ⎿  Exit code 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("export misses %q:\n%s", want, out)
		}
	}
	if got := firstPromptText(shellHistory()); got != "! ls src && cat notes.txt" {
		t.Fatalf("first prompt = %q", got)
	}
}
