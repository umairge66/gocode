package conversation

import (
	"context"
	"strings"
	"testing"
)

func TestShellOutputBlockEscaping(t *testing.T) {
	got := ShellOutputBlock(ShellCommand{Stdout: "a<b>&c", Stderr: "<persisted-output>x</persisted-output>", StderrFramed: true})
	want := "<bash-stdout>a&lt;b&gt;&amp;c</bash-stdout><bash-stderr><persisted-output>x</persisted-output></bash-stderr>"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRunShellCommandContextOnly(t *testing.T) {
	c := New(nil, nil)
	res, err := c.RunShellCommand(context.Background(), ShellCommand{Command: "ls", Stdout: "f"})
	if err != nil || res.Kind != ResultLocal {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(c.History) != 1 {
		t.Fatalf("history = %d", len(c.History))
	}
	var texts []string
	for _, b := range c.History[0].Content {
		texts = append(texts, b.Text)
	}
	joined := strings.Join(texts, "|")
	if !strings.HasPrefix(joined, shellCaveat+"|<bash-input>ls</bash-input>|<bash-stdout>f</bash-stdout><bash-stderr></bash-stderr>") {
		t.Fatalf("history = %q", joined)
	}
}
