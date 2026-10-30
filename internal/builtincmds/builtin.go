// Package builtincmds implements the EXECUTION layer of the built-in slash
// commands and the custom-command `!`-bash blocks.
//
// The sibling package internal/commands already implements how `/command`
// input is parsed (ParseInput), how custom markdown commands are loaded, and how
// argument placeholders are expanded. What a built-in command actually *does*
// and the running of the `!`-shell blocks live in this package.
//
// Design: instead of each handler mutating a live session directly (closing
// over the running session object and the UI), every built-in
// returns a typed Effect describing the host-visible consequence — print text,
// change the model, change the permission mode, clear the transcript, trigger
// compaction, open a dialog, exit, run shell, or submit a prompt. The host
// (conversation / cli layer) applies the Effect. This keeps the package
// testable and free of UI/session coupling, and splits a command descriptor
// (pure metadata) from its `call`/dialog behavior.
//
// Command descriptors are `{type:"local"|"local-jsx"|"prompt", name, aliases,
// description, ...}`. A "local" command's call returns text or acts on the
// session; a "local-jsx" command opens a dialog; a "prompt" command returns
// text submitted to the model. For example, /clear resets the session
// (resetTrigger "clear"), /model with an empty or "list" argument shows the
// current model and otherwise validates and sets it, and /compact summarizes
// the conversation (trigger "manual"), optionally with custom instructions.
package builtincmds

import (
	"strings"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/toolreg"
)

// EffectKind enumerates the host-visible consequences a built-in command can
// have. The names follow the descriptor table (the `type` field plus each
// `call`/dialog's action).
type EffectKind int

const (
	// EffectNone is the zero value: no effect (used by hidden/disabled stubs).
	EffectNone EffectKind = iota
	// EffectPrintText prints Effect.Text to the user without involving the
	// model: a `type:"local"` call returning {type:"text", value}
	// (e.g. /usage, /version, /context).
	EffectPrintText
	// EffectSetModel changes the active model to Effect.Model.
	EffectSetModel
	// EffectSetPermissionMode switches the permission mode to Effect.Mode.
	EffectSetPermissionMode
	// EffectClearTranscript starts a fresh session with empty context; the old
	// session stays on disk (resumable). /clear, resetTrigger:"clear".
	EffectClearTranscript
	// EffectTriggerCompaction summarizes the conversation to free context.
	// Effect.Text carries optional custom summarization instructions.
	EffectTriggerCompaction
	// EffectOpenDialog asks the host to open an interactive UI; Effect.Dialog
	// names it and Effect.Args carries the raw argument string. Every
	// `type:"local-jsx"` descriptor (e.g. /help, /status, /config, /mcp).
	EffectOpenDialog
	// EffectExit leaves the CLI (or detaches a background session) (/exit,
	// aliases ["quit"]).
	EffectExit
	// EffectRunShell asks the host to run the embedded `!`-shell blocks and
	// splice their output into the prompt. Effect.Shell carries the blocks.
	EffectRunShell
	// EffectSubmitPrompt injects Effect.Text as a user prompt for the model to
	// act on: a `type:"prompt"` built-in whose getPromptForCommand
	// returns text (e.g. /init, /ultrareview).
	EffectSubmitPrompt
)

// String renders an EffectKind for diagnostics and test output.
func (k EffectKind) String() string {
	switch k {
	case EffectPrintText:
		return "PrintText"
	case EffectSetModel:
		return "SetModel"
	case EffectSetPermissionMode:
		return "SetPermissionMode"
	case EffectClearTranscript:
		return "ClearTranscript"
	case EffectTriggerCompaction:
		return "TriggerCompaction"
	case EffectOpenDialog:
		return "OpenDialog"
	case EffectExit:
		return "Exit"
	case EffectRunShell:
		return "RunShell"
	case EffectSubmitPrompt:
		return "SubmitPrompt"
	default:
		return "None"
	}
}

// Effect is the typed result of running a built-in command. Only the fields
// relevant to Kind are populated.
type Effect struct {
	Kind EffectKind
	// Text is the body for EffectPrintText / EffectSubmitPrompt, and the
	// optional custom instructions for EffectTriggerCompaction.
	Text string
	// Model is the target model id for EffectSetModel.
	Model string
	// Mode is the target permission mode for EffectSetPermissionMode.
	Mode toolreg.Mode
	// Dialog names the UI to open for EffectOpenDialog (e.g. "help", "status").
	Dialog string
	// Args is the raw, untrimmed argument string the command was invoked with.
	// Carried on dialog/shell effects so the host can act on it.
	Args string
	// Shell holds the `!`-blocks for EffectRunShell.
	Shell []commands.ShellBlock

	// Generator, for EffectSubmitPrompt, names a host-side prompt builder
	// (GeneratorInsights, GeneratorTeamOnboarding) whose output replaces Text:
	// the prompt needs session state this package does not hold (local
	// transcripts, the model caller): a `type:"prompt"`
	// descriptor whose getPromptForCommand computes the prompt at run time.
	Generator string
	// AllowedTools are tool permission rules granted for the turn the prompt
	// starts.
	AllowedTools []string
	// Effort is the turn's reasoning-effort override (the descriptor's
	// effort), "" for none.
	Effort string
}

// Host-side prompt generators (Effect.Generator).
const (
	GeneratorInsights       = "insights"
	GeneratorTeamOnboarding = "team-onboarding"
)

// Builtin is a single built-in slash command: its descriptor metadata plus the
// handler that maps an argument string to an Effect.
type Builtin struct {
	// Name is the canonical bare command name (no leading '/').
	Name string
	// Aliases are alternate bare names the command answers to.
	Aliases []string
	// Description is the one-line summary shown in listings.
	Description string
	// Kind is the descriptor type: "local", "local-jsx" or "prompt".
	Kind string
	// Hidden mirrors the descriptor's isHidden (hidden commands still resolve
	// when typed explicitly).
	Hidden bool
	// Handler maps the raw (untrimmed) argument string to an Effect.
	Handler func(args string) Effect
}

// matchNames returns the lowercased tokens this built-in answers to.
func (b *Builtin) matchNames() []string {
	out := make([]string, 0, 1+len(b.Aliases))
	out = append(out, strings.ToLower(b.Name))
	for _, a := range b.Aliases {
		if a != "" {
			out = append(out, strings.ToLower(a))
		}
	}
	return out
}

// dialogEffect builds an EffectOpenDialog for a local-jsx command.
func dialogEffect(name, args string) Effect {
	return Effect{Kind: EffectOpenDialog, Dialog: name, Args: args}
}

// builtinModelList is shown by /model when invoked with no argument. The host
// supplies the real catalog, so the model picker dialog opens instead of
// printing a stale list.
func modelHandler(args string) Effect {
	m := strings.TrimSpace(args)
	if m == "" {
		// No model given: open the interactive picker (local-jsx /model).
		return dialogEffect("model", args)
	}
	// A concrete model was named: set it. There is no hosted entitlement list
	// or premium-model consent check: with provider API keys there is no such
	// service, and the provider rejects an unknown model
	// on the first request.
	return Effect{Kind: EffectSetModel, Model: m, Args: args}
}

// compactHandler carries any trimmed custom summarization instructions.
func compactHandler(args string) Effect {
	return Effect{Kind: EffectTriggerCompaction, Text: strings.TrimSpace(args), Args: args}
}
