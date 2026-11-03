package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/commands"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/skills"
)

// SkillToolName is the model-facing tool name.
const SkillToolName = string(protocol.ToolSkill)

// skillToolPrompt is the Skill tool prompt.
const skillToolPrompt = "Invoke a skill.\n\n" +
	"A skill is a packaged set of instructions the user or project has set up for a particular kind of task (deploy steps, a review checklist, a repo-specific workflow). Available skills appear in a system-reminder listing with one-line descriptions. When the task at hand is one a listed skill covers, call this tool first — the skill's instructions load into the turn for you to follow in place of your default approach; some skills instead run in a subagent and return the finished result. A skill that runs in the background returns only the agent's name — its result arrives later as a task notification, so don't wait on it or invoke it again in the meantime. Users may also ask for one by name (`/<name>`, or \"slash command\"); that's a request to invoke it.\n\n" +
	"- `skill`: exact name from the listing, no leading slash. Plugin skills use `plugin:skill`. Directory-scoped skills are listed with a path prefix (`apps/web:deploy`); when both scoped and unscoped variants of a name exist, pick the one whose directory contains the files you're working on (most specific wins; unscoped otherwise).\n" +
	"- `args`: optional arguments to pass through.\n\n" +
	"Only names from the listing (or that the user typed explicitly) are valid. Built-in CLI commands (`/help`, `/clear`, …) aren't skills. If a `<command-name>` block is already present this turn, the skill is loaded — follow it directly rather than calling again.\n"

// Schema field descriptions.
const (
	descSkillName = "The name of a skill from the available-skills list. Do not guess names."
	descSkillArgs = "Optional arguments for the skill"
)

// Placeholders substituted into a skill body. The CLAUDE_* spellings are
// accepted as aliases so Claude-style skills on disk keep working.
var (
	skillDirVars   = []string{"${GOCODE_SKILL_DIR}", "${gocode_SKILL_DIR}", "${CLAUDE_SKILL_DIR}"}
	projectDirVars = []string{"${GOCODE_PROJECT_DIR}", "${gocode_PROJECT_DIR}", "${CLAUDE_PROJECT_DIR}"}
	pluginRootVars = []string{"${GOCODE_PLUGIN_ROOT}", "${gocode_PLUGIN_ROOT}", "${CLAUDE_PLUGIN_ROOT}"}
	pluginDataVars = []string{"${GOCODE_PLUGIN_DATA}", "${gocode_PLUGIN_DATA}", "${CLAUDE_PLUGIN_DATA}"}
	sessionIDVars  = []string{"${GOCODE_SESSION_ID}", "${gocode_SESSION_ID}", "${CLAUDE_SESSION_ID}"}
	effortVars     = []string{"${GOCODE_EFFORT}", "${gocode_EFFORT}", "${CLAUDE_EFFORT}"}
)

// ErrUnknownSkill matches (errors.Is) the "Unknown skill" refusal.
var ErrUnknownSkill = errors.New("unknown skill")

// ErrModelInvocationDisabled matches (errors.Is) the disable-model-invocation
// refusal.
var ErrModelInvocationDisabled = errors.New("skill model invocation disabled")

// SkillInput is the decoded Skill tool input (skill + optional args).
type SkillInput struct {
	Skill string `json:"skill"`
	Args  string `json:"args,omitempty"`
}

// SkillInvocation is the outcome of invoking a skill.
type SkillInvocation struct {
	// CommandName is the invoked skill name as the model wrote it (slash
	// stripped).
	CommandName string
	// Content is the skill prompt injected into the conversation:
	// "Base directory for this skill: <dir>\n\n" + body, with arguments and
	// ${gocode_*} placeholders substituted. Empty for a forked skill.
	Content string
	// AllowedTools / DisallowedTools / Model / Effort are the skill's
	// frontmatter overrides. For an inline skill they were activated through
	// SkillTool.Scope for the rest of the turn; for a forked skill they shaped
	// the subagent spawn.
	AllowedTools    []string
	DisallowedTools []string
	Model           string
	Effort          string
	// ActiveModel is the model the rest of the turn runs on when an inline
	// skill's `model` override took effect ("" otherwise).
	ActiveModel string
	// Forked reports that the skill ran in a subagent; Result holds its
	// report (or, when Background, the launch line).
	Forked bool
	Result string
	// Background reports that the forked skill was launched as a background
	// agent; AgentID is its task id and the outcome arrives later as a task
	// notification.
	Background bool
	AgentID    string
}

