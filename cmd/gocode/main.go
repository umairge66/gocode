// Command gocode is the top-level CLI entrypoint for the Gocode agentic coding
// tool. The argument parsing, fast-path dispatch
// and subcommand surface live in internal/cli; this file only wires the
// process together.
package main

import (
	"os"

	"github.com/umairge66/gocode/internal/cli"

	// Bundled root certificates, used only when the system has none (minimal
	// containers without ca-certificates); otherwise the system store is used.
	_ "golang.org/x/crypto/x509roots/fallback"
)

func main() {
	os.Exit(cli.Main())
}
