package sandbox

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Layer is one loaded settings source handed to ParseSettings, in precedence
// order (lowest first: user, project, local, flag, policy).
type Layer struct {
	// Source is the settings-source name ("userSettings", "projectSettings",
	// "localSettings", "flagSettings", "policySettings").
	Source string
	// Settings is the source's parsed settings object.
	Settings map[string]any
	// BaseDir is the directory a relative sandbox path in this source resolves
	// against (the project root for project/local settings, the config home for
	// user settings). "" uses the session cwd.
	BaseDir string
}

// Source names (settingspolicy.Source values).
const (
	srcUser    = string(settingspolicy.SourceUser)
	srcProject = string(settingspolicy.SourceProject)
	srcLocal   = string(settingspolicy.SourceLocal)
	srcFlag    = string(settingspolicy.SourceFlag)
	srcPolicy  = string(settingspolicy.SourcePolicy)
)

// repoSource reports the settings a repository can commit (project/local),
// which may never widen what the user or the admin decided.
func repoSource(s string) bool { return s == srcProject || s == srcLocal }

// trustedSource reports the tiers whose sandbox posture holds over the
// repository's (managed policy, --settings, user settings).
func trustedSource(s string) bool { return s == srcUser || s == srcFlag || s == srcPolicy }

// NetworkSettings is sandbox.network.
type NetworkSettings struct {
	AllowedDomains      []string
	DeniedDomains       []string
	StrictAllowlist     bool
	AllowManagedOnly    bool
	AllowUnixSockets    []string
	AllowAllUnixSockets bool
	AllowLocalBinding   bool
	AllowMachLookup     []string
	// HTTPProxyPort / SOCKSProxyPort name the user's own local proxy, used
	// instead of the one gocode runs. 0 = gocode's proxy.
	HTTPProxyPort  int
	SOCKSProxyPort int
}

// FilesystemSettings is sandbox.filesystem, with the permission-rule paths
// folded in (Edit(...) allow → AllowWrite, Edit(...) deny → DenyWrite,
// Read(...) deny → DenyRead). Paths are absolute.
type FilesystemSettings struct {
	AllowWrite []string
	DenyWrite  []string
	DenyRead   []string
	AllowRead  []string
	// Disabled skips filesystem isolation (network isolation stays).
	Disabled bool
	// GlobsIgnored are rule/settings entries with glob characters the Linux
	// backend leaves out (reported by /sandbox and `gocode sandbox status`).
	GlobsIgnored []string
}

// Settings is the resolved `sandbox` configuration of a session.
type Settings struct {
	Enabled                  bool
	FailIfUnavailable        bool
	AutoAllowBashIfSandboxed bool
	// AutoAllowSet reports that some settings file set autoAllowBashIfSandboxed.
	AutoAllowSet bool
	// UnsandboxedForbidden is the strict mode: allowUnsandboxedCommands is false
	// (in the merged settings, or in a trusted tier).
	UnsandboxedForbidden bool
	ExcludedCommands     []string
	// EnabledPlatforms is the managed sandbox.enabledPlatforms list; nil = all.
	EnabledPlatforms             []string
	IgnoreViolations             map[string][]string
	EnableWeakerNestedSandbox    bool
	EnableWeakerNetworkIsolation bool
	AllowAppleEvents             bool
	AllowNestedUserNamespaces    bool
	// BwrapPath / SocatPath are the managed-only absolute overrides.
	BwrapPath string
	SocatPath string

	Network    NetworkSettings
	Filesystem FilesystemSettings
	// CredentialEnvDeny are sandbox.credentials.envVars names unset for
	// sandboxed commands; CredentialFileDeny are sandbox.credentials.files
	// paths made unreadable. A "mask" entry degrades to "deny" (there is no
	// injecting proxy).
	CredentialEnvDeny  []string
	CredentialFileDeny []string
	// CredentialEnvAllow are sandbox.credentials.passThroughEnvVars (trusted
	// tiers only): provider keys and other secret-looking
	// variables that the default secret-env scrub (SecretEnvNames) would
	// otherwise strip, but which the user has explicitly opted to pass into
	// sandboxed commands. An explicit envVars deny still wins.
	CredentialEnvAllow []string
	// AdditionalDirectories are permissions.additionalDirectories (absolute).
	AdditionalDirectories []string

	// LockedKeys records which of enabled / autoAllowBashIfSandboxed /
	// allowUnsandboxedCommands are pinned by the policy or --settings tier.
	LockedKeys map[string]bool
	// PolicyKeys records the same keys set by the managed policy alone.
	PolicyKeys map[string]bool
}

