package cli

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/textutil"
	"github.com/umairge66/gocode/internal/updater"
)

// `gocode doctor`: check the health of the installation and configuration and
// print a plain-text report. It reads settings files in the current directory
// without a trust prompt, runs no hooks, helpers or MCP servers, makes no
// network call and sends nothing anywhere. Exit code 0 whatever it finds.
//
// gocode has no account service or background updater (it updates from GitHub
// Releases with `gocode update`) and ships as a single Go binary, so the
// report has no remote-policy, updater-channel or package-manager sections.
//
// Also reported, in the same "- item" style: the provider and which API keys
// were found (never their values), MCP configuration problems, plugin
// problems, permission-rule problems (listed with the invalid-settings
// entries) and the Chrome native-messaging host status (the
// `gocode chrome status` logic).

// doctorSeams are the environment probes, injectable for tests.
type doctorSeams struct {
	executable func() (string, error) // os.Executable
	goos       string
	goarch     string
	chrome     func(env *Env) (string, bool) // chrome host report + ok
}

func defaultDoctorSeams() doctorSeams {
	return doctorSeams{
		executable: os.Executable,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		chrome:     doctorChromeStatus,
	}
}

// doctorIssue is one installation warning.
type doctorIssue struct {
	Issue string
	Fix   string
}

// runDoctorCmd is the command action.
func runDoctorCmd(env *Env, d *DispatchDefaults) int {
	return runDoctor(env, d, defaultDoctorSeams())
}

