package ui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updatePlainGolden rewrites testdata/plain_render.golden from the current
// PlainRenderer output (go test -run TestPlainRendererGolden -update-plain).
var updatePlainGolden = flag.Bool("update-plain", false, "rewrite the PlainRenderer golden file")

// plainGoldenState is a ViewState exercising every transcript kind and the
// bottom region, used to pin the PlainRenderer output byte for byte.
func plainGoldenState() ViewState {
	vs := New(72)
	vs = Reduce(vs, SetBanner{Banner: &Banner{AppName: "gocode", Version: "1.0.0", Cwd: "/home/u/proj", Provider: "openrouter", Model: "opus-5.5"}})
	vs = AppendUser(vs, "fix the failing test in parser.go")
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{ID: "t1", Name: "Read", Args: "internal/parser/parser.go", ResultText: "Read 40 lines", FullText: "a\nb", Ended: true}})
	vs = Reduce(vs, AppendToolCall{Tool: ToolCall{ID: "t2", Name: "Bash", Args: "go test ./...", ResultText: "FAIL parser_test.go:12\nexit status 1", IsError: true, Ended: true}})
	vs = Reduce(vs, AppendDiff{Diff: *ParseUnifiedDiff("parser.go", "@@ -1,3 +1,3 @@\n package parser\n-old line\n+new line\n func Parse() {}")})
	vs = AppendAssistant(vs, "The test fails because the **tokenizer** drops the last rune.")
	vs = Reduce(vs, AppendMessage{Message: Message{Role: RoleSystem, Text: "Auto-compacted."}})
	vs = Reduce(vs, AppendMessage{Message: Message{Role: RoleError, Text: "boom"}})
	vs = Reduce(vs, SetStatus{Status: Status{Kind: StatusThinking, Label: "Thinking", Interrupt: "esc to interrupt", Elapsed: 12, Tokens: 1200}})
	vs = Reduce(vs, SetFooterTokens{In: 1200, Out: 340})
	vs = Reduce(vs, SetPermissionMode{Mode: ModeAuto})
	return vs
}

// TestPlainRendererGolden pins the -p / non-TTY PlainRenderer output: the card
// style is a full-screen LipglossRenderer feature and must never leak into it.
func TestPlainRendererGolden(t *testing.T) {
	got := strings.Join(PlainRenderer{}.Render(plainGoldenState()), "\n") + "\n"
	path := filepath.Join("testdata", "plain_render.golden")
	if *updatePlainGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("PlainRenderer output changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