// LockedByPolicy reports that the policy (or --settings) pins the sandbox
// mode keys, so /sandbox may not change them.
func (s Settings) LockedByPolicy() bool {
	return s.LockedKeys["enabled"] || s.LockedKeys["autoAllowBashIfSandboxed"] || s.LockedKeys["allowUnsandboxedCommands"]
}

// PathContext resolves the relative paths settings entries use.
type PathContext struct {
	Cwd       string
	Home      string
	ConfigDir string
}

// ParseSettings resolves the session's sandbox settings from its loaded
// sources (lowest precedence first).
func ParseSettings(layers []Layer, pc PathContext) Settings {
	merged := map[string]any{}
	for _, l := range layers {
		if l.Settings != nil {
			settingspolicy.MergeLayered(merged, settingspolicy.DeepCloneSettings(l.Settings))
		}
	}
	sb := asMap(merged["sandbox"])
	var s Settings

	s.Enabled, _ = asBool(sb["enabled"])
	s.FailIfUnavailable, _ = asBool(sb["failIfUnavailable"])
	if v, ok := asBool(sb["autoAllowBashIfSandboxed"]); ok {
		s.AutoAllowBashIfSandboxed, s.AutoAllowSet = v, true
	} else {
		s.AutoAllowBashIfSandboxed = true
	}
	if v, ok := asBool(sb["allowUnsandboxedCommands"]); ok && !v {
		s.UnsandboxedForbidden = true
	}
	s.LockedKeys, s.PolicyKeys = map[string]bool{}, map[string]bool{}
	for _, l := range layers {
		lsb := asMap(l.Settings["sandbox"])
		if trustedSource(l.Source) {
			if v, ok := asBool(lsb["allowUnsandboxedCommands"]); ok && !v {
				s.UnsandboxedForbidden = true
			}
		}
		if l.Source == srcPolicy || l.Source == srcFlag {
			for _, k := range []string{"enabled", "autoAllowBashIfSandboxed", "allowUnsandboxedCommands"} {
				if _, ok := lsb[k]; ok {
					s.LockedKeys[k] = true
					if l.Source == srcPolicy {
						s.PolicyKeys[k] = true
					}
				}
			}
		}
		if l.Source == srcPolicy {
			if list, ok := lsb["enabledPlatforms"].([]any); ok {
				s.EnabledPlatforms = append([]string{}, stringList(list)...)
				if s.EnabledPlatforms == nil {
					s.EnabledPlatforms = []string{}
				}
			}
			if p, ok := lsb["bwrapPath"].(string); ok && filepath.IsAbs(p) {
				s.BwrapPath = p
			}
			if p, ok := lsb["socatPath"].(string); ok && filepath.IsAbs(p) {
				s.SocatPath = p
			}
		}
	}
	pinUserSandboxKeys(&s, layers)
	s.ExcludedCommands = stringList(sb["excludedCommands"])
	s.IgnoreViolations = map[string][]string{}
	for k, v := range asMap(sb["ignoreViolations"]) {
		s.IgnoreViolations[k] = stringList(v)
	}
	if len(s.IgnoreViolations) == 0 {
		s.IgnoreViolations = nil
	}
	s.EnableWeakerNestedSandbox, _ = asBool(sb["enableWeakerNestedSandbox"])
	s.EnableWeakerNetworkIsolation, _ = asBool(sb["enableWeakerNetworkIsolation"])
	// allowAppleEvents / allowNestedUserNamespaces are honored only from the
	// trusted tiers (true from a repository is ignored).
	s.AllowAppleEvents = trustedBool(layers, "allowAppleEvents")
	s.AllowNestedUserNamespaces = trustedBool(layers, "allowNestedUserNamespaces")

	s.CredentialEnvDeny, s.CredentialEnvAllow, s.CredentialFileDeny = parseCredentials(layers, pc)
	s.Network = parseNetwork(layers, merged)
	s.Filesystem = parseFilesystem(layers, pc)
	s.AdditionalDirectories = additionalDirectories(layers, pc)
	return s
}

