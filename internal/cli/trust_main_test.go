package cli

import (
	"os"
	"testing"
)

// TestMain runs the package's tests with every folder trusted: most of them
// put project settings in temp folders and expect them to apply. The
// workspace-trust tests opt back into the real trust store per Env
// (useRealTrust), so production behaviour is never weakened.
func TestMain(m *testing.M) {
	defaultTrustCheck = func(*Env, string) bool { return true }
	os.Exit(m.Run())
}

// useRealTrust makes env consult the real trust store (its HOME /
// GOCODE_CONFIG_DIR .gocode.json) instead of the test default.
func useRealTrust(env *Env) {
	env.trustCheck = func(cwd string) bool { return folderTrustedInStore(env, cwd) }
}
