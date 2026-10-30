package settingsload

import (
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// SkillShellExecutionKey is the settings key that turns off the `!`-blocks of
// skills and custom slash commands (they are replaced by a placeholder instead
// of being run).
const SkillShellExecutionKey = "disableSkillShellExecution"

// SkillShellExecutionDisabled reports whether disableSkillShellExecution is in
// effect for this session. The setting can only restrict, so any settings
// source may turn it on:
//
//  1. GOCODE_CODE_IS_COWORK set (cowork sessions never run blocks);
//  2. the composed policySettings (ComposePolicy) sets it to true. The key is
//     a restrictive path, so when admin tiers are merged a true in any of
//     them is kept; under first-wins only the top tier counts;
//  3. otherwise the merged effective value (last source that sets the key,
//     in canonical precedence order) is true.
//
// The result is read from disk on every call (re-reading the settings
// cache), so an edit applies to the next expansion.
func SkillShellExecutionDisabled(fsys fsfacade.FS, ctx *Context) bool {
	if ctx == nil {
		ctx = &Context{}
	}
	if fsys == nil {
		fsys = fsfacade.OSFS{}
	}
	if ctx.env("GOCODE_CODE_IS_COWORK") != "" {
		return true
	}
	return mergedSkillShellDisabled(EffectiveSources(fsys, ctx, CanonicalSourceOrder))
}

// mergedSkillShellDisabled is the policy check plus the merged-value check
// over already-loaded sources: a policySettings true wins outright, else the
// last loaded source that sets the key decides.
//
// Hardening: a true from a trusted source (user, --settings) is sticky
// against a later repo-controlled source (project/local settings, which a
// cloned repository can ship): a repository cannot re-enable `!`-blocks the
// user turned off (a plain merged read would let the project's false win).
func mergedSkillShellDisabled(ordered []OrderedResult) bool {
	var merged any
	trustedOn := false
	for _, or := range ordered {
		if or.Result.LoadState != settingspolicy.LoadLoaded || or.Result.Settings == nil {
			continue
		}
		v, ok := or.Result.Settings[SkillShellExecutionKey]
		if !ok {
			continue
		}
		if or.Source == settingspolicy.SourcePolicy && v == true {
			return true
		}
		repo := or.Source == settingspolicy.SourceProject || or.Source == settingspolicy.SourceLocal
		if v == true && !repo {
			trustedOn = true
		}
		merged = v
	}
	return merged == true || trustedOn
}
