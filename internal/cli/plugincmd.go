package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/pluginfetch"
	"github.com/umairge66/gocode/internal/pluginmgr"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// This file implements the non-interactive `gocode plugin <subcommand>` surface
// and the shared Manager builder used by both the subcommand and the session
// wiring. The subcommands and their one-line descriptions:
//
//	plugin marketplace add <source>     Add a marketplace from a URL, path, or GitHub repo
//	plugin marketplace list             List all configured marketplaces
//	plugin marketplace remove <name>    Remove a configured marketplace
//	plugin marketplace update [name]    Update marketplace(s) from their source
//	plugin install <plugin>             Install a plugin from a marketplace
//	plugin list                         List installed plugins
//	plugin enable <plugin>              Enable a disabled plugin
//	plugin disable [plugin]             Disable an enabled plugin
//	plugin uninstall <plugin>           Uninstall an installed plugin
//
// Everything drives internal/pluginmgr.Manager, which writes the SAME plugin
// cache + enabledPlugins settings the session discovery reads at startup. A
// plugin installed/enabled here is loaded on the next run ("restart required
// to apply"), so install/enable print a
// re-run hint rather than hot-reloading the session. There is NO telemetry: the
// only network access is the user-initiated fetch of a marketplace/plugin
// source through internal/pluginfetch. The default marketplace list is empty.

// newPluginManager builds the session's plugin Manager from the resolved config
// home: its cache is plugins.PluginCacheDir(configHome) (the exact tree
// discoverPlugins scans) and its settings file is the user settings.json that
// holds enabledPlugins. The fetcher is the real pluginfetch.Fetcher (used only
// when a non-local source is added/installed). It returns nil when no user
// config home can be resolved (e.g. a diskless session), so callers degrade
// gracefully instead of panicking (pluginmgr.New panics on an empty cache dir).
func newPluginManager(env *Env, paths sessionPaths) *pluginmgr.Manager {
	if paths.configDir == "" {
		return nil
	}
	settingsPath := ""
	if p, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser); ok {
		settingsPath = p
	}
	// Policy zero value allows every source (the enterprise allow/block policy is
	// not applied here; see the Config.Policy doc). Fetcher is the real one.
	return pluginmgr.NewFromConfigHome(paths.configDir, settingsPath, pluginfetch.New(), plugins.Policy{})
}

// runPluginCmd is the `gocode plugin <subcommand>` entrypoint. It mirrors the
// auth/mcp subcommand dispatch (same place in runMain). `plugin eval` and
// `plugin test` are handled elsewhere (the dev/eval path and the Run() fast
// path respectively) and are not claimed here.
func runPluginCmd(env *Env, d *DispatchDefaults, rest []string) int {
	args := positionalsAfter(rest, "plugin")
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	// Leave the dev-only eval/test surfaces to their existing handlers.
	switch sub {
	case "eval":
		return runPluginEval(env, d, rest)
	case "test":
		// Reached only when options precede `plugin` (the fast path in Run()
		// catches `gocode plugin test ...`).
		fmt.Fprintln(env.Stderr, "gocode plugin test: not run from this spelling. Use `gocode plugin test [dir]`, with no options before `plugin`")
		return 1
	}

	m := newPluginManager(env, resolveSessionPaths(env))
	if m == nil {
		fmt.Fprintln(env.Stderr, "gocode plugin: could not resolve the user config directory (set GOCODE_CONFIG_DIR, XDG_CONFIG_HOME or HOME).")
		return 1
	}
	ctx := context.Background()

	switch sub {
	case "marketplace":
		return runPluginMarketplace(env, m, args[1:], rest)
	case "install":
		return runPluginInstall(ctx, env, m, args[1:], rest)
	case "list":
		return runPluginList(env, m, rest)
	case "enable":
		return runPluginEnable(env, m, args[1:])
	case "disable":
		return runPluginDisable(env, m, args[1:])
	case "uninstall":
		return runPluginUninstall(env, m, args[1:])
	case "":
		fmt.Fprintln(env.Stderr, pluginUsage)
		return 1
	default:
		fmt.Fprintf(env.Stderr, "gocode plugin: unknown subcommand %q.\n", sub)
		fmt.Fprintln(env.Stderr, pluginUsage)
		return 1
	}
}

