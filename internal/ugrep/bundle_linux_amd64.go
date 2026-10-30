//go:build linux && amd64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The linux/amd64 ugrep binary, when one is vendored under bin/linux_amd64.
//
//go:embed bin/VERSION bin/linux_amd64
var bundleFS embed.FS

const bundleDir = "bin/linux_amd64"