// trustedBool is a sandbox boolean only the trusted tiers may switch on, while
// a false anywhere still applies.
func trustedBool(layers []Layer, key string) bool {
	on := false
	for _, l := range layers {
		v, ok := asBool(asMap(l.Settings["sandbox"])[key])
		if !ok {
			continue
		}
		if !v {
			return false
		}
		if trustedSource(l.Source) {
			on = true
		}
	}
	return on
}

// webFetchDomainRules returns the domain:<host> contents of WebFetch rules in
// a permissions list (WebFetch rules whose content starts with "domain:").
func webFetchDomainRules(perms map[string]any, list string) []string {
	var out []string
	for _, r := range stringList(perms[list]) {
		tool, content := splitRule(r)
		if tool == "WebFetch" && strings.HasPrefix(content, "domain:") {
			out = append(out, strings.TrimPrefix(content, "domain:"))
		}
	}
	return out
}

// toolRuleContents returns the rule contents of tool's rules in a permissions
// list ("Edit(src/**)" → "src/**").
func toolRuleContents(perms map[string]any, list, tool string) []string {
	var out []string
	for _, r := range stringList(perms[list]) {
		t, content := splitRule(r)
		if t == tool && content != "" {
			out = append(out, content)
		}
	}
	return out
}

// splitRule splits "Tool(content)" into its parts.
func splitRule(r string) (tool, content string) {
	r = strings.TrimSpace(r)
	open := strings.Index(r, "(")
	if open < 0 || !strings.HasSuffix(r, ")") {
		return r, ""
	}
	return strings.TrimSpace(r[:open]), r[open+1 : len(r)-1]
}

func parseNetwork(layers []Layer, merged map[string]any) NetworkSettings {
	var n NetworkSettings
	mnet := asMap(asMap(merged["sandbox"])["network"])
	managedOnly := false
	strict := false
	trustedDenies := false
	for _, l := range layers {
		lnet := asMap(asMap(l.Settings["sandbox"])["network"])
		if l.Source == srcPolicy {
			if v, _ := asBool(lnet["allowManagedDomainsOnly"]); v {
				managedOnly = true
			}
		}
		if trustedSource(l.Source) {
			if v, _ := asBool(lnet["strictAllowlist"]); v {
				strict = true
			}
			if len(stringList(lnet["deniedDomains"])) > 0 {
				trustedDenies = true
			}
		}
	}
	n.AllowManagedOnly = managedOnly
	n.StrictAllowlist = strict
	for _, l := range layers {
		lnet := asMap(asMap(l.Settings["sandbox"])["network"])
		perms := asMap(l.Settings["permissions"])
		switch {
		case managedOnly && l.Source != srcPolicy:
		case strict && repoSource(l.Source):
			// network.strictAllowlist: a repository's allowedDomains and
			// WebFetch(domain:…) allow rules are left out.
		default:
			n.AllowedDomains = append(n.AllowedDomains, stringList(lnet["allowedDomains"])...)
			n.AllowedDomains = append(n.AllowedDomains, webFetchDomainRules(perms, "allow")...)
		}
		// Denied domains count from every source.
		n.DeniedDomains = append(n.DeniedDomains, stringList(lnet["deniedDomains"])...)
		n.DeniedDomains = append(n.DeniedDomains, webFetchDomainRules(perms, "deny")...)
	}
	n.AllowedDomains = dedupe(n.AllowedDomains)
	n.DeniedDomains = dedupe(n.DeniedDomains)
	n.AllowUnixSockets = stringList(mnet["allowUnixSockets"])
	n.AllowAllUnixSockets, _ = asBool(mnet["allowAllUnixSockets"])
	n.AllowLocalBinding, _ = asBool(mnet["allowLocalBinding"])
	n.AllowMachLookup = stringList(mnet["allowMachLookup"])
	// A project may not replace the filtering proxy once a trusted tier denies
	// hosts or asks for a strict allowlist.
	if strict || trustedDenies {
		for _, l := range layers {
			if !trustedSource(l.Source) {
				continue
			}
			lnet := asMap(asMap(l.Settings["sandbox"])["network"])
			if v, ok := asInt(lnet["httpProxyPort"]); ok {
				n.HTTPProxyPort = v
			}
			if v, ok := asInt(lnet["socksProxyPort"]); ok {
				n.SOCKSProxyPort = v
			}
		}
	} else {
		n.HTTPProxyPort, _ = asInt(mnet["httpProxyPort"])
		n.SOCKSProxyPort, _ = asInt(mnet["socksProxyPort"])
	}
	return n
}