// ToolResult is the tool_result text returned for the invocation.
func (inv SkillInvocation) ToolResult() string {
	if inv.Forked && inv.Background {
		return fmt.Sprintf("Skill %q launched (forked execution, running in the background).\n\n%s", inv.CommandName, inv.Result)
	}
	if inv.Forked {
		return fmt.Sprintf("Skill %q completed (forked execution).\n\nResult:\n%s", inv.CommandName, inv.Result)
	}
	return "Launching skill: " + inv.CommandName
}

// SkillTool is the model-facing Skill tool.
type SkillTool struct {
	// Registry resolves skill names (required).
	Registry *skills.Registry
	// FS reads SKILL.md files (required).
	FS fsfacade.FS
	// ProjectDir / SessionID / DefaultEffort fill ${GOCODE_PROJECT_DIR},
	// ${GOCODE_SESSION_ID} and ${GOCODE_EFFORT}; empty values leave the
	// placeholder untouched (DefaultEffort is used when the skill sets none).
	ProjectDir    string
	SessionID     string
	DefaultEffort string
	// Agent, when set, runs `context: fork` skills in a subagent of the
	// skill's `agent` type. When nil such skills expand inline.
	Agent *AgentTool
	// ListingInReminder omits the available-skills listing from Description
	// (it is delivered as a system-reminder; the caller then injects
	// Listing() itself).
	ListingInReminder bool
	// Scope, when set, activates an inline skill's allowed-tools,
	// disallowed-tools, model and effort for the rest of the current turn and
	// undoes them when the turn ends. When nil they are carried on the
	// SkillInvocation but not applied.
	Scope SkillScope
	// Hooks, when set, registers an invoked skill's frontmatter `hooks` for
	// the rest of the session (hookhost.Host satisfies it). When nil skill
	// hooks are ignored.
	Hooks SkillHooksRegistrar
	// ShellExecutionDisabled, when set and true, applies the
	// disableSkillShellExecution setting: the `!`-blocks of a covered skill
	// (every scope but managed and builtin) are replaced by the policy
	// placeholder before the model sees the text. nil means off.
	ShellExecutionDisabled func() bool
	// ExpandShell, when set, runs a skill's `!`-blocks and splices their
	// output into the prompt (see expandShellBlocks). The skill's identity,
	// `allowed-tools` and `shell:` ride on ctx (commands.WithShellInvocation,
	// commands.WithCommandShell); the cli layer wires it to the slash-command
	// expander and permission gate (commands.ExpandShellBlocks over
	// toolrun.Dispatcher.AuthorizeShellCommand). nil leaves blocks as written:
	// nothing runs.
	ExpandShell func(ctx context.Context, body string) (string, error)
	// CallerDenyRules, when set, returns the invoking agent's command-scoped
	// deny rules (rule strings) at invocation: the temporary deny rules of
	// skills active on it or inherited by it. A background fork validates them
	// as its frozen scoping record. nil = none.
	CallerDenyRules func() []string
}

// callerDenies snapshots CallerDenyRules.
func (s *SkillTool) callerDenies() []string {
	if s.CallerDenyRules == nil {
		return nil
	}
	return s.CallerDenyRules()
}

// SkillActivation is an inline skill's frontmatter as it applies to the rest
// of the turn.
type SkillActivation struct {
	// Skill is the invoked skill name (for attribution).
	Skill string
	// AllowedTools / DisallowedTools are permission-rule strings
	// ("Bash(make:*)", "Write", …) to allow / deny for the scope.
	AllowedTools    []string
	DisallowedTools []string
	// Model is the frontmatter model ("" when absent; "inherit" is already
	// dropped by the Skill tool). Effort is the frontmatter effort.
	Model  string
	Effort string
}

// SkillScope applies an inline skill's activation for the rest of the current
// turn (the per-query permission layers: allowed_tools,
// disallowed_tools, model, effort) and restores everything when that turn ends.
// It returns the model the turn now runs on when a model override took effect
// ("" otherwise).
type SkillScope interface {
	ActivateInline(ctx context.Context, a SkillActivation) (activeModel string, err error)
}

