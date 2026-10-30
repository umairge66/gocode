//go:build darwin && amd64 && !gocode_nobundledugrep

package ugrep

import "embed"

// The darwin/amd64 ugrep binary, when one is vendored under bin/darwin_amd64.
//
//go:embed bin/VERSION bin/darwin_amd64
var bundleFS embed.FS

const bundleDir = "bin/darwin_amd64"