// parseCredentials reads sandbox.credentials: envVars and files entries in
// "deny" mode, and "mask" entries degraded to "deny"; plus the
// passThroughEnvVars opt-in (trusted tiers only).
func parseCredentials(layers []Layer, pc PathContext) (envDeny, envAllow, files []string) {
	for _, l := range layers {
		creds := asMap(asMap(l.Settings["sandbox"])["credentials"])
		base := l.BaseDir
		if base == "" {
			base = pc.Cwd
		}
		for _, e := range asSlice(creds["envVars"]) {
			m := asMap(e)
			if name, _ := m["name"].(string); name != "" {
				envDeny = append(envDeny, name)
			}
		}
		// Setting: sandbox.credentials.passThroughEnvVars names
		// variables the default secret scrub (SecretEnvNames) would strip but
		// the user wants inside sandboxed commands. Honored only from the
		// trusted tiers: a repository's settings must not be able to hand the
		// user's keys to the commands it gets run.
		if trustedSource(l.Source) {
			envAllow = append(envAllow, stringList(creds["passThroughEnvVars"])...)
		}
		for _, f := range asSlice(creds["files"]) {
			m := asMap(f)
			if p, _ := m["path"].(string); p != "" {
				files = append(files, resolveSandboxPath(p, base, pc.Home))
			}
		}
	}
	return dedupe(envDeny), dedupe(envAllow), dedupe(files)
}

func asSlice(v any) []any {
	l, _ := v.([]any)
	return l
}

func parseFilesystem(layers []Layer, pc PathContext) FilesystemSettings {
	var f FilesystemSettings
	disabledSet := false
	for _, l := range layers {
		fs := asMap(asMap(l.Settings["sandbox"])["filesystem"])
		perms := asMap(l.Settings["permissions"])
		base := l.BaseDir
		if base == "" {
			base = pc.Cwd
		}
		sp := func(list []string) []string {
			return resolveAll(list, func(p string) string { return resolveSandboxPath(p, base, pc.Home) })
		}
		rp := func(list []string) []string {
			return resolveAll(list, func(p string) string { return resolveRulePath(p, base, pc.Cwd, pc.Home) })
		}
		f.AllowWrite = append(f.AllowWrite, sp(stringList(fs["allowWrite"]))...)
		f.AllowWrite = append(f.AllowWrite, rp(toolRuleContents(perms, "allow", "Edit"))...)
		f.DenyWrite = append(f.DenyWrite, sp(stringList(fs["denyWrite"]))...)
		f.DenyWrite = append(f.DenyWrite, rp(toolRuleContents(perms, "deny", "Edit"))...)
		f.DenyRead = append(f.DenyRead, sp(stringList(fs["denyRead"]))...)
		f.DenyRead = append(f.DenyRead, rp(toolRuleContents(perms, "deny", "Read"))...)
		f.AllowRead = append(f.AllowRead, sp(stringList(fs["allowRead"]))...)
		// filesystem.disabled is honored from the trusted tiers only.
		if v, ok := asBool(fs["disabled"]); ok && trustedSource(l.Source) {
			f.Disabled, disabledSet = v, true
		}
	}
	_ = disabledSet
	f.AllowWrite, f.GlobsIgnored = splitGlobs(dedupe(f.AllowWrite), f.GlobsIgnored)
	f.DenyWrite, f.GlobsIgnored = splitGlobs(dedupe(f.DenyWrite), f.GlobsIgnored)
	f.AllowRead = dedupe(f.AllowRead)
	f.DenyRead = dedupe(f.DenyRead)
	return f
}

