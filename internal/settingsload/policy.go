package settingsload

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// The effective policySettings source: the composition of the admin tiers.
//
// This is the single place the policy source is produced; LoadSource /
// LoadAll / EffectiveSources / Store all read it, so every consumer of
// policySettings (permission rules, allowManagedPermissionRulesOnly,
// allowManagedHooksOnly, disableAllHooks, enforceAvailableModels /
// availableModels, the plugin and marketplace policies, MCP policy, sandbox
// policy, ...) sees the same composed object.
//
// The tiers, highest priority first:
//
//  1. server-managed (remote) settings — not supported (hosted service);
//  2. the OS MDM tier — the macOS managed-preferences plist or the HKLM policy
//     key (ComposeOSPolicy);
//  3. the managed-settings.json file tier — the base file deep-merged with the
//     sorted managed-settings.d drop-ins, later documents winning per key and
//     arrays unioned.
//
// The present tiers go through the admin-tier merge
// (settingspolicy.MergeAdminTiers). managedSourcesBehavior, read from the
// highest tier that sets it or holds the slot, picks "first-wins" (the
// default: the top tier is used verbatim) or "merge" (lower tiers deep-merged
// beneath it after secret/non-inheritable keys are stripped, then the
// restrictive caps and the modelOverrides rule apply).
//
// When no admin tier holds the slot, the per-user HKCU policy key is the
// policySettings; a
// user-writable document never outranks or merges with an admin one, and its
// admin-only keys were already stripped (ParseManagedDocument).
//
// Fail-closed behavior:
//   - a document that cannot be read or parsed contributes nothing and
//     surfaces a fatal error; it still counts as deployed, so the user-writable
//     HKCU key stays suppressed;
//   - a tier that holds the slot with no keys (the unreadable WSL opt-in flag)
//     yields an empty policy object, which still shuts HKCU out;
//   - when nothing applies and an admin document failed to load, the policy
//     source reports LoadUnreadable, so key-presence checks answer
//     "unknowable" rather than "absent".
//
// Not supported, by design: the remote / policy-helper / parent-managed /
// host-managed-provider / cowork-cloud branches (hosted
// surfaces), the sign-in org pins, the
// status-only "not applied" notices and all telemetry.

// ComposedPolicy is the composed policySettings plus the composition facts.
type ComposedPolicy struct {
	// Settings is the effective policySettings; nil when no policy applies.
	Settings map[string]any
	// Errors are the diagnostics of every document read (fatal for an
	// unreadable/unparsable admin document, warnings otherwise).
	Errors []settingspolicy.LoadError
	// Warnings are the advisory schema warnings of the documents, each
	// prefixed with the document it came from.
	Warnings []string
	// Tiers are the admin tiers fed to the merge, highest priority first, and
	// TierSources their kinds ("plist", "hklm" or "file").
	Tiers       []map[string]any
	TierSources []string
	// Behavior is the managedSourcesBehavior in effect and Merged whether
	// tiers were actually combined.
	Behavior settingspolicy.ParentSettingsBehavior
	Merged   bool
	// FromHKCU reports that Settings is the user-writable HKCU document.
	FromHKCU bool
	// LoadState folds the admin documents' load states.
	LoadState settingspolicy.LoadState
}

// Result converts the composition to the per-source loader shape.
func (p ComposedPolicy) Result() settingspolicy.LoadResult {
	r := settingspolicy.LoadResult{Errors: p.Errors, Warnings: p.Warnings}
	switch {
	case p.Settings != nil:
		r.Settings = p.Settings
		r.LoadState = settingspolicy.LoadLoaded
	case p.LoadState == settingspolicy.LoadDidNotLoad:
		// An admin document exists but did not load and nothing else applies:
		// the policy is not knowable (fail closed for presence checks).
		r.LoadState = settingspolicy.LoadUnreadable
	default:
		r.Settings = settingspolicy.Settings{}
		r.LoadState = settingspolicy.LoadAbsent
	}
	return r
}

