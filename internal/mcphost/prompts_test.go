package mcphost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

func promptServer() *fakeServer {
	srv := echoServer()
	srv.prompts = []mcp.Prompt{
		{Name: "review", Description: "Review a file", Arguments: []mcp.PromptArgument{{Name: "file", Required: true}, {Name: "focus"}}},
		{Name: "hello"},
	}
	srv.getPrompt = func(p mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
		args, _ := json.Marshal(p.Arguments)
		return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
			{Role: "user", Content: mcp.ContentBlock{Type: "text", Text: p.Name + " " + string(args)}},
			{Role: "user", Content: mcp.ContentBlock{Type: "resource", Resource: json.RawMessage(`{"uri":"file:///a.go","text":"package a"}`)}},
			{Role: "user", Content: mcp.ContentBlock{Type: "resource_link", Name: "spec", URI: "https://x/spec", Description: "the spec"}},
			{Role: "user", Content: mcp.ContentBlock{Type: "image", Data: "AAAA", MimeType: "image/png"}},
		}}, nil
	}
	return srv
}

func TestHostPromptCommands(t *testing.T) {
	h := Start(context.Background(), map[string]mcp.ServerConfig{"my srv": {Command: "x"}, "plain": {Command: "y"}},
		Options{Transport: multiFactory(map[string]*fakeServer{"my srv": promptServer(), "plain": echoServer()}), Getenv: noEnv})
	defer h.Close()

	cmds := h.PromptCommands()
	if len(cmds) != 2 || cmds[0].Name != "mcp__my_srv__review" || cmds[1].Name != "mcp__my_srv__hello" {
		t.Fatalf("commands = %+v", cmds)
	}
	if cmds[0].UserFacingName() != "my srv:review (MCP)" || strings.Join(cmds[0].ArgNames, ",") != "file,focus" {
		t.Fatalf("review = %+v", cmds[0])
	}

	// Missing required argument → the usage error, no request.
	if _, err := h.GetPrompt(context.Background(), cmds[0], "  "); err == nil ||
		err.Error() != "Missing required argument: file. Usage: /mcp__my_srv__review file focus" {
		t.Fatalf("missing-arg err = %v", err)
	}

	blocks, err := h.GetPrompt(context.Background(), cmds[0], "a.go  bugs extra")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, b := range blocks {
		texts = append(texts, b.Text)
	}
	want := []string{
		`review {"file":"a.go","focus":"bugs"}`,
		"[Resource from my srv at file:///a.go] package a",
		"[Resource link: spec] https://x/spec (the spec)",
		"[Image from my srv] Binary content (image/png, 3 bytes) could not be saved to disk: not supported",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("blocks =\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if _, ok := h.LookupPromptCommand("mcp__my_srv__hello"); !ok {
		t.Fatal("lookup failed")
	}
}

func TestPromptArgsZip(t *testing.T) {
	c := PromptCommand{Name: "mcp__s__p", Arguments: []mcp.PromptArgument{{Name: "a"}, {Name: "b", Required: true}, {Name: "c", Required: true}}, ArgNames: []string{"a", "b", "c"}}
	if _, err := c.PromptArgs("x"); err == nil || !strings.HasPrefix(err.Error(), "Missing required arguments: b, c.") {
		t.Fatalf("err = %v", err)
	}
	args, err := c.PromptArgs("1 2 3")
	if err != nil || args["a"] != "1" || args["c"] != "3" {
		t.Fatalf("args = %v %v", args, err)
	}
	if args, _ := (PromptCommand{}).PromptArgs(""); args != nil {
		t.Fatalf("no-arg prompt args = %v", args)
	}
}
