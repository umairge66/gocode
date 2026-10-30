package settingspolicy

import (
	"github.com/umairge66/gocode/internal/platform"
	"github.com/umairge66/gocode/internal/secretpaths"
)

// Admin-tier merge engine.
//
// Implemented in this package:
//
//   - DeepMerge: the recursive object merge (mergeWith-style with the
//     replaceCustomizer/appendCustomizer customizers, specialCombine, and alwaysSpecialKeys/replaceOnlySpecialKeys key lists).
//     See deepmerge.go; the exported DeepMerge function is the
//     MergeDeps.DeepMerge implementation.
//   - ApplyRestrictiveCaps: clamps a merged result to the most restrictive
//     value across tiers for the restrictive-path registry. See
//     restrictive.go.
//
// The admin tiers MergeAdminTiers combines are assembled by
// internal/settingsload (ComposePolicy): the OS MDM tier (macOS managed
// preferences plist or the HKLM policy key), then the managed-settings.json
// file tier (the base file merged with its managed-settings.d drop-ins). The
// HKCU policy key is not an admin tier: it is the fallback used only when no
// admin tier holds the slot. Server-managed (remote) settings are not supported.
//
// Wired from other packages (see DefaultMergeDeps):
//
//   - StripSecrets: removes credential/non-inheritable *paths* from a lower
//     tier. This is path-registry removal, not the free-text pattern
//     redaction internal/redact provides (redact.RedactValue). The registry
//     lives in internal/secretpaths and deletes the listed top-level keys
//     (no replacement value). Wired to secretpaths.StripSecrets.
//   - CurrentPlatform: the running platform id.
//     Wired to platform.CurrentPlatform.
//
// The orchestration itself, the non-inheritable key stripping, the
// sandbox/enabledPlatforms rule, and model-override resolution are
// implemented below.

// nonInheritablePaths are settings paths that must NOT be inherited from a
// lower admin tier when merging (they are stripped from every tier except,
// under "merge", the preserved top one).
var nonInheritablePaths = [][]string{
	{"permissions", "defaultMode"},
	{"modelPicker", "replaceBuiltInOptions"},
}

// MergeDeps supplies the primitives the merge engine needs, injectable so
// tests can observe the bare merge; DefaultMergeDeps wires the real ones.
type MergeDeps struct {
	// DeepMerge merges src into dst. When appendArrays is true (the
	// appendCustomizer mode, used for the preserved top tier in gateway
	// sessions) array values are concatenated rather than replaced.
	DeepMerge func(dst, src map[string]any, appendArrays bool)
	// StripSecrets returns a deep clone of s with credential values masked.
	StripSecrets func(s map[string]any) map[string]any
	// ApplyRestrictiveCaps clamps result to the most restrictive value across
	// the ordered tiers (highest-priority first).
	ApplyRestrictiveCaps func(result map[string]any, tiers []map[string]any)
	// CurrentPlatform returns the running platform identifier, compared
	// against sandbox.enabledPlatforms.
	CurrentPlatform func() string
}

// DefaultMergeDeps returns MergeDeps with the primitives wired to their
// implementations: DeepMerge (this package), StripSecrets
// (internal/secretpaths), CurrentPlatform (internal/platform) and
// ApplyRestrictiveCaps (this package). A nil applyRestrictiveCaps selects
// ApplyRestrictiveCaps;
// a non-nil one replaces it (tests pass a no-op to observe the bare merge).
func DefaultMergeDeps(applyRestrictiveCaps func(result map[string]any, tiers []map[string]any)) MergeDeps {
	deps := MergeDeps{
		DeepMerge:            DeepMerge,
		StripSecrets:         secretpaths.StripSecrets,
		ApplyRestrictiveCaps: applyRestrictiveCaps,
		CurrentPlatform:      platform.CurrentPlatform,
	}
	if applyRestrictiveCaps == nil {
		deps.ApplyRestrictiveCaps = ApplyRestrictiveCaps
	}
	return deps
}

// MergeResult is the outcome of MergeAdminTiers.
type MergeResult struct {
	// Admin is the resolved admin-tier settings object (nil when there were no
	// tiers).
	Admin map[string]any
	// Merged reports whether multiple tiers were actually combined.
	Merged bool
}

