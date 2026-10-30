// Package pluginmgr is the plugin/marketplace install manager: the machinery
// behind the `/plugin marketplace add|remove|update` and
// `/plugin install|uninstall|enable|disable` commands. It adds and removes
// marketplaces, plans and performs plugin installs, and records the on-disk
// cache layout + the `enabledPlugins` setting that the session's plugin
// discovery (internal/cli/session.go discoverPlugins) reads back.
//
// # Scope
//
// This package OWNS the installer side of the subsystem. The structural halves
// it builds on live elsewhere:
//
//   - internal/plugins — manifest + marketplace index parsing, the source
//     union, MarketplaceFromGitHub, LoadMarketplaceDir, LoadPluginDir,
//     DiscoverDir, PluginCacheDir / MarketplacesDir / KnownMarketplacesFile,
//     and the allow/block Policy.
//   - internal/pluginfetch — the concrete plugins.Fetcher (git commit-pin,
//     url/archive sha256, npm, local passthrough) behind injectable seams.
//
// Network/exec effects stay behind the injected plugins.Fetcher; every
// filesystem mutation goes through the os package at paths derived from the
// configured cache root, and reads for the discovery proof go through
// fsfacade.OSFS so the real plugins loader can be run against the result.
//
// # Cache layout and state files
//
// Install root (plugins.PluginCacheDir): <configHome>/plugins (env override
// GOCODE_CODE_PLUGIN_CACHE_DIR). Under it:
//
//	cache/<marketplace>/<name>[/<version>]   installed plugin tree
//	marketplaces/<name>                      materialized marketplace tree
//	known_marketplaces.json                  marketplace registry
//	installed_plugins_v2.json                installed-plugin registry
//
// discoverPlugins scans cache/ one level deep for marketplace dirs, then
// DiscoverDir(cache/<marketplace>) loads each child as a plugin; the optional
// /<version> leaf is absorbed by LoadPluginDir's single-wrapper descent. The
// manager writes exactly that layout.
//
// known_marketplaces.json is a flat object keyed by
// marketplace name; each value is {source, installLocation, lastUpdated,
// autoUpdate?}. installed_plugins_v2.json is
// {version:2, plugins:{<id>:[{scope,installPath,version,installedAt,
// lastUpdated,gitCommitSha?}]}} keyed by the "name@marketplace" id.
//
// The `enabledPlugins` setting lives in the user settings file; it is a flat
// map of "name@marketplace" -> bool. Install sets it true, disable false,
// enable true, uninstall/prune drops the key — always as a read-modify-write that
// preserves every other key. This writer should later unify with
// internal/settingsload's settings writer (currently unexported).
//
// Removing a marketplace first turns its dependent plugins off.
// enabledPlugins resolution: an explicit entry wins, else defaultEnabled.
package pluginmgr