const pluginUsage = `Usage: gocode plugin <command>

Marketplaces:
  marketplace add <source>     Add a marketplace from a URL, path, or GitHub repo
  marketplace list             List all configured marketplaces
  marketplace remove <name>    Remove a configured marketplace
  marketplace update [name]    Update marketplace(s) from their source

Plugins:
  install <plugin>[@market]    Install a plugin from a marketplace
  list                         List installed plugins
  enable <plugin>              Enable a disabled plugin
  disable <plugin>             Disable an enabled plugin
  uninstall <plugin>           Uninstall an installed plugin

Development:
  eval [target]                Run a plugin's eval suite and report scored results
  eval init --bare <name>      Write a blank eval case under the eval dir
  test [dir]                   Run a mod's tests`

// --- marketplace subcommands -----------------------------------------------

func runPluginMarketplace(env *Env, m *pluginmgr.Manager, args, rest []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		if len(args) < 2 {
			fmt.Fprintln(env.Stderr, "gocode plugin marketplace add: a source is required (owner/repo, a git URL, or a local path).")
			return 1
		}
		mkt, err := m.AddMarketplace(context.Background(), args[1])
		if err != nil {
			fmt.Fprintf(env.Stderr, "gocode plugin marketplace add: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "Added marketplace %q (%d plugin(s)).\n", mkt.Name, len(mkt.Plugins))
		return 0
	case "list", "":
		return runPluginMarketplaceList(env, m)
	case "remove":
		if len(args) < 2 {
			fmt.Fprintln(env.Stderr, "gocode plugin marketplace remove: a marketplace name is required.")
			return 1
		}
		if err := m.RemoveMarketplace(args[1], hasFlag(rest, "--force")); err != nil {
			fmt.Fprintf(env.Stderr, "gocode plugin marketplace remove: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "Removed marketplace %q.\n", args[1])
		return 0
	case "update":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return runPluginMarketplaceUpdate(env, m, name)
	default:
		fmt.Fprintf(env.Stderr, "gocode plugin marketplace: unknown subcommand %q (add, list, remove, update).\n", sub)
		return 1
	}
}

func runPluginMarketplaceList(env *Env, m *pluginmgr.Manager) int {
	list, err := m.ListMarketplaces()
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin marketplace list: %v\n", err)
		return 1
	}
	fmt.Fprintln(env.Stdout, marketplaceListText(list))
	return 0
}

func runPluginMarketplaceUpdate(env *Env, m *pluginmgr.Manager, name string) int {
	if name != "" {
		mkt, err := m.UpdateMarketplace(context.Background(), name)
		if err != nil {
			fmt.Fprintf(env.Stderr, "gocode plugin marketplace update: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "Updated marketplace %q (%d plugin(s)).\n", mkt.Name, len(mkt.Plugins))
		return 0
	}
	// No name: update every configured marketplace ("updates all if no name
	// specified").
	list, err := m.ListMarketplaces()
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin marketplace update: %v\n", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(env.Stdout, "No marketplaces configured.")
		return 0
	}
	rc := 0
	for _, info := range list {
		if _, err := m.UpdateMarketplace(context.Background(), info.Name); err != nil {
			fmt.Fprintf(env.Stderr, "gocode plugin marketplace update %q: %v\n", info.Name, err)
			rc = 1
			continue
		}
		fmt.Fprintf(env.Stdout, "Updated marketplace %q.\n", info.Name)
	}
	return rc
}

// --- plugin subcommands -----------------------------------------------------

func runPluginInstall(ctx context.Context, env *Env, m *pluginmgr.Manager, args, rest []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "gocode plugin install: a plugin is required (name or name@marketplace).")
		return 1
	}
	ref := args[0]

	// `--marketplace <source>` adds the source first, then installs from it.
	if src := flagValue(rest, "--marketplace"); src != "" {
		mktName, err := ensureMarketplace(ctx, m, src)
		if err != nil {
			fmt.Fprintf(env.Stderr, "gocode plugin install: %v\n", err)
			return 1
		}
		if !strings.Contains(ref, "@") {
			ref = ref + "@" + mktName
		}
	}

	plan, err := m.PlanInstall(ctx, ref)
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin install: %v\n", err)
		return 1
	}
	// Surface the executable contributions (hooks, MCP servers) before writing —
	// non-interactively, we print them so the user sees what the install brings
	// in.
	if txt := planContributionsText(plan); txt != "" {
		fmt.Fprintln(env.Stdout, txt)
	}
	if err := m.Install(ctx, plan); err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin install: %v\n", err)
		return 1
	}
	if w := plan.Dependencies.Error; w != "" {
		fmt.Fprintf(env.Stderr, "Plugin dependency install warning for %s: %s\n", plan.ID, w)
	}
	fmt.Fprintf(env.Stdout, "Installed and enabled %s%s.\n", plan.ID, versionSuffixText(plan.Version))
	fmt.Fprintln(env.Stdout, pluginRestartHint)
	return 0
}