// splitGlobs moves glob-spelled write entries aside: the backends confine
// writes to literal directories.
func splitGlobs(in, ignored []string) ([]string, []string) {
	var out []string
	for _, p := range in {
		if isGlob(p) {
			ignored = append(ignored, p)
			continue
		}
		out = append(out, p)
	}
	return out, ignored
}

func additionalDirectories(layers []Layer, pc PathContext) []string {
	var out []string
	for _, l := range layers {
		base := l.BaseDir
		if base == "" {
			base = pc.Cwd
		}
		for _, d := range stringList(asMap(l.Settings["permissions"])["additionalDirectories"]) {
			out = append(out, trimGlobTail(resolveSandboxPath(d, base, pc.Home)))
		}
	}
	return dedupe(out)
}

// resolveSandboxPath resolves a sandbox.filesystem entry: "//x" is the
// absolute "/x", "~" is the home directory, an absolute path stays, anything
// else is relative to the source's base directory. A trailing "/" is dropped.
func resolveSandboxPath(p, base, home string) string {
	switch {
	case strings.HasPrefix(p, "//"):
		p = p[1:]
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case filepath.IsAbs(p):
	default:
		p = filepath.Join(base, p)
	}
	return trimTrailingSlash(p)
}

// resolveRulePath resolves an Edit/Read permission-rule path: "//x" is
// absolute, "~/x" is under home, "/x" is relative to the settings source's
// root, and a bare path is relative to the cwd. A trailing "/**" (the whole
// subtree) collapses to the directory.
func resolveRulePath(p, base, cwd, home string) string {
	switch {
	case strings.HasPrefix(p, "//"):
		p = p[1:]
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "/"):
		p = filepath.Join(base, p[1:])
	default:
		p = filepath.Join(cwd, p)
	}
	return trimTrailingSlash(trimGlobTail(p))
}

func trimGlobTail(p string) string {
	for strings.HasSuffix(p, "/**") {
		p = strings.TrimSuffix(p, "/**")
	}
	if p == "" {
		return "/"
	}
	return p
}

func trimTrailingSlash(p string) string {
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			return "/"
		}
	}
	return p
}

func resolveAll(in []string, f func(string) string) []string {
	var out []string
	for _, p := range in {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, f(p))
	}
	return out
}

// --- small JSON helpers -----------------------------------------------------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 && n <= 65535 && n == float64(int(n)) {
			return int(n), true
		}
	case int:
		if n > 0 && n <= 65535 {
			return n, true
		}
	}
	return 0, false
}

func stringList(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range l {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// sortedDedupe returns a sorted, de-duplicated copy.
func sortedDedupe(in []string) []string {
	out := dedupe(in)
	sort.Strings(out)
	return out
}

// pinUserSandboxKeys keeps the user's own sandbox.enabled / failIfUnavailable
// = true in force when the admin policy or --settings makes unsandboxed
// commands forbidden (strict mode) and does not itself set the key: a
// repository's .gocode/settings.json or settings.local.json cannot then turn
// the user's sandbox off.
func pinUserSandboxKeys(s *Settings, layers []Layer) {
	strict := false
	set := map[string]bool{}
	var user map[string]any
	for _, l := range layers {
		lsb := asMap(l.Settings["sandbox"])
		switch l.Source {
		case srcPolicy, srcFlag:
			if v, ok := asBool(lsb["allowUnsandboxedCommands"]); ok && !v {
				strict = true
			}
			if l.Source == srcPolicy {
				if v, _ := asBool(asMap(lsb["network"])["allowManagedDomainsOnly"]); v {
					strict = true
				}
			}
			for k := range lsb {
				set[k] = true
			}
		case srcUser:
			user = lsb
		}
	}
	if !strict || user == nil {
		return
	}
	if v, _ := asBool(user["enabled"]); v && !set["enabled"] {
		s.Enabled = true
	}
	if v, _ := asBool(user["failIfUnavailable"]); v && !set["failIfUnavailable"] {
		s.FailIfUnavailable = true
	}
}
