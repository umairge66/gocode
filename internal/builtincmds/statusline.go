package builtincmds

import "strings"

// /statusline asks the model to delegate status-line setup to the built-in
// "statusline-setup" agent, which reads the user's shell configuration and
// writes the statusLine setting.

// StatuslineAllowedTools are the rules granted for the /statusline turn.
var StatuslineAllowedTools = []string{"Agent", "Read(~/**)", "Edit(~/.config/gocode/settings.json)"}

// statuslineDefaultRequest is the request used when /statusline has no
// argument.
const statuslineDefaultRequest = "Configure my statusLine from my shell PS1 configuration"

// StatuslinePrompt is the prompt /statusline submits for args.
func StatuslinePrompt(args string) string {
	n := strings.TrimSpace(args)
	if n == "" {
		n = statuslineDefaultRequest
	}
	return `Create an Agent with subagent_type "statusline-setup" and the prompt "` + n + `"`
}

func statuslineHandler(args string) Effect {
	return Effect{Kind: EffectSubmitPrompt, Text: StatuslinePrompt(args), Args: args,
		AllowedTools: append([]string(nil), StatuslineAllowedTools...)}
}