func runPluginList(env *Env, m *pluginmgr.Manager, rest []string) int {
	installed, err := m.ListInstalled()
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin list: %v\n", err)
		return 1
	}
	enabled, err := m.EnabledPlugins()
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin list: %v\n", err)
		return 1
	}
	only := ""
	if hasFlag(rest, "--enabled") {
		only = "enabled"
	} else if hasFlag(rest, "--disabled") {
		only = "disabled"
	}
	fmt.Fprintln(env.Stdout, installedListText(installed, enabled, only))
	return 0
}

func runPluginEnable(env *Env, m *pluginmgr.Manager, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "gocode plugin enable: a plugin id is required.")
		return 1
	}
	id := resolveInstalledID(m, args[0])
	if err := m.Enable(id); err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin enable: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Enabled %s.\n", id)
	fmt.Fprintln(env.Stdout, pluginRestartHint)
	return 0
}

func runPluginDisable(env *Env, m *pluginmgr.Manager, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "gocode plugin disable: a plugin id is required.")
		return 1
	}
	id := resolveInstalledID(m, args[0])
	if err := m.Disable(id); err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin disable: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Disabled %s.\n", id)
	fmt.Fprintln(env.Stdout, pluginRestartHint)
	return 0
}

func runPluginUninstall(env *Env, m *pluginmgr.Manager, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "gocode plugin uninstall: a plugin id is required.")
		return 1
	}
	id := resolveInstalledID(m, args[0])
	if err := m.Uninstall(id); err != nil {
		fmt.Fprintf(env.Stderr, "gocode plugin uninstall: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Uninstalled %s.\n", id)
	return 0
}

// --- shared helpers ---------------------------------------------------------

const pluginRestartHint = "Restart gocode (or start a new session) to load the plugin's skills, commands and agents."

// ensureMarketplace returns the marketplace name for a `--marketplace` value:
// if it is already a registered name, use it; otherwise add it as a source and
// return the added marketplace's name.
func ensureMarketplace(ctx context.Context, m *pluginmgr.Manager, spec string) (string, error) {
	list, err := m.ListMarketplaces()
	if err != nil {
		return "", err
	}
	for _, info := range list {
		if info.Name == spec {
			return info.Name, nil
		}
	}
	mkt, err := m.AddMarketplace(ctx, spec)
	if err != nil {
		return "", err
	}
	return mkt.Name, nil
}

