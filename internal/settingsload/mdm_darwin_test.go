//go:build darwin

package settingsload

import (
	"strings"
	"testing"
)

func TestDarwinPlistPaths(t *testing.T) {
	got := MacManagedPlistPaths("alice")
	if len(got) != 2 || !strings.HasPrefix(got[0], "/Library/Managed Preferences/alice/") ||
		got[1] != "/Library/Managed Preferences/"+MacManagedPreferencesDomain+".plist" {
		t.Fatalf("paths = %v", got)
	}
}

func TestDarwinPlatformReaderRuns(t *testing.T) {
	r := readPlatformOSPolicy()
	if r.HKLM != nil || r.HKCU != nil || r.IsWSL {
		t.Fatalf("macOS readout must carry plists only: %+v", r)
	}
	for _, d := range r.Plists {
		if d.JSON == nil && d.UnreadReason == "" {
			t.Errorf("unreadable document without reason: %+v", d)
		}
	}
}
