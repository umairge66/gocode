package mods

import (
	"slices"
	"strings"
)

// Policy is what the settings say about hooks modules.
type Policy struct {
	// Off, when non-empty, says why no hooks module loads here.
	Off string
	// ManagedModsOnly loads only the managed plugins' modules (the prepend
	// and append tiers): the built-in security mod's allowManagedModsOnly.
	ManagedModsOnly bool
	// OverrideDenyRules lets a mod's tool.check answer loosen a verdict a
	// deny rule reached (the built-in security mod's
	// allowModsToOverrideDenyRules); off, deny rules hold over every mod.
	OverrideDenyRules bool
	// StrictPluginOnly is strictPluginOnlyCustomization for hooks: the hooks
	// surface is limited to plugins. A hooks module always comes from a
	// plugin, so it still loads; a module from any other source would not.
	StrictPluginOnly bool
}

// offText is the refusal given where modules cannot load.
const offText = "hooks modules are turned off here (disableAllHooks, allowManagedHooksOnly or a policy)"

// secDefaultConfig is the built-in security mod's options path in settings
// .
var secDefaultConfig = []string{"pluginConfigs", "cc-plugin-sec-default@builtin", "options"}

// PolicyFrom reads the policy layer and the merged settings.
//
// Modules load only where hooks run at all and are not limited to managed
// hooks: policy disableAllHooks,
// policy allowManagedHooksOnly, or a disableAllHooks set outside policy
// turns them off, as does safe mode.
func PolicyFrom(policy, merged map[string]any, safeMode bool) Policy {
	var p Policy
	pDisable, _ := policy["disableAllHooks"].(bool)
	pManaged, _ := policy["allowManagedHooksOnly"].(bool)
	mDisable, _ := merged["disableAllHooks"].(bool)
	if safeMode || pDisable || pManaged || (mDisable && !pDisable) {
		p.Off = offText
	}
	opts := lookup(merged, secDefaultConfig)
	if v, ok := opts["allowManagedModsOnly"]; ok && v != nil && v != false {
		p.ManagedModsOnly = true
	}
	if pol := lookup(policy, secDefaultConfig); pol != nil {
		if v, ok := pol["allowManagedModsOnly"]; ok && v != nil && v != false {
			p.ManagedModsOnly = true
		}
	}
	// Deny rules hold unless the organization's own option lifts them.
	if pol := lookup(policy, secDefaultConfig); pol != nil {
		p.OverrideDenyRules, _ = pol["allowModsToOverrideDenyRules"].(bool)
	}
	switch v := policy["strictPluginOnlyCustomization"].(type) {
	case bool:
		p.StrictPluginOnly = v
	case []any:
		p.StrictPluginOnly = slices.Contains(v, any("hooks"))
	}
	return p
}

func lookup(m map[string]any, path []string) map[string]any {
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// TierFor: where a plugin's hooks stand. A plugin the organization
// enables (managed) stands in prepend, or append when the organization's
// appendPlugins names it; with no organization lists, a person's own
// prependPlugins / appendPlugins place a plugin they installed; everything
// else stands in user.
func TierFor(pluginID string, managed bool, policy, user map[string]any) Tier {
	list := func(m map[string]any, key string) []string {
		raw, _ := m[key].([]any)
		var out []string
		for _, x := range raw {
			if s, ok := x.(string); ok {
				out = append(out, strings.ToLower(s))
			}
		}
		return out
	}
	id := strings.ToLower(pluginID)
	_, polPre := policy["prependPlugins"]
	_, polApp := policy["appendPlugins"]
	fromPolicy := polPre || polApp
	src := user
	if fromPolicy {
		src = policy
	}
	in := func(key string) bool {
		if !slices.Contains(list(src, key), id) {
			return false
		}
		if fromPolicy {
			return managed
		}
		return !managed
	}
	switch {
	case in("prependPlugins"):
		return TierPrepend
	case in("appendPlugins"):
		return TierAppend
	case managed:
		return TierPrepend
	}
	return TierUser
}