// SkillHooksRegistrar registers a skill's frontmatter hooks for the session.
type SkillHooksRegistrar interface {
	AddSkillHooks(skill, skillRoot string, raw map[string]any, managed bool) (int, error)
}

// WithScope returns a copy of the tool whose inline activations go through sc
// (a subagent's dispatcher gets its own copy so a skill it invokes is scoped
// to that subagent, not to the main conversation).
func (s *SkillTool) WithScope(sc SkillScope) *SkillTool {
	c := *s
	c.Scope = sc
	return &c
}

// NewSkillTool returns a Skill tool over the registry and filesystem.
func NewSkillTool(reg *skills.Registry, fsys fsfacade.FS) *SkillTool {
	return &SkillTool{Registry: reg, FS: fsys}
}

// Name returns "Skill".
func (s *SkillTool) Name() string { return SkillToolName }

// Listing renders the skill_listing reminder: the fixed header and one line
// per model-invocable skill. Empty when no skill is listable.
func (s *SkillTool) Listing() string {
	if s.Registry == nil {
		return ""
	}
	body := s.Registry.Listing()
	if body == "" {
		return ""
	}
	return skills.ListingHeader + "\n\n" + body
}

// Description returns the tool prompt, followed by the skill listing unless
// ListingInReminder is set.
func (s *SkillTool) Description() string {
	if s.ListingInReminder {
		return skillToolPrompt
	}
	if l := s.Listing(); l != "" {
		return skillToolPrompt + "\n" + l
	}
	return skillToolPrompt
}

// InputSchema returns the Skill tool input schema.
func (s *SkillTool) InputSchema() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skill": map[string]any{"type": "string", "description": descSkillName},
			"args":  map[string]any{"type": "string", "description": descSkillArgs},
		},
		"required":             []string{"skill"},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// Execute runs one model-initiated Skill call and returns the tool-result text
// followed by the injected skill content (there is no separate meta
// message channel, so the content rides in the tool result).
func (s *SkillTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in SkillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid Skill input: %v", err)
	}
	inv, err := s.Invoke(ctx, in.Skill, in.Args)
	if err != nil {
		return "", err
	}
	if inv.Forked {
		return truncateResult(inv.ToolResult()), nil
	}
	return truncateResult(inv.ToolResult() + "\n\n" + inv.Content), nil
}

// ErrNotUserInvocable matches (errors.Is) the user-invocable: false refusal
// of a skill typed as a slash command.
var ErrNotUserInvocable = errors.New("skill not user-invocable")

// Invoke validates and loads a skill as the model would invoke it.
func (s *SkillTool) Invoke(ctx context.Context, name, args string) (SkillInvocation, error) {
	return s.invoke(ctx, name, args, false)
}

// InvokeAsUser runs a skill the person typed as `/name args`: the same
// loading, scoping and inline-or-fork execution as the model's Skill call, but
// disable-model-invocation does not apply (it only hides the skill from the
// model) and a `user-invocable: false` skill is refused.
func (s *SkillTool) InvokeAsUser(ctx context.Context, name, args string) (SkillInvocation, error) {
	return s.invoke(ctx, name, args, true)
}

