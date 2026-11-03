package cli

import (
	"strings"
)

// DispatchDefaults holds the global dispatch defaults parsed off the argument
// vector before a session is created.
type DispatchDefaults struct {
	PermissionMode          string
	PermissionModeInherited bool
	Model                   string
	// Provider is the `--provider <openrouter|anthropic|openai>` selection
	// ("" when the flag is absent; env / settings / default then apply).
	Provider    string
	Effort      string
	Agent       string
	AllowBypass bool
	// FallbackModel is the raw `--fallback-model <model>` value (a
	// comma-separated list; see provider.ParseFallbackModels).
	FallbackModel string
	// AllowedTools / DisallowedTools are the raw `--allowedTools <tools...>` /
	// `--disallowedTools <tools...>` values (variadic; each may itself hold
	// several comma/space-separated rules, see splitToolSpecs). They become
	// "cliArg" permission rules (cliPermissionRules).
	AllowedTools    []string
	DisallowedTools []string

	// Root-config flags threaded into the session (set by runMain from the
	// root pre-parse): --mcp-config values (JSON or file paths),
	// --strict-mcp-config, and --plugin-dir / --plugin-dir-no-mcp
	// directories.
	MCPConfig       []string
	StrictMCPConfig bool
	// Chrome is the --chrome / --no-chrome choice ("on", "off" or "").
	Chrome          string
	PluginDirs      []string
	PluginDirsNoMCP []string
	// AddDirs are the --add-dir directories (as given; resolved against the
	// cwd when the session is built).
	AddDirs []string

	// Session-persistence / resume flags.
	Continue             bool   // -c/--continue: resume the latest session for the cwd (no picker)
	Resume               bool   // -r/--resume present
	ResumeValue          string // the optional [value]: a session id, name, or picker search term
	SessionID            string // --session-id <uuid>: use this id for a fresh session
	ForkSession          bool   // --fork-session: resume into a NEW session id (don't append to the original)
	NoSessionPersistence bool   // --no-session-persistence: never write a transcript

	// --max-turns <turns> (print mode): the raw value and whether the flag was
	// given; MaxTurnsLimit is the validated limit dispatch hands to the print
	// session (0 = none).
	MaxTurns      string
	MaxTurnsSet   bool
	MaxTurnsLimit int

	// --max-budget-usd <amount>: the raw value and whether the flag was
	// given; MaxBudgetUSDLimit is the validated cap dispatch hands to the
	// session (0 = none; the maxSessionCostUsd setting and
	// GOCODE_MAX_SESSION_COST_USD then apply). See maxbudget.go.
	MaxBudgetUSD      string
	MaxBudgetUSDSet   bool
	MaxBudgetUSDLimit float64
}

// stripDebugFlags removes the debug options (--debug[=filter], -d,
// --debug-to-stderr, -d2e, --debug-file <path>, --debug-file=<path>) that
// appear before a "--" terminator, so neither they nor a --debug-file path
// are read as a prompt or subcommand. It reports whether any were present.
func stripDebugFlags(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for i := 0; i < len(args); i++ {
		t := args[i]
		if t == "--" {
			out = append(out, args[i:]...)
			break
		}
		if t == "--debug" || t == "-d" || t == "--debug-to-stderr" || t == "-d2e" ||
			strings.HasPrefix(t, "--debug=") || strings.HasPrefix(t, "--debug-file=") {
			found = true
			continue
		}
		if t == "--debug-file" {
			found = true
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, t)
	}
	return out, found
}

// parseDispatchDefaults extracts the dispatch-default flags from args and
// returns the defaults (nil when none were present) together with the
// remaining, unconsumed arguments.
func parseDispatchDefaults(args []string) (*DispatchDefaults, []string) {
	var permMode, inheritMode, model, provider, effort, agent, fallbackModel string
	var allowBypass bool
	var cont, resume, fork, noPersist bool
	var resumeVal, sessionID string
	var maxTurns, maxBudget string
	var maxTurnsSet, maxBudgetSet bool
	var allowedTools, disallowedTools []string
	var rest []string

	for i := 0; i < len(args); i++ {
		x := args[i]
		var key, val string
		hasVal := false
		if c := strings.Index(x, "="); c > 0 {
			key, val, hasVal = x[:c], x[c+1:], true
		} else {
			key = x
		}
		// A value is available either inline (=) or as the next token.
		avail := hasVal || i+1 < len(args)
		next := func() string {
			if hasVal {
				return val
			}
			i++
			return args[i]
		}
		switch {
		case key == "--dangerously-skip-permissions":
			permMode = "bypassPermissions"
		case key == "--allow-dangerously-skip-permissions":
			allowBypass = true
		case key == "--permission-mode" && avail:
			permMode = next()
		case key == "--inherit-permission-mode" && avail:
			inheritMode = next()
		case key == "--model" && avail:
			model = next()
		case key == "--provider" && avail:
			provider = next()
		case key == "--effort" && avail:
			effort = next()
		case key == "--fallback-model" && avail:
			fallbackModel = next()
		case key == "--agent" && avail:
			agent = next()
		case (key == "--allowedTools" || key == "--allowed-tools" || key == "--disallowedTools" || key == "--disallowed-tools") && avail:
			// Variadic (`<tools...>`): an inline =value, or every
			// following token up to the next option.
			var vals []string
			if hasVal {
				vals = append(vals, val)
			} else {
				for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					vals = append(vals, args[i])
				}
			}
			if strings.HasPrefix(key, "--allowed") {
				allowedTools = append(allowedTools, vals...)
			} else {
				disallowedTools = append(disallowedTools, vals...)
			}
		case key == "-c" || key == "--continue":
			cont = true
		case key == "-r" || key == "--resume":
			resume = true
			// Optional value: an inline =value, or the next token when it is not
			// itself a flag (`[value]` semantics). A bare --resume opens
			// the picker (interactive) or needs an explicit id (-p).
			if hasVal {
				resumeVal = val
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				resumeVal = args[i]
			}
		case key == "--session-id" && avail:
			sessionID = next()
		case key == "--max-turns" && avail:
			maxTurns, maxTurnsSet = next(), true
		case key == "--max-budget-usd" && avail:
			maxBudget, maxBudgetSet = next(), true
		case key == "--fork-session":
			fork = true
		case key == "--no-session-persistence":
			noPersist = true
		default:
			rest = append(rest, x)
		}
	}

	mode := permMode
	if mode == "" {
		mode = inheritMode
	}
	if mode == "" && model == "" && provider == "" && effort == "" && agent == "" && fallbackModel == "" &&
		!allowBypass && !cont && !resume && !fork && !noPersist && sessionID == "" && !maxTurnsSet && !maxBudgetSet &&
		len(allowedTools) == 0 && len(disallowedTools) == 0 {
		return nil, rest
	}
	d := &DispatchDefaults{
		PermissionMode:       mode,
		Model:                model,
		Provider:             provider,
		Effort:               effort,
		FallbackModel:        fallbackModel,
		AllowedTools:         allowedTools,
		DisallowedTools:      disallowedTools,
		Agent:                agent,
		AllowBypass:          allowBypass,
		Continue:             cont,
		Resume:               resume,
		ResumeValue:          resumeVal,
		SessionID:            sessionID,
		ForkSession:          fork,
		NoSessionPersistence: noPersist,
		MaxTurns:             maxTurns,
		MaxTurnsSet:          maxTurnsSet,
		MaxBudgetUSD:         maxBudget,
		MaxBudgetUSDSet:      maxBudgetSet,
	}
	if permMode == "" && inheritMode != "" {
		d.PermissionModeInherited = true
	}
	return d, rest
}