func runDoctor(env *Env, d *DispatchDefaults, seams doctorSeams) int {
	if d == nil {
		d = &DispatchDefaults{}
	}
	fsys := fsfacade.OSFS{}
	ctx := settingsContext(env)
	ordered := settingsload.EffectiveSources(fsys, ctx, settingsload.CanonicalSourceOrder)
	merged := map[string]any{}
	for _, or := range ordered {
		if or.Result.LoadState == settingspolicy.LoadLoaded {
			for k, v := range or.Result.Settings {
				merged[k] = v
			}
		}
	}

	var warnings []doctorIssue
	lines := []string{"gocode doctor", ""}

	// Installation.
	invoked, _ := seams.executable()
	installPath := invoked
	if resolved, err := filepath.EvalSymlinks(invoked); err == nil {
		installPath = resolved
	}
	lines = append(lines, fmt.Sprintf("Running: native (%s)", doctorClean(updateCurrentVersion())))
	if Build.GitSHA != "" {
		sha := Build.GitSHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		lines = append(lines, "Commit: "+sha)
	}
	lines = append(lines, "Platform: "+doctorPlatform(seams.goos, seams.goarch))
	if installPath == "" {
		installPath = "unknown"
	}
	lines = append(lines, "Path: "+doctorClean(installPath))
	if invoked != "" && invoked != installPath {
		lines = append(lines, "Invoked: "+doctorClean(invoked))
	}
	if dir, ok := ctx.UserConfigDir(); ok {
		lines = append(lines, "Data directory: "+doctorClean(dir))
		if left := confighome.LegacyLeftovers(ctx.HomeDir); len(left) > 0 {
			warnings = append(warnings, doctorIssue{
				Issue: fmt.Sprintf("The former data directory %s still holds %s, which gocode no longer reads", confighome.LegacyDir(ctx.HomeDir), strings.Join(left, ", ")),
				Fix:   "Those names already exist in " + dir + " (or GOCODE_CONFIG_DIR is set); compare the two and move or delete the leftovers by hand",
			})
		}
	} else {
		lines = append(lines, "Data directory: none (set HOME, XDG_CONFIG_HOME or GOCODE_CONFIG_DIR)")
	}
	lines = append(lines, "Search: OK ("+doctorClean(searchEngineSummary(env))+")")
	repo, _, rerr := updater.ResolveRepo(envGetenv(env), settingString(merged, updater.RepoSettingsKey))
	if rerr == nil && repo != "" {
		lines = append(lines, fmt.Sprintf("Updates: manual, from GitHub Releases of %s (run gocode update)", doctorClean(repo)))
	} else {
		lines = append(lines, fmt.Sprintf("Updates: release repository not configured (set %s or the %q setting)", updater.RepoEnvVar, updater.RepoSettingsKey))
	}

	// Provider and API keys (presence and source only — never the value).
	active, perr := resolveProvider(env, d)
	if perr != nil {
		lines = append(lines, "Provider: invalid — "+doctorClean(perr.Error()))
	} else {
		lines = append(lines, "Provider: "+active)
	}
	resolver := newKeyResolver(env)
	activeKey := false
	for _, p := range validProviders() {
		_, source, ok := resolver.ResolveProviderKey(auth.Provider(p))
		if ok {
			lines = append(lines, fmt.Sprintf("API key (%s): found (source: %s)", p, doctorClean(source)))
		} else {
			lines = append(lines, fmt.Sprintf("API key (%s): not set", p))
		}
		if p == active && ok {
			activeKey = true
		}
	}
	if perr == nil && !activeKey {
		envName := strings.Join(auth.ProviderEnvVars(auth.Provider(active)), "/")
		warnings = append(warnings, doctorIssue{
			Issue: fmt.Sprintf("No API key for the active provider (%s)", active),
			Fix:   fmt.Sprintf("Set %s, add it under the \"apiKeys\" setting, or run /login in a session", envName),
		})
	}
	// Configured provider endpoints (credentials in the URL are masked).
	for _, p := range validProviders() {
		bu := resolveProviderBaseURL(env, p)
		if bu.configured() {
			lines = append(lines, fmt.Sprintf("%s: %s", provider.BaseURLLabel(p), doctorClean(bu.display())))
		}
		if bu.Err != nil {
			warnings = append(warnings, doctorIssue{
				Issue: doctorClean(fmt.Sprintf("Invalid %s base URL: %v", p, bu.Err)),
				Fix:   fmt.Sprintf("Set %s to an absolute http(s) URL, or unset it to use %s", bu.EnvVar, provider.DefaultBaseURL(p)),
			})
		}
		for _, ign := range bu.Ignored {
			warnings = append(warnings, doctorIssue{
				Issue: doctorClean(fmt.Sprintf("%s is ignored: a project or local settings file cannot redirect the %s API key", ign, p)),
				Fix:   "Set it in the environment or in your user settings \"env\" block instead",
			})
		}
	}

	// Invalid settings (load errors, schema warnings, permission rules).
	invalid, notices := doctorSettingsProblems(fsys, ctx)
	if len(invalid) > 0 {
		lines = append(lines, "", "Invalid settings")
		lines = append(lines, invalid...)
	}
	if len(notices) > 0 {
		lines = append(lines, "")
		lines = append(lines, notices...)
	}

	// Environment variables.
	if envIssues := doctorEnvVars(env, active, loadModelSetting(env), d.Model); len(envIssues) > 0 {
		lines = append(lines, "", "Environment variables")
		lines = append(lines, envIssues...)
	}

	// Other installations on PATH, and whether this one is on PATH.
	others, onPath := doctorInstallations(envGetenv(env)("PATH"), installPath, seams.goos)
	if len(others) > 1 {
		lines = append(lines, "", "Multiple installations found")
		for _, o := range others {
			lines = append(lines, "- native at "+doctorClean(o))
		}
	}
	if installPath != "unknown" && !onPath {
		warnings = append(warnings, doctorPathIssue(filepath.Dir(installPath), seams.goos, envGetenv(env)))
	}

	// MCP configuration.
	if mcp := doctorMCPProblems(env, d); len(mcp) > 0 {
		lines = append(lines, "", "MCP servers")
		lines = append(lines, mcp...)
	}

	// Plugins.
	if pl := doctorPluginProblems(env, d, ordered); len(pl) > 0 {
		lines = append(lines, "", "Plugins")
		lines = append(lines, pl...)
	}

	// Chrome native-messaging host (`gocode chrome status`).
	if seams.chrome != nil {
		if report, ok := seams.chrome(env); report != "" {
			state := "installed"
			if !ok {
				state = "not installed"
			}
			lines = append(lines, "", "Chrome native host: "+state)
			for _, l := range strings.Split(strings.TrimRight(report, "\n"), "\n") {
				lines = append(lines, "  "+strings.TrimSpace(l))
			}
		}
	}

	if len(warnings) > 0 {
		lines = append(lines, "", fmt.Sprintf("%d %s found", len(warnings), pluralize(len(warnings), "warning", "warnings")))
		for _, w := range warnings {
			lines = append(lines, "- "+doctorClean(w.Issue), "  Fix: "+doctorClean(w.Fix))
		}
	} else {
		lines = append(lines, "", "No installation issues found.")
	}
	lines = append(lines, "", "For a full setup checkup that can also fix issues, run /doctor in a gocode session.")
	fmt.Fprint(env.Stdout, strings.Join(lines, "\n")+"\n")
	return 0
}

// doctorClean strips control characters and backticks from an
// interpolated value.
func doctorClean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "`", "")
}

