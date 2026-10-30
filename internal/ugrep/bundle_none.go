//go:build gocode_nobundledugrep || !((linux || darwin || windows) && (amd64 || arm64))

package ugrep

import "embed"

// No ugrep is embedded for this platform (or the build used the
// gocode_nobundledugrep tag): ResolveDetailed falls back to a ugrep on PATH,
// then to the built-in Go engine.
var bundleFS embed.FS

const bundleDir = ""