// resolveInstalledID lets the user pass a bare plugin name when exactly one
// installed plugin has that name; otherwise the token is used verbatim (so an
// explicit name@marketplace always works).
func resolveInstalledID(m *pluginmgr.Manager, token string) string {
	if strings.Contains(token, "@") {
		return token
	}
	installed, err := m.ListInstalled()
	if err != nil {
		return token
	}
	var matches []string
	for _, p := range installed {
		name := p.ID
		if i := strings.LastIndex(name, "@"); i > 0 {
			name = name[:i]
		}
		if name == token {
			matches = append(matches, p.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return token
}

// marketplaceListText renders the configured marketplaces (wording:
// "Configured marketplaces:" / "No marketplaces configured.").
func marketplaceListText(list []pluginmgr.MarketplaceInfo) string {
	if len(list) == 0 {
		return "No marketplaces configured."
	}
	var b strings.Builder
	b.WriteString("Configured marketplaces:\n")
	for _, info := range list {
		n := 0
		if info.Index != nil {
			n = len(info.Index.Plugins)
		}
		fmt.Fprintf(&b, "  %s (%d plugin(s)) — %s\n", info.Name, n, info.Entry.Source.Canonical())
	}
	return strings.TrimRight(b.String(), "\n")
}

// installedListText renders the installed plugins with their enabled state.
// only filters to "enabled"/"disabled" (empty shows all).
func installedListText(installed []pluginmgr.InstalledPlugin, enabled map[string]bool, only string) string {
	var rows []string
	for _, p := range installed {
		on := enabled[p.ID] // missing → false (installed-but-not-enabled)
		switch only {
		case "enabled":
			if !on {
				continue
			}
		case "disabled":
			if on {
				continue
			}
		}
		state := "disabled"
		if on {
			state = "enabled"
		}
		rows = append(rows, fmt.Sprintf("  %s [%s]%s", p.ID, state, versionSuffixText(p.Record.Version)))
	}
	if len(rows) == 0 {
		return "No plugins installed."
	}
	sort.Strings(rows)
	return "Installed plugins:\n" + strings.Join(rows, "\n")
}

// planContributionsText lists a plan's contributed resources and — prominently —
// its executable contributions (hook commands, MCP servers).
func planContributionsText(plan *pluginmgr.Plan) string {
	var b strings.Builder
	if n := len(plan.Commands) + len(plan.Agents) + len(plan.Skills); n > 0 {
		var parts []string
		if len(plan.Commands) > 0 {
			parts = append(parts, fmt.Sprintf("%d command(s)", len(plan.Commands)))
		}
		if len(plan.Agents) > 0 {
			parts = append(parts, fmt.Sprintf("%d agent(s)", len(plan.Agents)))
		}
		if len(plan.Skills) > 0 {
			parts = append(parts, fmt.Sprintf("%d skill(s)", len(plan.Skills)))
		}
		fmt.Fprintf(&b, "%s contributes %s.\n", plan.ID, strings.Join(parts, ", "))
	}
	for _, h := range plan.Hooks {
		fmt.Fprintf(&b, "  hook (%s): %s\n", h.Event, h.Command)
	}
	for _, s := range plan.MCPServers {
		if s.Command != "" {
			fmt.Fprintf(&b, "  MCP server %q runs: %s\n", s.Name, s.Command)
		} else if s.URL != "" {
			fmt.Fprintf(&b, "  MCP server %q connects to: %s\n", s.Name, s.URL)
		}
	}
	if len(plan.Hooks) > 0 || len(plan.MCPServers) > 0 {
		b.WriteString("These run on your machine when the plugin is active.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func versionSuffixText(v string) string {
	if v == "" {
		return ""
	}
	return " v" + v
}

// positionalsAfter returns the non-flag tokens that follow the first occurrence
// of marker (exclusive). It does not try to associate flag values, so a flag's
// value may appear here too; callers that need a flag value use flagValue.
func positionalsAfter(tokens []string, marker string) []string {
	i := indexOf(tokens, marker)
	if i < 0 {
		return nil
	}
	var out []string
	skipNext := false
	for _, t := range tokens[i+1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(t, "-") {
			// `--marketplace <v>` consumes the next token as its value.
			if (t == "--marketplace" || t == "-m") && !strings.Contains(t, "=") {
				skipNext = true
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// flagValue returns the value of `--name <value>` or `--name=<value>`, or "".
func flagValue(tokens []string, name string) string {
	for i, t := range tokens {
		if t == name && i+1 < len(tokens) {
			return tokens[i+1]
		}
		if strings.HasPrefix(t, name+"=") {
			return t[len(name)+1:]
		}
	}
	return ""
}

// hasFlag reports whether tokens contains the exact flag.
func hasFlag(tokens []string, name string) bool { return indexOf(tokens, name) != -1 }