// RootConfig captures the globally-recognized settings/plugin flags that are
// parsed ahead of subcommand routing.
type RootConfig struct {
	AddDir          []string
	PluginDir       []string
	PluginDirNoMcp  []string
	Settings        string
	ManagedSettings string
	McpConfig       []string
	StrictMcpConfig bool
	Restricted      bool
	// Chrome is "on" for --chrome, "off" for --no-chrome, "" when neither
	// was given (the last one wins).
	Chrome string
}

// RootParse is the result of the root pre-parse.
type RootParse struct {
	HasAgentsPositional bool
	CwdFilter           string // empty when unset
	CwdFilterSet        bool
	Config              RootConfig
	Rest                []string
}

// parseRootConfig pulls the global config flags (and the first bare "agents"
// positional) out of args with a hand-rolled scanner.
func parseRootConfig(args []string) RootParse {
	r := RootParse{}
	cfg := &r.Config

	setters := map[string]func(string){
		"--cwd":               func(v string) { r.CwdFilter, r.CwdFilterSet = v, true },
		"--settings":          func(v string) { cfg.Settings = v },
		"--managed-settings":  func(v string) { cfg.ManagedSettings = v },
		"--setting-sources":   func(string) {},
		"--add-dir":           func(v string) { cfg.AddDir = append(cfg.AddDir, v) },
		"--plugin-dir":        func(v string) { cfg.PluginDir = append(cfg.PluginDir, v) },
		"--plugin-dir-no-mcp": func(v string) { cfg.PluginDirNoMcp = append(cfg.PluginDirNoMcp, v) },
		"--mcp-config":        func(v string) { cfg.McpConfig = append(cfg.McpConfig, v) },
	}

	for i := 0; i < len(args); i++ {
		s := args[i]
		if s == "agents" && !r.HasAgentsPositional {
			r.HasAgentsPositional = true
			continue
		}
		if s == "--strict-mcp-config" {
			cfg.StrictMcpConfig = true
			continue
		}
		if s == "--restricted" {
			cfg.Restricted = true
			continue
		}
		if s == "--chrome" || s == "--no-chrome" {
			cfg.Chrome = map[bool]string{true: "on", false: "off"}[s == "--chrome"]
			continue
		}
		key := s
		eq := strings.Index(s, "=")
		if eq != -1 {
			key = s[:eq]
		}
		set, ok := setters[key]
		if ok {
			if eq != -1 {
				set(s[eq+1:])
			} else if i+1 < len(args) {
				i++
				set(args[i])
			} else {
				r.Rest = append(r.Rest, s)
			}
			continue
		}
		r.Rest = append(r.Rest, s)
	}
	return r
}

// --- Subcommand classifiers ----------------------------------------------

// isMcpServeInvocation reports whether args is an `mcp serve ...` invocation.
func isMcpServeInvocation(args []string) bool {
	n := indexOf(args, "mcp")
	return n != -1 && n+1 < len(args) && args[n+1] == "serve"
}

// --- Daemon detection ----------------------------------------------------

// daemonArgs detects a daemon invocation: after skipping leading
// --dangerously-skip-permissions / --allow-dangerously-skip-permissions flags,
// if the next token is "daemon" it returns the remaining args; otherwise nil.
func daemonArgs(args []string) ([]string, bool) {
	e := 0
	for e < len(args) && (args[e] == "--dangerously-skip-permissions" || args[e] == "--allow-dangerously-skip-permissions") {
		e++
	}
	if e < len(args) && args[e] == "daemon" {
		return args[e+1:], true
	}
	return nil, false
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func contains(s []string, v string) bool { return indexOf(s, v) != -1 }