// managedDocState is one composed document or folder.
type managedDocState struct {
	settings         map[string]any
	errors           []settingspolicy.LoadError
	warnings         []string
	hasPolicyContent bool
	loadState        settingspolicy.LoadState
}

// counts reports whether an admin (never user-writable) record holds the
// slot: it has policy content or did not load.
func (d managedDocState) counts() bool {
	return d.hasPolicyContent || d.loadState == settingspolicy.LoadDidNotLoad
}

// ComposePolicy composes the effective policySettings for ctx.
func ComposePolicy(fsys fsfacade.FS, ctx *Context) ComposedPolicy {
	if ctx == nil {
		ctx = &Context{}
	}
	if fsys == nil {
		fsys = fsfacade.OSFS{}
	}
	if ctx.Diskless {
		return ComposedPolicy{LoadState: settingspolicy.LoadAbsent}
	}

	readout := ctx.readOSPolicy()
	osPolicy := ComposeOSPolicy(readout, fsys, ctx)
	mdmLabel := "hklm"
	if len(readout.Plists) > 0 {
		mdmLabel = "plist" // "plist" on macOS, else "hklm"
	}

	// The MDM record for composition: an empty object is null.
	mdm := osPolicy.MDM
	var mdmRaw map[string]any
	if len(mdm.Settings) > 0 {
		mdmRaw = mdm.Settings
	}
	file := loadFileTier(fsys, ctx, readout.IsWSL, osPolicy)

	var out ComposedPolicy
	out.Errors = append(out.Errors, mdm.Errors...)
	out.Errors = append(out.Errors, file.errors...)
	if mdmRaw != nil {
		out.Warnings = append(out.Warnings, labelWarnings(mdmLabel, validateSettings(mdmRaw))...)
	}
	out.Warnings = append(out.Warnings, file.warnings...)
	out.LoadState = combineLoadState(mdm.LoadState, file.loadState)

	// Slot ranking.
	h := policyContentOrNil(mdmRaw)
	y := policyContentOrNil(file.settings)
	mdmApplies := mdmRaw != nil && applicable(mdmRaw)
	fileApplies := file.settings != nil && applicable(file.settings)
	mdmWins := h != nil && slotRank(h, mdmApplies) >= slotRank(y, fileApplies)

	// managedSourcesBehavior comes from the first record (MDM, then file)
	// that sets it or wins its slot.
	behavior := settingspolicy.FirstWins
	for _, rec := range []struct {
		s    map[string]any
		wins bool
	}{{mdmRaw, mdmWins}, {file.settings, y != nil}} {
		if rec.s == nil {
			continue
		}
		v, has := rec.s["managedSourcesBehavior"]
		if !has && !rec.wins {
			continue
		}
		if v == string(settingspolicy.MergeBehavior) {
			behavior = settingspolicy.MergeBehavior
		}
		break
	}

	// An MDM tier outranked by the file tier is displaced to the end of the
	// list, where only its restrictive values still bind.
	var top, displaced map[string]any
	if h != nil && !mdmWins {
		displaced = h
	} else {
		top = h
	}
	addTier := func(t map[string]any, src string) {
		if t != nil {
			out.Tiers = append(out.Tiers, t)
			out.TierSources = append(out.TierSources, src)
		}
	}
	addTier(top, mdmLabel)
	addTier(y, "file")
	takeLast := 0
	if displaced != nil {
		addTier(displaced, mdmLabel)
		takeLast = 1
	}

	res := settingspolicy.MergeAdminTiers(cloneTiers(out.Tiers), behavior, false, takeLast, settingspolicy.DefaultMergeDeps(nil))
	out.Behavior, out.Merged = behavior, res.Merged
	admin := res.Admin
	if admin == nil && (mdm.HasPolicyContent || file.hasPolicyContent) {
		admin = map[string]any{} // a slot held with nothing applicable
	}

	if admin == nil {
		out.Errors = append(out.Errors, osPolicy.HKCUErrors...)
		if len(osPolicy.HKCU) > 0 {
			out.Settings = osPolicy.HKCU
			out.FromHKCU = true
			out.Warnings = append(out.Warnings, labelWarnings("hkcu", validateSettings(osPolicy.HKCU))...)
		}
		return out
	}

	settings := settingspolicy.DeepCloneSettings(admin)
	for _, t := range out.Tiers {
		if t["forceRemoteSettingsRefresh"] == true {
			settings["forceRemoteSettingsRefresh"] = true
			break
		}
	}
	if !envTruthy(ctx.env(disableAdminEnvUnionVar)) {
		envs := make([]map[string]any, len(out.Tiers))
		otel := make([]bool, len(out.Tiers))
		bound := make([]map[string]bool, len(out.Tiers))
		for i, t := range out.Tiers {
			envs[i], _ = t["env"].(map[string]any)
			s, _ := t["otelHeadersHelper"].(string)
			otel[i] = strings.TrimSpace(s) != ""
			bound[i] = helperBoundVars(t)
		}
		if u := unionAdminEnv(envs, otel, bound, 0); len(u) > 0 {
			settings["env"] = u
		} else {
			delete(settings, "env")
		}
	}
	if env, ok := settings["env"].(map[string]any); ok {
		if env = withoutEnvUnionFlag(env); len(env) > 0 {
			settings["env"] = env
		} else {
			delete(settings, "env")
		}
	}
	out.Settings = settings
	return out
}

