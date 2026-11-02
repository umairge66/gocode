package settingsload

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Settings schema / known-key validation.
//
//	unknown key : `Unrecognized field: <k>`  / `Unrecognized fields: <k>, <k>`
//	wrong type  : `Expected <t>, but received <t>`
//
// Schema issues are deliberately DOWNGRADED to advisory warnings rather than
// dropping the file's settings (the standing directive at load.go): the valid
// keys still apply, nothing is rejected or dropped, and precedence is
// unchanged. Only unknown top-level keys and basic value-type mismatches are
// checked here.
//
// COVERAGE / APPROXIMATION:
//   - The known-key set below is the full top-level settings field set (185
//     keys incl. "$schema"), plus
//     gocode's own "search" object (Grep/Glob engine) and "ui" object (the
//     full-screen look); their sub-keys are checked by knownNestedKeys.
//   - Each key carries its basic JSON type expectation: string (incl. enums),
//     boolean, number, array, object (shape / record / policy-helper). A
//     handful of union fields accept more than one type (e.g. forceLoginOrgUUID
//     string|array, strictPluginOnlyCustomization boolean|array).
//   - Enum MEMBERSHIP
//     and deep nested shapes are NOT validated: an enum field is checked only
//     as a string. This is intentionally conservative so no value that loads
//     today is flagged. The dynamic marketplace/plugin keys spread into the
//     schema are likewise not enumerable from
//     a static table, so a key absent here is reported as unrecognized rather
//     than hard-rejected.
//
// The accepted-type lists use the JSON type names reported to the user:
// "string", "boolean", "number", "array", "object".
var knownSettingKeys = map[string][]string{
	"$schema":                           {"string"},
	"additionalMarketplaces":            {"object"},
	"advisorModel":                      {"string"},
	"agentPushNotifEnabled":             {"boolean"},
	"agent":                             {"string"},
	"allowAllgocodeAiMcps":              {"boolean"},
	"allowedChannelPlugins":             {"array"},
	"allowedHttpHookUrls":               {"array"},
	"allowedMarketplaces":               {"object"},
	"allowedMcpServers":                 {"object"},
	"allowedProviders":                  {"array"},
	"allowgocodeInChromeWithManagedMcp": {"boolean"},
	"allowManagedHooksOnly":             {"boolean"},
	"allowManagedMcpServersOnly":        {"boolean"},
	"allowManagedPermissionRulesOnly":   {"boolean"},
	"alwaysThinkingEnabled":             {"boolean"},
	"apiKeyHelper":                      {"string"},
	"appendPlugins":                     {"array"},
	"askUserQuestionTimeout":            {"string"},
	"attributionProjectPolicy":          {"object"},
	"attributionProject":                {"string"},
	"autoCompactEnabled":                {"boolean"},
	"autoCompactWindow":                 {"number"},
	"autoContinueAtUsageLimit":          {"boolean"},
	"autoDreamEnabled":                  {"boolean"},
	"autoMemoryDirectory":               {"string"},
	"autoMemoryEnabled":                 {"boolean"},
	"autoScrollEnabled":                 {"boolean"},
	"autoUpdatesChannel":                {"string"},
	"autoUploadSessions":                {"boolean"},
	"availableModels":                   {"array"},
	"availableModelsMatch":              {"string"},
	"awaySummaryEnabled":                {"boolean"},
	"awsAuthRefresh":                    {"string"},
	"awsCredentialExport":               {"string"},
	"bashEditDiffEnabled":               {"boolean"},
	"bashOutputMaxChars":                {"number"},
	"blockedMarketplaces":               {"object"},
	"breakReminder":                     {"object"},
	"channelsEnabled":                   {"boolean"},
	"cleanupPeriodDays":                 {"number"},
	"companyAnnouncements":              {"array"},
	"crossSessionInbound":               {"string"},
	"daemonColdStart":                   {"string"},
	"defaultShell":                      {"string"},
	"deniedMcpServers":                  {"object"},
	"deniedModels":                      {"array"},
	"desktopSessionCleanupPeriodDays":   {"number"},
	"dialogExpiry":                      {"string"},
	"disableAgentView":                  {"boolean"},
	"disableAllHooks":                   {"boolean"},
	"disableArtifact":                   {"boolean"},
	"disableAutoMode":                   {"string"},
	"disableBundledSkills":              {"boolean"},
	"disableCommandPluginSources":       {"boolean"},
	"disabledMcpjsonServers":            {"array"},
	"disablegocodeAiConnectors":         {"boolean"},
	"disableRemoteControl":              {"boolean"},
	"disableSideloadFlags":              {"boolean"},
	"disableSkillShellExecution":        {"boolean"},
	"disableWorkflows":                  {"boolean"},
	"doneMeansMerged":                   {"boolean"},
	"editorMode":                        {"string"},
	"effortLevel":                       {"string"},
	"emojiCompletionEnabled":            {"boolean"},
	"enableAllProjectMcpServers":        {"boolean"},
	"enableArtifact":                    {"boolean"},
	"enabledMcpjsonServers":             {"array"},
	"enabledPlugins":                    {"object"},
	"enableWorkflows":                   {"boolean"},
	"enforceAvailableModels":            {"boolean"},
	"env":                               {"object"},
	"extraKnownMarketplaces":            {"object"},
	"fallbackModel":                     {"array"},
	"fastMode":                          {"boolean"},
	"fastModePerSessionOptIn":           {"boolean"},
	"feedbackDrafts":                    {"string"},
	"feedbackSurveyRate":                {"number"},
	"fileCheckpointingEnabled":          {"boolean"},
	"fileSuggestion":                    {"object"},
	"footerLinksRegexes":                {"array"},
	"forceLoginGatewayUrl":              {"string"},
	"forceLoginMethod":                  {"string"},
	"forceLoginOrgUUID":                 {"string", "array"},
	"forceRemoteSettingsRefresh":        {"boolean"},
	"gatewayInternalNetworks":           {"array"},
	"gcpAuthRefresh":                    {"string"},
	"gocodeMdExcludes":                  {"array"},
	"gocodeMd":                          {"string"},
	"hooks":                             {"object"},
	"httpHookAllowedEnvVars":            {"array"},
	"idleCompaction":                    {"boolean"},
	"includeCoAuthoredBy":               {"boolean"},
	"includeGitInstructions":            {"boolean"},
	"inputNeededNotifEnabled":           {"boolean"},
	"isolatePeerMachines":               {"boolean"},
	"keybindingFlavor":                  {"string"},
	"language":                          {"string"},
	"loopGuard":                         {"object"},
	"managedMcpServers":                 {"object"},
	"managedSourcesBehavior":            {"string"},
	"maxEffortLevel":                    {"string"},
	"maxProseWidth":                     {"number"},
	"minimumVersion":                    {"string"},
	"modelOverrides":                    {"object"},
	"modelPicker":                       {"object"},
	"modelPricing":                      {"object"},
	"modelProposedGoals":                {"string"},
	"modelSettings":                     {"object"},
	"model":                             {"string"},
	"otelHeadersHelper":                 {"string"},
	"outputStyle":                       {"string"},
	"parentSettingsBehavior":            {"string"},
	"permissions":                       {"object"},
	"plansDirectory":                    {"string"},
	"pluginConfigs":                     {"object"},
	"pluginSuggestionMarketplaces":      {"array"},
	"pluginTrustMessage":                {"string"},
	"policyHelper":                      {"object"},
	"policyHelpers":                     {"object"},
	"precomputeCompactionEnabled":       {"boolean"},
	"preferredNotifChannel":             {"string"},
	"prefersReducedMotion":              {"boolean"},
	"prependPlugins":                    {"array"},
	"processWrapper":                    {"string"},
	"promptCacheTtl":                    {"string"},
	"promptSuggestionEnabled":           {"boolean"},
	"proxyAuthHelper":                   {"string"},
	"prUrlTemplate":                     {"string"},
	"quietHours":                        {"object"},
	"remoteControlAtStartup":            {"boolean"},
	"remoteControl":                     {"object"},
	"remote":                            {"object"},
	"remoteTools":                       {"object"},
	"requiredMaximumVersion":            {"string"},
	"requiredMinimumVersion":            {"string"},
	"respectGitignore":                  {"boolean"},
	"respondToBashCommands":             {"boolean"},
	"sandbox":                           {"object"},
	"search":                            {"object"},
	"showClearContextOnPlanAccept":      {"boolean"},
	"showMessageTimestamps":             {"boolean"},
	"showThinkingSummaries":             {"boolean"},
	"showTurnDuration":                  {"boolean"},
	"skillListingBudgetFraction":        {"number"},
	"skillListingMaxDescChars":          {"number"},
	"skillOverrides":                    {"object"},
	"skipDangerousModePermissionPrompt": {"boolean"},
	"skipWebFetchPreflight":             {"boolean"},
	"skipWorkflowUsageWarning":          {"boolean"},
	"spellcheck":                        {"object"},
	"spinnerTipsEnabled":                {"boolean"},
	"spinnerTipsOverride":               {"object"},
	"spinnerVerbs":                      {"object"},
	"sshConfigs":                        {"array"},
	"statusLine":                        {"object"},
	"strictKnownMarketplaces":           {"object"},
	"strictPluginOnlyCustomization":     {"boolean", "array"},
	"subagentPromptCacheTtl":            {"string"},
	"subagentStatusLine":                {"object"},
	"switchModelsOnFlag":                {"boolean"},
	"syncgocodeAiPlugins":               {"boolean"},
	"syncgocodeAiSkills":                {"boolean"},
	"syntaxHighlightingDisabled":        {"boolean"},
	"taskOutputMaxChars":                {"number"},
	"teammateMode":                      {"string"},
	"terminalProgressBarEnabled":        {"boolean"},
	"terminalTitleFromRename":           {"boolean"},
	"theme":                             {"string"},
	"timeFormat":                        {"string"},
	"timeZone":                          {"string"},
	"todoFeatureEnabled":                {"boolean"},
	"totalTokensReminderAfterUserTurn":  {"boolean"},
	"totalTokensReminderBudget":         {"number"},
	"totalTokensReminder":               {"string"},
	"tui":                               {"string"},
	"ui":                                {"object"},
	"ultracode":                         {"boolean"},
	"verbose":                           {"boolean"},
	"viewMode":                          {"string"},
	"vimInsertModeRemaps":               {"object"},
	"voice":                             {"object"},
	"wheelScrollAccelerationEnabled":    {"boolean"},
	"workflowKeywordTriggerEnabled":     {"boolean"},
	"workflowSizeGuideline":             {"string"},
	"worktree":                          {"object"},
	"wslInheritsWindowsSettings":        {"boolean"},
	"xaaIdp":                            {"object"},
}

