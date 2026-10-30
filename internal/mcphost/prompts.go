package mcphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/mcp"
)

// MCP prompts as slash commands: every prompt a connected server lists
// becomes a `/mcp__<server>__<prompt>` command. Running it fetches the prompt
// (prompts/get) with the typed arguments and sends the returned messages to
// the model as the user turn.

// maxPromptPages bounds prompts/list pagination (same bound as tools).
const maxPromptPages = maxToolPages

// listPrompts fetches a server's prompt catalog. A server without the prompts
// capability, or a failed listing, yields none.
func listPrompts(ctx context.Context, client *mcp.Client, caps mcp.ServerCapabilities) []mcp.Prompt {
	if caps.Prompts == nil {
		return nil
	}
	var out []mcp.Prompt
	cursor := ""
	for range maxPromptPages {
		var p *mcp.ListPromptsParams
		if cursor != "" {
			p = &mcp.ListPromptsParams{Cursor: cursor}
		}
		page, err := client.ListPrompts(ctx, p)
		if err != nil {
			return nil
		}
		out = append(out, page.Prompts...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return out
}

// PromptCommand is a slash command backed by an MCP prompt.
type PromptCommand struct {
	// Name is the command name without the leading slash:
	// "mcp__" + NormalizeName(server) + "__" + prompt name.
	Name string
	// Server is the server's configured name; Prompt is the prompt's name.
	Server string
	Prompt string
	// Description is the prompt's description.
	Description string
	// Arguments are the prompt's declared arguments, in order; ArgNames their
	// names.
	Arguments []mcp.PromptArgument
	ArgNames  []string
}

// UserFacingName is how listings show the command: "<server>:<prompt> (MCP)".
func (p PromptCommand) UserFacingName() string {
	return p.Server + ":" + p.Prompt + " (MCP)"
}

// PromptCommandName builds the slash command name of a server's prompt.
func PromptCommandName(server, prompt string) string {
	return ToolPrefix + NormalizeName(server) + "__" + prompt
}

// PromptCommands lists the prompt commands of the servers that are connected
// (or reconnecting / awaiting sign-in, so running one explains the state), in
// server order. A name that collides with an earlier one is skipped.
func (h *Host) PromptCommands() []PromptCommand {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []PromptCommand
	seen := map[string]bool{}
	for _, name := range h.order {
		s := h.servers[name]
		switch s.status.State {
		case StateConnected, StateReconnecting, StateNeedsAuth:
		default:
			continue
		}
		for _, p := range s.prompts {
			cmd := PromptCommand{
				Name:        PromptCommandName(name, p.Name),
				Server:      name,
				Prompt:      p.Name,
				Description: p.Description,
				Arguments:   append([]mcp.PromptArgument(nil), p.Arguments...),
			}
			if seen[cmd.Name] {
				continue
			}
			seen[cmd.Name] = true
			for _, a := range p.Arguments {
				cmd.ArgNames = append(cmd.ArgNames, a.Name)
			}
			out = append(out, cmd)
		}
	}
	return out
}

// LookupPromptCommand resolves a command name (without the slash) to its
// prompt command.
func (h *Host) LookupPromptCommand(name string) (PromptCommand, bool) {
	for _, c := range h.PromptCommands() {
		if c.Name == name {
			return c, true
		}
	}
	return PromptCommand{}, false
}

// PromptArgs maps the typed argument string onto the prompt's arguments: the
// string is split on whitespace and the values are zipped onto the argument
// names in order (extra values are ignored). A missing required argument is
// an error carrying the usage line.
func (p PromptCommand) PromptArgs(raw string) (map[string]string, error) {
	fields := strings.Fields(raw)
	var missing []string
	for i, a := range p.Arguments {
		if a.Required && i >= len(fields) {
			missing = append(missing, a.Name)
		}
	}
	if len(missing) > 0 {
		noun := "argument"
		if len(missing) != 1 {
			noun = "arguments"
		}
		return nil, fmt.Errorf("Missing required %s: %s. Usage: /%s %s", noun, strings.Join(missing, ", "), p.Name, strings.Join(p.ArgNames, " "))
	}
	var args map[string]string
	for i, name := range p.ArgNames {
		if i >= len(fields) {
			break
		}
		if args == nil {
			args = map[string]string{}
		}
		args[name] = fields[i]
	}
	return args, nil
}

// GetPrompt runs a prompt command: it checks the arguments, fetches the
// prompt from its server and converts every returned message to content
// blocks for the user turn.
func (h *Host) GetPrompt(ctx context.Context, cmd PromptCommand, rawArgs string) ([]agent.ContentBlock, error) {
	args, err := cmd.PromptArgs(rawArgs)
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	s := h.servers[cmd.Server]
	var client *mcp.Client
	var state State
	if s != nil {
		client, state = s.client, s.status.State
	}
	closed := h.closed
	h.mu.RUnlock()
	switch {
	case closed:
		return nil, errors.New("mcp host is closed")
	case s == nil:
		return nil, fmt.Errorf("unknown MCP server %q", cmd.Server)
	case client == nil && state == StateNeedsAuth:
		return nil, needsAuthCallErr(cmd.Server)
	case client == nil:
		return nil, fmt.Errorf("MCP server %q is not connected", cmd.Server)
	}
	res, err := client.GetPrompt(ctx, mcp.GetPromptParams{Name: cmd.Prompt, Arguments: args})
	if err != nil {
		if errors.Is(err, mcp.ErrNeedsAuth) {
			h.markNeedsAuth(cmd.Server, client)
			return nil, needsAuthCallErr(cmd.Server)
		}
		return nil, fmt.Errorf("Error running command '%s': %w", cmd.Prompt, err)
	}
	var out []agent.ContentBlock
	for _, m := range res.Messages {
		out = append(out, promptContentBlocks(m.Content, cmd.Server)...)
	}
	return out, nil
}

// promptContentBlocks converts one MCP content block of a prompt message to
// user-turn content.
func promptContentBlocks(b mcp.ContentBlock, server string) []agent.ContentBlock {
	text := func(s string) []agent.ContentBlock { return []agent.ContentBlock{agent.TextBlock(s)} }
	binaryNote := func(prefix, mime string, size int) string {
		if mime == "" {
			mime = "unknown type"
		}
		return fmt.Sprintf("%sBinary content (%s, %d bytes) could not be saved to disk: not supported", prefix, mime, size)
	}
	switch b.Type {
	case "text":
		return text(b.Text)
	case "image":
		return text(binaryNote("[Image from "+server+"] ", b.MimeType, base64Len(b.Data)))
	case "audio":
		return text(binaryNote("[Audio from "+server+"] ", b.MimeType, base64Len(b.Data)))
	case "resource":
		var r struct {
			URI      string  `json:"uri"`
			MimeType string  `json:"mimeType"`
			Text     *string `json:"text"`
			Blob     *string `json:"blob"`
		}
		_ = json.Unmarshal(b.Resource, &r)
		prefix := "[Resource from " + server + " at " + r.URI + "] "
		if r.Text != nil {
			return text(prefix + *r.Text)
		}
		if r.Blob != nil {
			return text(binaryNote(prefix, r.MimeType, base64Len(*r.Blob)))
		}
		return nil
	case "resource_link":
		s := "[Resource link: " + b.Name + "] " + b.URI
		if b.Description != "" {
			s += " (" + b.Description + ")"
		}
		return text(s)
	}
	return nil
}

// base64Len is the decoded size of a base64 payload (0 when invalid).
func base64Len(s string) int {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	return len(data)
}

// ExpandPromptCommand runs the prompt command name (without the slash) with
// the raw argument string. ok is false when no connected server offers a
// prompt under that name (the input is then not an MCP prompt command).
func (h *Host) ExpandPromptCommand(ctx context.Context, name, rawArgs string) (blocks []agent.ContentBlock, ok bool, err error) {
	cmd, found := h.LookupPromptCommand(name)
	if !found {
		return nil, false, nil
	}
	blocks, err = h.GetPrompt(ctx, cmd, rawArgs)
	return blocks, true, err
}
