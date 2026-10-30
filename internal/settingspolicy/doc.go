// Package settingspolicy is gocode "settings and managed policy"
// configuration core.
//
// The settings domain covers:
//
//   - the Gocode settings *schema*, covering permissions, modelPicker,
//     sandbox/credentials, MCP servers, plugins, marketplaces, hooks, OTEL,
//     provider env vars (AWS/Bedrock/Vertex);
//   - the *settings source* model and precedence: userSettings,
//     projectSettings, localSettings, managedSettings, flagSettings,
//     policySettings, defaultSettings;
//   - the *merge engine* that layers admin tiers under a
//     parentSettingsBehavior of "first-wins" (default) or "merge", strips
//     secrets and non-inheritable keys from lower tiers, and applies
//     restrictive policy caps;
//   - managed-policy *enforcement* keys (strictPluginOnlyCustomization,
//     allowed/blocked MCP servers / marketplaces / models / domains /
//     read-paths / hooks, bypassPermissions, forceLoginOrgUUID);
//   - the *settings loader* result shape {settings, errors, loadState} with
//     load states loaded / absent / unreadable / unparsable / malformed /
//     didNotLoad;
//   - *signed-cache* validation reason codes for remote managed settings
//     (unsigned, bad_signature, bad_chain, sub_mismatch, ws_mismatch,
//     sha_mismatch, unsupported_version).
//
// This package holds the self-contained, general-purpose core:
//
//   - path get/set/delete over dynamic settings objects (paths.go);
//   - the settings-source / scope model and precedence selection
//     (sources.go);
//   - the enum/reason-code vocabularies (status.go);
//   - skill-name normalization (skills.go);
//   - the admin-tier merge orchestration (secret/key stripping drive,
//     sandbox-platform handling, model-override resolution), the deep merge
//     (deepmerge.go) and restrictive caps (restrictive.go), with every
//     primitive injectable through MergeDeps (merge.go).
package settingspolicy
