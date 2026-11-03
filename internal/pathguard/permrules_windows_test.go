//go:build windows

package pathguard

import (
	"path/filepath"
	"strings"
	"testing"
)

// On real Windows paths, an absolute deny rule written in any of the
// accepted forms covers files beneath it.
func TestWindowsAbsoluteRuleOnRealPath(t *testing.T) {
	dir := t.TempDir() // e.g. D:\a\_temp\TestX\001
	file := filepath.Join(dir, "creds.txt")
	slash := filepath.ToSlash(dir) // D:/a/_temp/...
	posix := "/" + strings.ToLower(slash[:1]) + slash[2:]
	var r PathRuleRoots
	for _, rule := range []string{"/" + posix + "/**", "//" + slash + "/**", "//" + dir + `\**`, "//" + strings.ToUpper(slash) + "/**"} {
		if !r.PathRuleMatches(rule, "projectSettings", file, false) {
			t.Errorf("rule %s did not cover %s", rule, file)
		}
	}
}