// loadFileTier picks and loads the managed folder(s) of the file tier
// .
//
//   - not WSL: the platform managed folder;
//   - WSL inheriting the Windows chain: the DrvFs Windows folder when it (or
//     the MDM tier) holds the slot, otherwise /etc/gocode, with the Windows
//     folder's diagnostics kept;
//   - WSL not inheriting while the MDM tier holds the slot: no folder;
//   - otherwise /etc/gocode.
func loadFileTier(fsys fsfacade.FS, ctx *Context, isWSL bool, osPolicy OSPolicy) managedDocState {
	if !isWSL {
		return loadManagedFolder(fsys, ctx.ManagedBaseDir())
	}
	mdmHolds := osPolicy.MDM.counts()
	if osPolicy.WSLInherits {
		win := loadManagedFolder(fsys, ctx.wslWindowsManagedDir())
		if win.counts() || mdmHolds {
			return win
		}
		etc := loadManagedFolder(fsys, ctx.ManagedBaseDir())
		etc.errors = append(append([]settingspolicy.LoadError{}, win.errors...), etc.errors...)
		etc.warnings = append(append([]string{}, win.warnings...), etc.warnings...)
		etc.loadState = combineLoadState(win.loadState, etc.loadState)
		return etc
	}
	if mdmHolds {
		return managedDocState{loadState: settingspolicy.LoadAbsent}
	}
	return loadManagedFolder(fsys, ctx.ManagedBaseDir())
}

// loadManagedFolder reads managed-settings.json then the sorted
// managed-settings.d drop-ins of dir and deep-merges them in that order
// (later documents win per key, arrays union). A document that fails to load
// adds its fatal error and marks the folder didNotLoad; an unreadable drop-in
// directory (anything but ENOENT/ENOTDIR) does the same.
func loadManagedFolder(fsys fsfacade.FS, dir string) managedDocState {
	out := managedDocState{loadState: settingspolicy.LoadAbsent}
	merged := map[string]any{}
	found := false
	add := func(path string) {
		res := LoadFile(fsys, path)
		out.errors = append(out.errors, res.Errors...)
		out.warnings = append(out.warnings, labelWarnings(path, res.Warnings)...)
		switch res.LoadState {
		case settingspolicy.LoadAbsent:
			return
		case settingspolicy.LoadLoaded:
			out.loadState = combineLoadState(out.loadState, settingspolicy.LoadLoaded)
		default:
			out.loadState = settingspolicy.LoadDidNotLoad
			return
		}
		if len(res.Settings) == 0 {
			return
		}
		out.hasPolicyContent = out.hasPolicyContent || hasPolicyContent(res.Settings)
		settingspolicy.MergeLayered(merged, settingspolicy.DeepCloneSettings(res.Settings))
		found = true
	}
	add(filepath.Join(dir, managedSettingsFile))
	dropIn := filepath.Join(dir, managedDropInDir)
	names, err := readDropIns(fsys, dropIn)
	if err != nil {
		out.errors = append(out.errors, settingspolicy.LoadError{
			File: dropIn, Severity: "fatal",
			Message: "Managed settings drop-in directory could not be read: " + err.Error(),
		})
		out.loadState = settingspolicy.LoadDidNotLoad
	}
	for _, name := range names {
		add(filepath.Join(dropIn, name))
	}
	if found && hasPolicyContent(merged) {
		out.settings = merged
	}
	return out
}