// doctorPlatform renders "<platform>-<arch>" with Node-style names
// (linux-x64, macos-arm64, windows-x64).
func doctorPlatform(goos, goarch string) string {
	p := goos
	if goos == "darwin" {
		p = "macos"
	}
	a := goarch
	switch goarch {
	case "amd64":
		a = "x64"
	case "386":
		a = "ia32"
	}
	return p + "-" + a
}

// doctorSettingsProblems lists invalid-settings entries ("- file › path:
// message") and permission-rule notices for every settings source.
func doctorSettingsProblems(fsys fsfacade.FS, ctx *settingsload.Context) (invalid, notices []string) {
	all := settingsload.LoadAll(fsys, ctx)
	sources := make([]string, 0, len(all))
	for src := range all {
		sources = append(sources, string(src))
	}
	sort.Strings(sources)
	for _, s := range sources {
		src := settingspolicy.Source(s)
		res := all[src]
		file, _ := ctx.PathForSource(src)
		entry := func(path, msg string) string {
			var parts []string
			for _, p := range []string{file, path} {
				if p != "" {
					parts = append(parts, doctorClean(p))
				}
			}
			head := strings.Join(parts, " › ")
			if head != "" {
				head += ": "
			}
			return "- " + head + doctorClean(msg)
		}
		for _, e := range res.Errors {
			f := file
			if e.File != "" {
				f = e.File
			}
			var parts []string
			for _, p := range []string{f, e.Path} {
				if p != "" {
					parts = append(parts, doctorClean(p))
				}
			}
			head := strings.Join(parts, " › ")
			if head != "" {
				head += ": "
			}
			invalid = append(invalid, "- "+head+doctorClean(e.Message))
		}
		for _, w := range res.Warnings {
			invalid = append(invalid, entry("", w))
		}
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		perms, _ := res.Settings["permissions"].(map[string]any)
		for _, behavior := range []string{"allow", "deny", "ask"} {
			list, _ := perms[behavior].([]any)
			for i, item := range list {
				rule, isStr := item.(string)
				if !isStr {
					continue
				}
				p := validatePermissionRule(rule, behavior)
				if p == nil {
					continue
				}
				if p.Warning {
					notices = append(notices, "- "+doctorClean(p.message()))
					continue
				}
				invalid = append(invalid, entry(fmt.Sprintf("permissions.%s.%d", behavior, i), p.message()))
			}
		}
	}
	return invalid, notices
}

// bashMaxOutputDefault / bashMaxOutputLimit are BASH_MAX_OUTPUT_LENGTH's
// default and ceiling.
const (
	bashMaxOutputDefault = 30000
	bashMaxOutputLimit   = 150000
)

// envVarCheck returns status "valid", "invalid" or "capped" with a
// user-facing message. upper <= 0 means no ceiling.
func envVarCheck(raw string, def, upper int) (status, message string) {
	if raw == "" {
		return "valid", ""
	}
	n := textutil.ParseGroupedNumber(raw)
	if math.IsNaN(n) || n <= 0 {
		return "invalid", fmt.Sprintf("Invalid value %q (using default: %s)", raw, envDefaultText(def))
	}
	if upper > 0 && n > float64(upper) {
		return "capped", fmt.Sprintf("Capped from %s to %d", strconv.FormatFloat(n, 'f', -1, 64), upper)
	}
	return "valid", ""
}

func envDefaultText(def int) string {
	if def <= 0 {
		return "the provider default"
	}
	return strconv.Itoa(def)
}

// doctorEnvVars checks BASH_MAX_OUTPUT_LENGTH and the max-output-tokens
// variable against the active model's capability entry.
func doctorEnvVars(env *Env, prov, settingModel, flagModel string) []string {
	model := settingModel
	if flagModel != "" {
		model = flagModel
	}
	caps := provider.CapabilitiesFor(prov, model)
	getenv := envGetenv(env)
	checks := []struct {
		name       string
		def, upper int
	}{
		{"BASH_MAX_OUTPUT_LENGTH", bashMaxOutputDefault, bashMaxOutputLimit},
		{provider.MaxOutputEnv, caps.MaxOutputTokens, caps.MaxOutputLimit},
	}
	var out []string
	for _, c := range checks {
		if status, msg := envVarCheck(getenv(c.name), c.def, c.upper); status != "valid" {
			out = append(out, fmt.Sprintf("- %s: %s", c.name, doctorClean(msg)))
		}
	}
	return out
}

