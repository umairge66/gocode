//go:build darwin && arm64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The darwin/arm64 ugrep binary, when one is vendored under bin/darwin_arm64.
//
//go:embed bin/VERSION bin/darwin_arm64
var bundleFS embed.FS

const bundleDir = "bin/darwin_arm64"
