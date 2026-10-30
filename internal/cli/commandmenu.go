package cli

import (
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/textutil"
	"github.com/umairge66/gocode/internal/ui"
)

// This file builds the slash-command suggestion list that feeds both the
// composer autocomplete menu (ui.SetCommandsMsg) and the /help screen. The
// sources, in menu order, are:
//
//   - the builtin slash commands (conversation.Builtins — the builtincmds
//     registry): name + description;
//   - the loaded custom commands (conversation.Commands — markdown/plugin
//     commands): name + description;
//
// Hidden / non-user-invocable entries are skipped (they still resolve when typed
// explicitly, but they are not offered in the menu). Each Suggestion carries
// Name (bare, for prefix filtering), Display ("/name"), Description and Value
// ("/name", what the composer is completed to).
//
// Skills live in a separate registry (conversation.Skills) surfaced to the
// model through the Skill tool listing (skills.Registry.Listing); there is no
// "/skill" dispatch path — conversation.expandInput resolves only
// conversation.Commands and returns ErrUnknownCommand otherwise. Listing
// skills here would therefore complete to a dead-end "/name", so they are
// intentionally left OUT until a later round wires skill slash-dispatch (which
// lives in the conversation package, not owned here). The "@" (file-mention)
// and "!" (bash-mode) composer triggers are likewise left as a seam; "!"
// already maps to ui.ModeBash.

// commandSuggestions builds the ordered, de-duplicated slash-command suggestion
// list for a conversation. Builtins come first (in registration order), then the
// custom commands (in registry order). A name already contributed by an
// earlier (higher-priority) source is not repeated.
func commandSuggestions(conv *conversation.Conversation) []ui.Suggestion {
	if conv == nil {
		return nil
	}
	var out []ui.Suggestion
	seen := map[string]struct{}{}
	add := func(name, desc, kind string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, ui.Suggestion{
			Name:        name,
			Display:     "/" + name,
			Description: desc,
			Value:       "/" + name,
			Kind:        kind,
		})
	}

	// Builtins first.
	if conv.Builtins != nil {
		for _, b := range conv.Builtins.All() {
			if b == nil || b.Hidden {
				continue
			}
			add(b.Name, b.Description, "")
		}
	}

	// Custom (markdown/plugin) commands next.
	if conv.Commands != nil {
		for _, c := range conv.Commands.All() {
			if c == nil || c.Hidden {
				continue
			}
			kind := ""
			if c.LoadedFrom == "skills" {
				kind = "skill"
			}
			add(c.Name, c.Description, kind)
		}
	}

	// MCP prompt commands last (the MCP servers' prompts join the command
	// list as /mcp__server__prompt).
	for _, p := range conv.MCPPromptCommands() {
		add(p.Name, p.Description, "mcp")
	}

	return out
}

// helpText renders the /help screen: a header followed by an aligned
// list of every slash command and its description, one per line
// ("/name    description"). Commands are sorted alphabetically for the help
// listing (the live menu keeps source order). It returns "" when there are no
// commands.
func helpText(suggestions []ui.Suggestion) string {
	if len(suggestions) == 0 {
		return ""
	}
	rows := make([]ui.Suggestion, len(suggestions))
	copy(rows, suggestions)
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})

	// Align the descriptions: pad every "/name" to the widest one plus a gutter.
	widest := 0
	for _, s := range rows {
		if n := textutil.CountCodePoints(s.Display); n > widest {
			widest = n
		}
	}
	var b strings.Builder
	b.WriteString("Available commands:")
	for _, s := range rows {
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(s.Display)
		if s.Description != "" {
			pad := widest - textutil.CountCodePoints(s.Display) + 2
			b.WriteString(strings.Repeat(" ", pad))
			b.WriteString(s.Description)
		}
	}
	return b.String()
}

// helpTextForConversation is the convenience used by both the interactive host
// and the -p path: build the command list and format it as the /help screen.
func helpTextForConversation(conv *conversation.Conversation) string {
	return helpText(commandSuggestions(conv))
}

// isRecognizedCommand reports whether input is a RECOGNIZED slash command for
// this conversation: it must parse as a slash command (start with '/' and have a
// non-empty name token) AND its first token — the name without the slash, before
// any space — must resolve either to a builtin (conv.Builtins) or to a loaded
// custom/plugin command (conv.Commands). These are the same two sources the "/"
// autocomplete menu (commandSuggestions) draws from, so the echo gate stays in
// lock-step with what the menu offers. An unknown `/foo` resolves to neither and
// returns false, so it still echoes and the conversation returns its normal
// "unknown command" handling. It is the host's ui.Model.IsCommand predicate; the
// Model uses it to suppress the transcript echo for commands while still running
// them and recording them in the input history.
func isRecognizedCommand(conv *conversation.Conversation, input string) bool {
	if conv == nil {
		return false
	}
	parsed, ok := commands.ParseInput(strings.TrimSpace(input))
	if !ok {
		return false
	}
	if conv.Builtins != nil {
		if _, ok := conv.Builtins.Lookup(parsed.Name); ok {
			return true
		}
	}
	if conv.Commands != nil {
		if conv.Commands.Resolve(parsed.Name) != nil {
			return true
		}
	}
	for _, p := range conv.MCPPromptCommands() {
		if p.Name == parsed.Name {
			return true
		}
	}
	return false
}