func (s *SkillTool) invoke(ctx context.Context, name, args string, byUser bool) (SkillInvocation, error) {
	if s.Registry == nil || s.FS == nil {
		return SkillInvocation{}, errors.New("Skill tool is not configured (missing registry or filesystem)")
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return SkillInvocation{}, fmt.Errorf("Invalid skill format: %s", name)
	}
	cmd := strings.TrimPrefix(trimmed, "/")

	d, ok := s.Registry.Lookup(cmd)
	if !ok {
		msg := "Unknown skill: " + cmd
		if sug := s.suggest(cmd); sug != "" {
			msg += ". Did you mean " + sug + "?"
		}
		return SkillInvocation{}, &toolError{msg: msg, kind: ErrUnknownSkill}
	}
	if byUser && !d.UserInvocable {
		return SkillInvocation{}, &toolError{
			msg:  fmt.Sprintf("Skill %s can only be invoked by the model (user-invocable: false).", cmd),
			kind: ErrNotUserInvocable,
		}
	}
	if d.DisableModelInvocation && !byUser {
		return SkillInvocation{}, &toolError{
			msg: fmt.Sprintf("Skill %s cannot be used with %s tool due to disable-model-invocation. Ask the user to run /%s themselves — it cannot be invoked via the %s tool. Do not replicate this skill's workflow by other means — it is reserved for explicit user invocation.",
				cmd, SkillToolName, cmd, SkillToolName),
			kind: ErrModelInvocationDisabled,
		}
	}

	content, err := s.render(ctx, d, args, s.shellHandOff(d))
	if err != nil {
		return SkillInvocation{}, err
	}
	inv := SkillInvocation{
		CommandName:     cmd,
		AllowedTools:    append([]string(nil), d.AllowedTools...),
		DisallowedTools: append([]string(nil), d.DisallowedTools...),
		Model:           d.Model,
		Effort:          d.Effort,
	}
	// "inherit" means: no model change.
	model := strings.TrimSpace(d.Model)
	if strings.EqualFold(model, "inherit") {
		model = ""
	}

	// Frontmatter hooks are registered for the session when the skill is
	// invoked.
	if s.Hooks != nil && len(d.Hooks) > 0 {
		if _, err := s.Hooks.AddSkillHooks(d.Name, d.BaseDir, d.Hooks, d.Scope == skills.ScopeManaged); err != nil {
			return SkillInvocation{}, fmt.Errorf("Skill %s could not be loaded: %v", cmd, err)
		}
	}

	if d.RunsInSubagent() && s.Agent != nil {
		bg := d.RunsInBackground()
		desc := strings.TrimSpace("/" + cmd + " " + args)
		out, err := s.Agent.run(ctx, AgentInput{
			Description:     desc,
			Prompt:          content,
			SubagentType:    d.Agent,
			RunInBackground: &bg,
			skill: &forkedSkill{
				Name:   d.Name,
				Model:  model,
				Effort: d.Effort,
				Scope: ChildRuleScope{
					Origin:     "/" + cmd,
					AllowRules: append([]string(nil), d.AllowedTools...),
					DenyRules:  append([]string(nil), d.DisallowedTools...),
				},
				FrozenDenies: s.callerDenies(),
			},
		})
		if errors.Is(err, ErrNestingLimit) {
			// The invoking agent is already at the nesting cap: refused with
			// the Agent tool's own message (see the depth note in
			// skillfork.go).
			return SkillInvocation{}, err
		}
		if err != nil {
			return SkillInvocation{}, fmt.Errorf("Skill %s failed: %v", cmd, err)
		}
		inv.Forked = true
		if out.background {
			inv.Background = true
			inv.AgentID = out.taskID
			inv.Result = "Running in the background as @" + out.taskID
			return inv, nil
		}
		inv.Result = out.text
		return inv, nil
	}

	if s.Scope != nil && (len(d.AllowedTools) > 0 || len(d.DisallowedTools) > 0 || model != "" || strings.TrimSpace(d.Effort) != "") {
		active, err := s.Scope.ActivateInline(ctx, SkillActivation{
			Skill:           cmd,
			AllowedTools:    append([]string(nil), d.AllowedTools...),
			DisallowedTools: append([]string(nil), d.DisallowedTools...),
			Model:           model,
			Effort:          strings.TrimSpace(d.Effort),
		})
		if err != nil {
			return SkillInvocation{}, fmt.Errorf("Skill %s could not be activated: %v", cmd, err)
		}
		inv.ActiveModel = active
	}
	inv.Content = content
	return inv, nil
}

