//go:build linux && arm64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The linux/arm64 ugrep binary, when one is vendored under bin/linux_arm64.
//
//go:embed bin/VERSION bin/linux_arm64
var bundleFS embed.FS

const bundleDir = "bin/linux_arm64"