// readDropIns lists the drop-in names; a missing directory is not an error.
func readDropIns(fsys fsfacade.FS, dir string) ([]string, error) {
	if _, err := fsys.ReadDir(dir); err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return dropInFiles(fsys, dir), nil
}

// policyContentOrNil returns s when it has a non-control key.
func policyContentOrNil(s map[string]any) map[string]any {
	if s != nil && hasPolicyContent(s) {
		return s
	}
	return nil
}

// applicable reports a non-control key whose value is not an empty object
// tree.
func applicable(s map[string]any) bool {
	for k, v := range s {
		if !containsString(managedControlKeys, k) && !emptyObjectTree(v) {
			return true
		}
	}
	return false
}

// emptyObjectTree reports an object all of whose values are empty object trees.
func emptyObjectTree(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, e := range m {
		if !emptyObjectTree(e) {
			return false
		}
	}
	return true
}

// slotRank ranks a record for the slot: -1 absent, 2 applicable,
// 1 with applicable keys but not as a whole, 0 otherwise.
func slotRank(s map[string]any, applies bool) int {
	switch {
	case s == nil:
		return -1
	case applies:
		return 2
	case applicable(s):
		return 1
	}
	return 0
}

func cloneTiers(tiers []map[string]any) []map[string]any {
	out := make([]map[string]any, len(tiers))
	for i, t := range tiers {
		out[i] = settingspolicy.DeepCloneSettings(t)
	}
	return out
}

func labelWarnings(label string, ws []string) []string {
	if len(ws) == 0 {
		return nil
	}
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = label + ": " + w
	}
	return out
}

// --- admin env union ---

// disableAdminEnvUnionVar opts out of the env union: only the top tier's env
// applies.
const disableAdminEnvUnionVar = "GOCODE_CODE_DISABLE_ADMIN_ENV_UNION"

// otlpExporterPrefixes / otelExactVars are the telemetry-exporter variables a
// single tier owns as a group. No telemetry is sent; these only decide which
// tier's values land in the composed env.
var otlpExporterPrefixes = []string{"OTEL_EXPORTER_OTLP_", "ANT_OTEL_EXPORTER_OTLP_"}

var otelExactVars = []string{
	"OTEL_LOG_RAW_API_BODIES", "OTEL_LOG_USER_PROMPTS", "OTEL_LOG_ASSISTANT_RESPONSES",
	"OTEL_LOG_TOOL_CONTENT", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_MANAGED_SETTINGS",
	"OTEL_LOGS_EXPORTER", "ENABLE_BETA_TRACING_DETAILED", "BETA_TRACING_ENDPOINT",
	"ANT_OTEL_LOGS_EXPORTER",
}

// isTelemetryGroupVar reports a telemetry-exporter group variable.
func isTelemetryGroupVar(upper string) bool {
	for _, p := range otlpExporterPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return containsString(otelExactVars, upper)
}

