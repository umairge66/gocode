// Package envsec classifies and sanitizes environment variables for
// subprocess safety. Loading, merging and validating layered settings
// (userSettings / projectSettings / localSettings / policySettings /
// flagSettings, per-OS managed-settings "policy helpers" and remote-settings
// verification) lives in internal/settingsload, not here.
//
// This package covers three concerns, all data-driven and side-effect free:
//
//   - Credential / secret-bearing env var names (e.g. OPENROUTER_API_KEY,
//     AWS_SECRET_ACCESS_KEY), including the INPUT_* GitHub-Actions mirror names.
//   - Config-file-path env vars that can redirect tools at attacker-controlled
//     config (e.g. KUBECONFIG, NPM_CONFIG_USERCONFIG, XDG_CONFIG_HOME).
//   - Package-registry index/URL env vars (e.g. PIP_INDEX_URL, NPM_CONFIG_REGISTRY,
//     GOPROXY) with alias canonicalization and "already populated" checks.
//
// It also implements the removal of dangerous environment variables before a
// routed `sh` interpreter is spawned, and a heuristic that recognizes
// token/secret-shaped string values.
package envsec
