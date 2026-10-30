package settingspolicy

// Restrictive policy caps across the admin tiers.
//
// When more than one admin tier takes part in the composition (managed
// sources merged with managedSourcesBehavior "merge", or the fail-closed
// displaced tier appended under first-wins), a key marked restrictive does
// not simply take the highest tier's value: the most restrictive value any
// tier sets wins, and a permissive value inherited only from a lower tier is
// dropped.

// RestrictivePath is one entry of the restrictive-path registry. Values lists
// the restrictive values, most restrictive first (a single value or an
// ordered list). Normalize, when non-nil, maps the raw
// value before comparison.
type RestrictivePath struct {
	Path      []string
	Values    []any
	Normalize func(v any, present bool) (any, bool)
}

// effortLevels is the effort order, lowest first; the lowest is the most
// restrictive cap.
var effortLevels = []any{"low", "medium", "high", "xhigh", "max"}

// normalizeTruthy maps undefined/false to itself and anything else to true.
func normalizeTruthy(v any, present bool) (any, bool) {
	if !present || v == false {
		return v, present
	}
	return true, true
}

func rp(path []string, values ...any) RestrictivePath {
	return RestrictivePath{Path: path, Values: values}
}

// RestrictivePaths is the restrictive-path registry.
var RestrictivePaths = []RestrictivePath{
	rp([]string{"allowManagedPermissionRulesOnly"}, true),
	rp([]string{"allowManagedHooksOnly"}, true),
	rp([]string{"allowManagedMcpServersOnly"}, true),
	rp([]string{"enforceAvailableModels"}, true),
	rp([]string{"disableAllHooks"}, true),
	rp([]string{"disablegocodeAiConnectors"}, true),
	rp([]string{"disableCommandPluginSources"}, true),
	rp([]string{"disableSideloadFlags"}, true),
	rp([]string{"disableSkillShellExecution"}, true),
	rp([]string{"disableRemoteControl"}, true),
	rp([]string{"disableAgentView"}, true),
	rp([]string{"disableWorkflows"}, true),
	rp([]string{"disableArtifact"}, true),
	rp([]string{"disableBundledSkills"}, true),
	rp([]string{"fastModePerSessionOptIn"}, true),
	rp([]string{"isolatePeerMachines"}, true),
	rp([]string{"strictPluginOnlyCustomization"}, true),
	rp([]string{"disableAutoMode"}, "disable"),
	rp([]string{"disableDeepLinkRegistration"}, "disable"),
	rp([]string{"permissions", "disableBypassPermissionsMode"}, "disable"),
	rp([]string{"permissions", "disableAutoMode"}, "disable"),
	rp([]string{"permissions", "blockReadsOutsideWorkingDirectories"}, true),
	rp([]string{"autoMode", "classifyAllShell"}, true),
	rp([]string{"worktree", "bgIsolation"}, "worktree"),
	rp([]string{"enableArtifact"}, false),
	rp([]string{"enableWorkflows"}, false),
	rp([]string{"syncgocodeAiSkills"}, false),
	rp([]string{"syncgocodeAiPlugins"}, false),
	rp([]string{"useAutoModeDuringPlan"}, false),
	rp([]string{"skipDangerousModePermissionPrompt"}, false),
	rp([]string{"skipAutoPermissionPrompt"}, false),
	rp([]string{"enableAllProjectMcpServers"}, false),
	rp([]string{"channelsEnabled"}, false),
	rp([]string{"skipWebFetchPreflight"}, false),
	rp([]string{"skipWorkflowUsageWarning"}, false),
	rp([]string{"autoUploadSessions"}, false),
	rp([]string{"remoteControlAtStartup"}, false),
	rp([]string{"remoteTools", "allowUnattendedServing"}, false),
	rp([]string{"autoContinueAtUsageLimit"}, false),
	{Path: []string{"disablePluginDirectory"}, Values: []any{true}, Normalize: normalizeTruthy},
	rp([]string{"attribution", "sessionUrl"}, false),
	rp([]string{"crossSessionInbound"}, "refuse", "hold"),
	rp([]string{"remoteControl", "shareHostProfile"}, "off", "basic"),
	rp([]string{"modelProposedGoals"}, "disabled", "alwaysAsk"),
	rp([]string{"maxEffortLevel"}, effortLevels...),
	rp([]string{"feedbackDrafts"}, "off"),
	rp([]string{"availableModelsMatch"}, "exact"),
	rp([]string{"askUserQuestionTimeout"}, "never"),
	rp([]string{"dialogExpiry"}, "never"),
	rp([]string{"sandbox", "enabled"}, true),
	rp([]string{"sandbox", "failIfUnavailable"}, true),
	rp([]string{"sandbox", "autoAllowBashIfSandboxed"}, false),
	rp([]string{"sandbox", "allowUnsandboxedCommands"}, false),
	rp([]string{"sandbox", "enableWeakerNestedSandbox"}, false),
	rp([]string{"sandbox", "enableWeakerNetworkIsolation"}, false),
	rp([]string{"sandbox", "allowAppleEvents"}, false),
	rp([]string{"sandbox", "allowNestedUserNamespaces"}, false),
	rp([]string{"sandbox", "network", "allowManagedDomainsOnly"}, true),
	rp([]string{"sandbox", "network", "strictAllowlist"}, true),
	rp([]string{"sandbox", "network", "allowAllUnixSockets"}, false),
	rp([]string{"sandbox", "network", "allowLocalBinding"}, false),
	rp([]string{"sandbox", "filesystem", "allowManagedReadPathsOnly"}, true),
	rp([]string{"sandbox", "filesystem", "disabled"}, false),
	rp([]string{"sandbox", "credentials", "allowPlaintextInject"}, false),
	rp([]string{"sandbox", "credentials", "sigv4", "streaming"}, "deny"),
	rp([]string{"sandbox", "credentials", "sigv4", "presigned"}, "deny"),
	rp([]string{"sandbox", "credentials", "sigv4", "sigv4a"}, "deny"),
	rp([]string{"isolation", "required"}, true),
	rp([]string{"isolation", "persistHome"}, false),
}