// render reads the SKILL.md body and applies the substitutions in order:
// base-directory prefix, argument substitution, then the
// ${GOCODE_SKILL_DIR}, ${GOCODE_PROJECT_DIR}, ${GOCODE_SESSION_ID} and
// ${GOCODE_EFFORT} placeholders, and last the `!`-blocks: replaced by the
// policy placeholder under disableSkillShellExecution, else run
// (expandShellBlocks). handOff reports whether an un-approved block may be
// handed to the model in auto mode.
func (s *SkillTool) render(ctx context.Context, d skills.Descriptor, args string, handOff bool) (string, error) {
	data, err := s.FS.ReadFile(d.Path)
	if err != nil {
		return "", fmt.Errorf("Skill %s could not be loaded. Proceed without it.", d.Name)
	}
	body := skills.ParseSkillMD(data, d.Path).Content

	text := body
	if d.BaseDir != "" {
		text = "Base directory for this skill: " + d.BaseDir + "\n\n" + body
	}
	text, err = SubstituteArguments(text, args, true, escapeShellMarks)
	if err != nil {
		return "", err
	}
	if d.BaseDir != "" {
		text = replaceAny(text, skillDirVars, d.BaseDir)
	}
	if d.PluginRoot != "" {
		text = replaceAny(text, pluginRootVars, d.PluginRoot)
	}
	if d.PluginData != "" {
		text = replaceAny(text, pluginDataVars, d.PluginData)
	}
	if s.ProjectDir != "" {
		text = replaceAny(text, projectDirVars, s.ProjectDir)
	}
	if s.SessionID != "" {
		text = replaceAny(text, sessionIDVars, s.SessionID)
	}
	effort := d.Effort
	if effort == "" {
		effort = s.DefaultEffort
	}
	if effort != "" {
		text = replaceAny(text, effortVars, effort)
	}
	if s.ShellExecutionDisabled != nil && d.Scope != skills.ScopeManaged && d.Scope != skills.ScopeBuiltin && s.ShellExecutionDisabled() {
		return commands.ReplaceShellBlocks(text, commands.ShellDisabledPlaceholder), nil
	}
	return s.expandShellBlocks(ctx, d, text, handOff)
}

// expandShellBlocks runs the `!`-blocks of a rendered skill through
// ExpandShell, exactly as a slash command's: every block is permission-checked
// as `Bash(<cmd>)` in the invoking agent's permission context, with the
// skill's `allowed-tools` as temporary allow rules for this expansion only
// (deny rules still win), before anything runs. A refused or failing block
// fails the whole Skill call with an error message; in auto mode an
// un-approved (not denied) block becomes the "run this first" instruction
// when handOff is set. Blocks run in the session's working directory (the
// Bash tool's), with the skill's `shell:` interpreter; no extra environment is
// set. Synced skills never run blocks; they are left as written.
func (s *SkillTool) expandShellBlocks(ctx context.Context, d skills.Descriptor, text string, handOff bool) (string, error) {
	if s.ExpandShell == nil || d.Scope == skills.ScopeSynced {
		return text, nil
	}
	ctx = commands.WithCommandShell(ctx, d.Shell)
	ctx = commands.WithShellInvocation(ctx, commands.ShellInvocation{
		Name:         d.Name,
		AllowedTools: append([]string(nil), d.AllowedTools...),
		NoHandOff:    !handOff,
	})
	out, err := s.ExpandShell(ctx, text)
	if err != nil {
		var perr *commands.ShellPermissionError
		var cerr *commands.ShellCommandError
		if errors.As(err, &perr) || errors.As(err, &cerr) || ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("Skill %s could not be loaded: %v", d.Name, err)
	}
	return out, nil
}

// shellHandOff reports whether d's `!`-blocks may be handed to the model in
// auto mode: always for an inline skill, and for a forked one only when it
// names no `agent` (the hand-off then lands in the fork's prompt).
func (s *SkillTool) shellHandOff(d skills.Descriptor) bool {
	if d.RunsInSubagent() && s.Agent != nil {
		return strings.TrimSpace(d.Agent) == ""
	}
	return true
}

func replaceAny(s string, vars []string, val string) string {
	for _, v := range vars {
		s = strings.ReplaceAll(s, v, val)
	}
	return s
}

