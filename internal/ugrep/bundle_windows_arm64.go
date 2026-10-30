//go:build windows && arm64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The windows/arm64 ugrep binary, when one is vendored under bin/windows_arm64.
//
//go:embed bin/VERSION bin/windows_arm64
var bundleFS embed.FS

const bundleDir = "bin/windows_arm64"