// doctorInstallations lists every gocode binary on PATH (the running one
// first) and reports whether the running one is reachable through PATH.
func doctorInstallations(pathEnv, self, goos string) (all []string, onPath bool) {
	name := updater.BinaryName(goos)
	seen := map[string]bool{}
	if self != "" && self != "unknown" {
		all = append(all, self)
		seen[self] = true
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() {
			continue
		}
		resolved := cand
		if r, err := filepath.EvalSymlinks(cand); err == nil {
			resolved = r
		}
		if resolved == self {
			onPath = true
		}
		if !seen[resolved] {
			seen[resolved] = true
			all = append(all, resolved)
		}
	}
	return all, onPath
}

// doctorPathIssue builds the PATH warning for the directory
// holding the running binary.
func doctorPathIssue(dir, goos string, getenv func(string) string) doctorIssue {
	if goos == "windows" {
		return doctorIssue{
			Issue: fmt.Sprintf("Native installation exists but %s is not in your PATH", dir),
			Fix:   "Add it by opening: System Properties → Environment Variables → Edit User PATH → New → Add the path above. Then restart your terminal.",
		}
	}
	rc := "your shell config file"
	shell := filepath.Base(getenv("SHELL"))
	switch shell {
	case "zsh":
		rc = "~/.zshrc"
	case "bash":
		rc = "~/.bashrc"
	case "fish":
		rc = "~/.config/fish/config.fish"
	}
	shown := dir
	if home := getenv("HOME"); home != "" && strings.HasPrefix(dir, home) {
		shown = "$HOME" + strings.TrimPrefix(dir, home)
	}
	return doctorIssue{
		Issue: fmt.Sprintf("Native installation exists but %s is not in your PATH", dir),
		Fix:   fmt.Sprintf("Run: echo 'export PATH=\"%s:$PATH\"' >> %s then open a new terminal or run: source %s", shown, rc, rc),
	}
}

// doctorMCPProblems gathers the MCP server configuration offline (no server is
// started) and lists load errors and servers left out by policy or settings.
// Project servers that only await approval are not problems.
func doctorMCPProblems(env *Env, d *DispatchDefaults) []string {
	s := &printSession{MCPConfig: d.MCPConfig, StrictMCPConfig: d.StrictMCPConfig, PluginDirs: d.PluginDirs, PluginDirsNoMCP: d.PluginDirsNoMCP}
	g := gatherMCP(env, s)
	var out []string
	for _, e := range g.Errors {
		out = append(out, "- Configuration error: "+doctorClean(e.Error()))
	}
	_, flagErrs := parseMCPConfigFlags(d.MCPConfig)
	for _, e := range flagErrs {
		out = append(out, "- --mcp-config: "+doctorClean(e.Error()))
	}
	for _, sk := range g.Skipped {
		if sk.Reason == mcphost.ReasonPendingApproval {
			continue
		}
		out = append(out, fmt.Sprintf("- %q (%s) not started: %s", sk.Name, sk.Scope, doctorClean(sk.Reason)))
	}
	return out
}

// doctorPluginProblems lists enabled plugins that are not installed and
// --plugin-dir directories that hold no loadable plugin.
func doctorPluginProblems(env *Env, d *DispatchDefaults, ordered []settingsload.OrderedResult) []string {
	s := &printSession{PluginDirs: d.PluginDirs, PluginDirsNoMCP: d.PluginDirsNoMCP}
	reg, _, _ := discoverPlugins(env, s, resolveSessionPaths(env), ordered)
	var out []string
	enabled := enabledPluginsSetting(ordered)
	ids := make([]string, 0, len(enabled))
	for id, on := range enabled {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if reg != nil {
			if _, ok := reg.Lookup(id); ok {
				continue
			}
		}
		out = append(out, fmt.Sprintf("- %q is enabled in settings but not installed (run gocode plugin install %s, or remove it from enabledPlugins)", id, id))
	}
	fsys := fsfacade.OSFS{}
	for _, dir := range append(append([]string{}, d.PluginDirs...), d.PluginDirsNoMCP...) {
		if _, err := plugins.LoadPluginDir(fsys, dir, "", plugins.OriginPluginDir); err == nil {
			continue
		} else if len(plugins.DiscoverDir(fsys, dir, "", plugins.OriginPluginDir)) == 0 {
			out = append(out, fmt.Sprintf("- --plugin-dir %s: %s", doctorClean(dir), doctorClean(err.Error())))
		}
	}
	return out
}

// doctorChromeStatus runs the `gocode chrome status` check for the default
// browser and captures its report.
func doctorChromeStatus(env *Env) (string, bool) {
	var out bytes.Buffer
	ci := defaultChromeInstaller(env)
	ci.stdout, ci.stderr = &out, &out
	code := ci.status(parseChromeFlags(nil))
	return out.String(), code == 0
}