// suggest returns the closest model-invocable skill name within edit distance
// 2, or "".
func (s *SkillTool) suggest(name string) string {
	best, bestD := "", 3
	for _, d := range s.Registry.List() {
		if d.DisableModelInvocation {
			continue
		}
		for _, cand := range []string{d.Name, d.UnqualifiedName} {
			if cand == "" {
				continue
			}
			if dist := levenshtein(strings.ToLower(name), strings.ToLower(cand)); dist < bestD {
				best, bestD = d.Name, dist
			}
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Argument substitution.

const (
	// maxSubstitutedChars is the cap on body + filled arguments.
	maxSubstitutedChars = 4000000
	// dollarSentinel protects a literal "$" through substitution;
	// fillSentinel brackets filled values so they are not re-expanded.
	dollarSentinel = "￿"
	fillSentinel   = "￾"
)

var (
	escapedDollarRe = regexp.MustCompile(`\\\$(\d|ARGUMENTS)`)
	indexedArgsRe   = regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]`)
	positionalRe    = regexp.MustCompile(`\$(\d+)`)
	shellBangRe     = regexp.MustCompile(`(?m)(^|\s)!`)
)

// SubstituteArguments fills a skill body with its arguments:
//   - `\$N` / `\$ARGUMENTS` escapes keep a literal "$";
//   - `$ARGUMENTS[n]` and `$n` take the n-th shell-split argument (left as-is
//     when absent);
//   - `$ARGUMENTS` takes the whole argument string;
//   - when nothing was substituted, appendIfUnused is set and args is
//     non-empty, "\n\nARGUMENTS: <args>" is appended.
//
// escape, when non-nil, is applied to every filled value.
func SubstituteArguments(body, args string, appendIfUnused bool, escape func(string) string) (string, error) {
	body = strings.NewReplacer(dollarSentinel, "�", fillSentinel, "�").Replace(body)
	total := len(body)
	var tooLong bool
	fill := func(v string) string {
		v = strings.NewReplacer(dollarSentinel, "�", fillSentinel, "�").Replace(v)
		if escape != nil {
			v = escape(v)
		}
		total += len(v)
		if total > maxSubstitutedChars {
			tooLong = true
		}
		return fillSentinel + strings.ReplaceAll(v, "$", dollarSentinel) + fillSentinel
	}
	words := splitArgs(args)
	used := false

	// `\$` (not itself escaped) before a digit or ARGUMENTS → protected "$".
	body = replaceUnescaped(body)

	body = indexedArgsRe.ReplaceAllStringFunc(body, func(m string) string {
		n, _ := strconv.Atoi(indexedArgsRe.FindStringSubmatch(m)[1])
		if n >= len(words) {
			return dollarSentinel + m[1:]
		}
		used = true
		return fill(words[n])
	})

	// `$N` not followed by a word character.
	var b strings.Builder
	last := 0
	for _, loc := range positionalRe.FindAllStringSubmatchIndex(body, -1) {
		end := loc[1]
		if end < len(body) && isWordByte(body[end]) {
			continue
		}
		n, err := strconv.Atoi(body[loc[2]:loc[3]])
		if err != nil || n >= len(words) {
			continue
		}
		b.WriteString(body[last:loc[0]])
		b.WriteString(fill(words[n]))
		used = true
		last = end
	}
	b.WriteString(body[last:])
	body = b.String()

	if strings.Contains(body, "$ARGUMENTS") {
		used = true
		body = strings.ReplaceAll(body, "$ARGUMENTS", fill(args))
	}
	if !used && appendIfUnused && args != "" {
		body += "\n\nARGUMENTS: " + fill(args)
	}
	if tooLong {
		return "", fmt.Errorf("This skill, command or hook and the arguments filled into it add up to more than 4,000,000 characters. Use shorter arguments or fewer placeholders such as $ARGUMENTS.")
	}
	body = strings.ReplaceAll(body, dollarSentinel, "$")
	body = strings.ReplaceAll(body, fillSentinel, "")
	return body, nil
}

// replaceUnescaped turns `\$` into the protected-dollar sentinel when it
// precedes a digit or ARGUMENTS and the backslash is not itself escaped.
func replaceUnescaped(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range escapedDollarRe.FindAllStringIndex(s, -1) {
		if loc[0] > 0 && s[loc[0]-1] == '\\' {
			continue
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(dollarSentinel)
		last = loc[0] + 2 // drop `\$`, keep the lookahead text
	}
	b.WriteString(s[last:])
	return b.String()
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// escapeShellMarks defuses the `!`-command
// markers in filled argument values so arguments cannot inject a shell block.
func escapeShellMarks(s string) string {
	s = strings.ReplaceAll(s, "`!", "` !")
	s = strings.ReplaceAll(s, "!`", "! `")
	return shellBangRe.ReplaceAllString(s, "${1}\\!")
}

// splitArgs does shell-style word splitting (quotes and backslash escapes
// honored), falling back to whitespace splitting.
func splitArgs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var (
		out     []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			inWord = true
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return strings.Fields(s) // unbalanced: fall back to whitespace splitting
	}
	if inWord {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return strings.Fields(s)
	}
	return out
}