// MergeAdminTiers layers a priority-ordered list of admin-tier settings
// objects (highest priority first) according to parentSettingsBehavior.
//
// Under "first-wins" (and whenever fewer than two tiers are present) the top
// tier is used verbatim. Under "merge" the lower tiers are sanitized (secrets
// masked, non-inheritable keys dropped, sandbox platform scoping applied) and
// deep-merged beneath the top tier, then restrictive policy caps and
// model-override resolution are applied.
//
// preserveTop keeps the
// top tier un-sanitized and switches its merge to array-append mode.
// takeLast bounds how many lower tiers participate when not
// merging.
func MergeAdminTiers(
	tiers []map[string]any,
	behavior ParentSettingsBehavior,
	preserveTop bool,
	takeLast int,
	deps MergeDeps,
) MergeResult {
	if len(tiers) == 0 {
		return MergeResult{Admin: nil, Merged: false}
	}
	top := tiers[0]

	// Split managedSourcesBehavior off the top tier; `withoutBehavior` is the
	// top tier minus that control key.
	withoutBehavior := cloneShallow(top)
	_, hadBehavior := withoutBehavior["managedSourcesBehavior"]
	delete(withoutBehavior, "managedSourcesBehavior")

	doMerge := behavior == MergeBehavior && len(tiers) >= 2

	// Not merging and no lower tiers requested: return the top tier as-is,
	// keeping managedSourcesBehavior only if it was not present to strip.
	if !doMerge && takeLast == 0 {
		if !hadBehavior {
			return MergeResult{Admin: top, Merged: false}
		}
		return MergeResult{Admin: withoutBehavior, Merged: false}
	}

	// Select and sanitize the lower tiers.
	var lower []map[string]any
	if doMerge {
		lower = tiers[1:]
	} else {
		lower = tiers[len(tiers)-takeLast:]
	}

	sanitized := make([]map[string]any, 0, len(lower))
	for i, tier := range lower {
		keepVerbatim := doMerge && preserveTop && i == 0
		var w map[string]any
		if keepVerbatim {
			w = cloneShallow(tier)
		} else {
			w = deps.StripSecrets(tier)
			for _, p := range nonInheritablePaths {
				if _, ok := GetPath(w, p); ok {
					DeletePath(w, p)
				}
			}
		}
		applySandboxPlatformScope(w, deps.CurrentPlatform)
		sanitized = append(sanitized, w)
	}

	// First-wins (or bounded take): no deep merge, just cap the top tier.
	if !doMerge {
		deps.ApplyRestrictiveCaps(withoutBehavior, prepend(top, sanitized))
		return MergeResult{Admin: withoutBehavior, Merged: false}
	}

	// Merge: lower tiers (lowest priority applied first) then the top tier.
	result := map[string]any{}
	for i := len(sanitized) - 1; i >= 0; i-- {
		deps.DeepMerge(result, sanitized[i], false)
	}
	deps.DeepMerge(result, withoutBehavior, preserveTop)

	ordered := prepend(top, sanitized)
	deps.ApplyRestrictiveCaps(result, ordered)
	ResolveModelOverrides(result, ordered, preserveTop)

	return MergeResult{Admin: result, Merged: true}
}

// applySandboxPlatformScope implements the sandbox.enabledPlatforms rule: if
// the list names the current platform, the list is cleared (the sandbox
// applies here, unconditionally); if it names other platforms only, the whole
// sandbox block is dropped (it does not apply here).
func applySandboxPlatformScope(w map[string]any, currentPlatform func() string) {
	v, ok := GetPath(w, []string{"sandbox", "enabledPlatforms"})
	if !ok {
		return
	}
	if platforms, isList := v.([]any); isList {
		cur := currentPlatform()
		for _, p := range platforms {
			if s, _ := p.(string); s == cur {
				DeletePath(w, []string{"sandbox", "enabledPlatforms"})
				return
			}
		}
	}
	// Present but not applicable to this platform: drop the whole sandbox.
	delete(w, "sandbox")
}

// ResolveModelOverrides decides whether the merged result keeps a
// modelOverrides block. The override survives only when it comes from a tier
// at least as high-priority as the first availableModels tier (or there is no
// availableModels tier, or we are preserving the top tier and it is first);
// otherwise it is dropped.
func ResolveModelOverrides(result map[string]any, tiers []map[string]any, preserveTop bool) {
	availIdx := indexWithKey(tiers, "availableModels")
	overrideIdx := indexWithKey(tiers, "modelOverrides")

	keep := overrideIdx != -1 &&
		(availIdx == -1 || overrideIdx <= availIdx || (preserveTop && availIdx == 0))
	if keep {
		result["modelOverrides"] = cloneShallow(asMap(tiers[overrideIdx]["modelOverrides"]))
		return
	}
	delete(result, "modelOverrides")
}

func indexWithKey(tiers []map[string]any, key string) int {
	for i, t := range tiers {
		if _, ok := t[key]; ok {
			return i
		}
	}
	return -1
}

func prepend(head map[string]any, tail []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tail)+1)
	out = append(out, head)
	out = append(out, tail...)
	return out
}