// value reads the entry's (normalized) value from a tier.
func (r RestrictivePath) value(tier map[string]any) (any, bool) {
	v, ok := GetPath(tier, r.Path)
	if r.Normalize != nil {
		return r.Normalize(v, ok)
	}
	return v, ok
}

// indexOf is Array.prototype.indexOf (strict equality on scalars).
func (r RestrictivePath) indexOf(v any, present bool) int {
	if !present {
		return -1
	}
	for i, c := range r.Values {
		if strictEqual(c, v) {
			return i
		}
	}
	return -1
}

// strictEqual is JavaScript === for JSON scalars; objects/arrays never match.
func strictEqual(a, b any) bool {
	switch x := a.(type) {
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	}
	return false
}

// ApplyRestrictiveCaps clamps result to the most restrictive value across the
// ordered tiers (highest priority first; tiers[0] is the top tier):
//
//   - for every registry path, the most restrictive value any tier sets is
//     written into result;
//   - when no tier sets a restrictive value and the top tier does not set the
//     path at all, a value result inherited from a lower tier is removed (a
//     lower tier cannot loosen a restrictive key on its own);
//   - unless strictPluginOnlyCustomization ended up true, the list forms of it
//     are unioned across all tiers.
//
// It is the MergeDeps.ApplyRestrictiveCaps implementation.
func ApplyRestrictiveCaps(result map[string]any, tiers []map[string]any) {
	if len(tiers) == 0 {
		return
	}
	top := tiers[0]
	for _, r := range RestrictivePaths {
		best := -1
		for _, t := range tiers {
			if i := r.indexOf(r.value(t)); i != -1 && (best == -1 || i < best) {
				best = i
			}
		}
		if best != -1 {
			SetPath(result, r.Path, r.Values[best])
			continue
		}
		_, topHas := GetPath(top, r.Path)
		if _, resHas := GetPath(result, r.Path); !topHas && resHas {
			DeletePath(result, r.Path)
		}
	}
	if result["strictPluginOnlyCustomization"] != true {
		var all []any
		for _, t := range tiers {
			if list, ok := t["strictPluginOnlyCustomization"].([]any); ok {
				all = append(all, list...)
			}
		}
		all = uniq(all)
		if len(all) > 0 {
			result["strictPluginOnlyCustomization"] = all
		} else if _, topHas := top["strictPluginOnlyCustomization"]; !topHas {
			delete(result, "strictPluginOnlyCustomization")
		}
	}
}

// MergeLayered deep-merges src into dst with the default baseDefault customizer —
// arrays union (dst then src, deduped; fallbackModel replaced), modelPicker
// replaced, extraKnownMarketplaces/managedMcpServers shallow-merged, other
// objects merged key-wise, scalars from src winning. It is how the documents
// of one managed folder (managed-settings.json then the sorted
// managed-settings.d drop-ins) combine into the file tier.
func MergeLayered(dst, src map[string]any) {
	baseMerge(dst, src, baseDefault)
}

// DeepCloneSettings deep-copies a settings object (nil stays nil).
func DeepCloneSettings(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	return deepClone(s).(map[string]any)
}
