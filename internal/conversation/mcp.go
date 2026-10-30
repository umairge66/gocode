package conversation

import (
	"context"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/commands"
)

// MCPPromptExpander expands MCP prompt slash commands. name is the command
// name without the slash ("mcp__server__prompt"); ok is false when no server
// offers that prompt. *mcphost.Host implements it.
type MCPPromptExpander interface {
	ExpandPromptCommand(ctx context.Context, name, rawArgs string) (blocks []agent.ContentBlock, ok bool, err error)
}

// expandMCPPrompt expands userInput when it is an MCP prompt command. isMCP is
// false for every other input (which then takes the custom-command path).
func (c *Conversation) expandMCPPrompt(ctx context.Context, userInput string) (blocks []agent.ContentBlock, isMCP bool, err error) {
	if c.MCPPrompts == nil || !commands.IsSlashCommand(userInput) {
		return nil, false, nil
	}
	parsed, ok := commands.ParseInput(userInput)
	if !ok || !strings.HasPrefix(parsed.Name, "mcp__") {
		return nil, false, nil
	}
	blocks, ok, err = c.MCPPrompts.ExpandPromptCommand(ctx, parsed.Name, parsed.Args)
	if !ok {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("/%s: %w", parsed.Name, err)
	}
	if len(blocks) == 0 {
		// A prompt with no content: send the typed command rather than an
		// empty user turn the API would reject.
		blocks = []agent.ContentBlock{agent.TextBlock(userInput)}
	}
	return blocks, true, nil
}

// MCPPromptCommand describes one MCP prompt command for listings (the
// slash-command menu, /help).
type MCPPromptCommand struct {
	// Name is the command name without the slash ("mcp__server__prompt").
	Name        string
	Description string
	// ArgNames are the prompt's argument names, in order (the menu's hint).
	ArgNames []string
}

// MCPPromptLister is the optional listing side of an MCPPromptExpander.
type MCPPromptLister interface {
	ListPromptCommands() []MCPPromptCommand
}

// MCPPromptCommands lists the MCP prompt commands currently available (nil
// when MCPPrompts is unset or cannot list).
func (c *Conversation) MCPPromptCommands() []MCPPromptCommand {
	l, ok := c.MCPPrompts.(MCPPromptLister)
	if !ok {
		return nil
	}
	return l.ListPromptCommands()
}