// jsonTypeName reports the JSON type name of a value decoded from JSON into an
// `any`, matching the names surfaced in the "Expected X, but received Y"
// diagnostics.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64, json.Number, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "object"
	}
}

// joinTypes renders an accepted-type list as "string" or "string or array".
func joinTypes(types []string) string {
	switch len(types) {
	case 0:
		return ""
	case 1:
		return types[0]
	default:
		return strings.Join(types[:len(types)-1], ", ") + " or " + types[len(types)-1]
	}
}

// validateSettings runs the advisory known-key + basic-type schema check over a
// parsed settings object and returns warning strings (empty when the object is
// fully valid). It never mutates obj and never drops a key; callers apply every
// key regardless of the warnings. Output is deterministic (keys sorted).
func validateSettings(obj map[string]any) []string {
	if len(obj) == 0 {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var warnings []string
	for _, k := range keys {
		accepted, known := knownSettingKeys[k]
		if !known {
			warnings = append(warnings, "Unrecognized field: "+k)
			continue
		}
		v := obj[k]
		// A null value is treated as "unset": every field in the
		// schema is optional, and the loader strips null-valued alias keys
		// before validating, so null never counts as a type violation here.
		if v == nil {
			continue
		}
		actual := jsonTypeName(v)
		if !containsString(accepted, actual) {
			warnings = append(warnings, fmt.Sprintf(
				"%s: Expected %s, but received %s", k, joinTypes(accepted), actual))
			continue
		}
		if nested, ok := knownNestedKeys[k]; ok {
			warnings = append(warnings, validateNested(k, nested, v.(map[string]any))...)
		}
	}
	return warnings
}

// knownNestedKeys validates the sub-keys of gocode-specific object settings
// (not part of the base schema).
var knownNestedKeys = map[string]map[string][]string{
	// "search": search engine settings (internal/tools/filetools,
	// internal/ugrep). The top-level "respectGitignore" also applies.
	"search": {
		"useUgrep":          {"boolean"},
		"ugrepPath":         {"string"},
		"preferSystemUgrep": {"boolean"},
		"includeHidden":     {"boolean"},
	},
	// "ui": the full-screen look (internal/ui): "style" is "modern" (the
	// default), "cards" or "classic"; "icons" is "unicode", "nerd" or
	// "ascii"; "animations" turns the modern style's animations on/off;
	// "sidebar" is "auto", "on" or "off"; "theme" names a theme and overrides
	// the top-level "theme".
	"ui": {
		"style":      {"string"},
		"icons":      {"string"},
		"animations": {"boolean"},
		"sidebar":    {"string"},
		"theme":      {"string"},
	},
}

func validateNested(parent string, known map[string][]string, obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var warnings []string
	for _, k := range keys {
		accepted, ok := known[k]
		if !ok {
			warnings = append(warnings, "Unrecognized field: "+parent+"."+k)
			continue
		}
		if obj[k] == nil {
			continue
		}
		if actual := jsonTypeName(obj[k]); !containsString(accepted, actual) {
			warnings = append(warnings, fmt.Sprintf(
				"%s.%s: Expected %s, but received %s", parent, k, joinTypes(accepted), actual))
		}
	}
	return warnings
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
