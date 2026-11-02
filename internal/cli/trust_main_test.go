package cli

import (
	"os"
	"testing"
)

// TestMain runs the package's tests with every folder trusted: most of them
// put project settings in temp folders and expect them to apply. The
// workspace-trust tests opt back into the real trust store per Env
// (useRealTrust), so production behaviour is never weakened.
//
// The tests also get a temp directory of their own, removed afterwards, so
// the sessions they build put their scratchpads there and not in the
// user's real temp directory.
func TestMain(m *testing.M) {
	defaultTrustCheck = func(*Env, string) bool { return true }
	tmp, err := os.MkdirTemp("", "gocode-cli-test-")
	if err == nil {
		for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
			_ = os.Setenv(k, tmp)
		}
	}
	code := m.Run()
	if err == nil {
		_ = os.RemoveAll(tmp)
	}
	os.Exit(code)
}

// useRealTrust makes env consult the real trust store (its HOME /
// GOCODE_CONFIG_DIR .gocode.json) instead of the test default.
func useRealTrust(env *Env) {
	env.trustCheck = func(cwd string) bool { return folderTrustedInStore(env, cwd) }
}