// credentialHelperVars maps a credential helper to the variables it is bound
// to: a lower tier may not supply them when it configures that helper. The
// product prefix uses the GOCODE_CODE_ spelling (the union compares
// upper-cased names).
var credentialHelperVars = map[string][]string{
	"apiKeyHelper": {"ANTHROPIC_BASE_URL"},
	"awsAuthRefresh": {"GOCODE_CODE_USE_BEDROCK", "GOCODE_CODE_USE_GOCODE_AWS", "GOCODE_CODE_USE_MANTLE",
		"GOCODE_BEDROCK_BASE_URL", "GOCODE_AWS_BASE_URL", "GOCODE_BEDROCK_MANTLE_BASE_URL"},
	"awsCredentialExport": {"GOCODE_CODE_USE_BEDROCK", "GOCODE_CODE_USE_GOCODE_AWS", "GOCODE_CODE_USE_MANTLE",
		"GOCODE_BEDROCK_BASE_URL", "GOCODE_AWS_BASE_URL", "GOCODE_BEDROCK_MANTLE_BASE_URL"},
	"gcpAuthRefresh": {"GOCODE_CODE_USE_VERTEX", "GOCODE_CODE_USE_GOCODE_GOOGLE_CLOUD",
		"GOCODE_VERTEX_BASE_URL", "GOCODE_GOOGLE_CLOUD_BASE_URL"},
}

func helperBoundVars(tier map[string]any) map[string]bool {
	var set map[string]bool
	for helper, vars := range credentialHelperVars {
		if s, ok := tier[helper].(string); ok && strings.TrimSpace(s) != "" {
			if set == nil {
				set = map[string]bool{}
			}
			for _, v := range vars {
				set[v] = true
			}
		}
	}
	return set
}

// unionAdminEnv unions the env blocks of the admin tiers (highest first): a
// variable takes the value of the highest tier that sets it (compared
// case-insensitively), the telemetry-exporter group comes whole from one
// tier, and a lower tier cannot supply variables bound to a credential helper
// it configures. topCount is the number of leading tiers exempt from the
// lower-tier rules.
func unionAdminEnv(envs []map[string]any, otelHelper []bool, bound []map[string]bool, topCount int) map[string]any {
	byUpper := map[string]any{}
	out := map[string]any{}
	telemetryTaken := false
	for g, env := range envs {
		seen := map[string]bool{}
		owns := otelHelper[g]
		for _, key := range sortedKeys(env) {
			val := env[key]
			upper := strings.ToUpper(key)
			tel := isTelemetryGroupVar(upper)
			if s, isStr := val.(string); tel && isStr && strings.TrimSpace(s) == "" {
				continue
			}
			if tel && telemetryTaken {
				continue
			}
			if tel && g > topCount && otelHelper[g] {
				continue
			}
			if prev, ok := byUpper[upper]; ok {
				if seen[upper] {
					out[key] = val
				} else if _, has := out[key]; !has {
					out[key] = prev
				}
				continue
			}
			if g > topCount && bound[g][upper] {
				continue
			}
			out[key] = val
			seen[upper] = true
			byUpper[upper] = val
			if tel {
				owns = true
			}
		}
		if owns {
			telemetryTaken = true
		}
	}
	return out
}

// withoutEnvUnionFlag drops the opt-out flag itself from the env.
func withoutEnvUnionFlag(env map[string]any) map[string]any {
	out := make(map[string]any, len(env))
	for k, v := range env {
		if strings.ToUpper(k) != disableAdminEnvUnionVar {
			out[k] = v
		}
	}
	return out
}

// envTruthy is the boolean env reading ("1", "true", "yes", "on").
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// --- OS readout cache ---

var (
	platformReadoutOnce sync.Once
	platformReadout     OSPolicyReadout
)

// cachedPlatformOSPolicy reads the OS policy once per process (the read can
// spawn reg.exe on WSL).
func cachedPlatformOSPolicy() OSPolicyReadout {
	platformReadoutOnce.Do(func() { platformReadout = readPlatformOSPolicy() })
	return platformReadout
}
