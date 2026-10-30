//go:build windows && amd64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The windows/amd64 ugrep binary, when one is vendored under bin/windows_amd64.
//
//go:embed bin/VERSION bin/windows_amd64
var bundleFS embed.FS

const bundleDir = "bin/windows_amd64"
