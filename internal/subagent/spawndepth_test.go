package subagent

import "testing"

// GOCODE_CODE_MAX_SUBAGENT_SPAWN_DEPTH is a digits-only whole number >= 1;
// anything else leaves the default cap.
func TestMaxSpawnDepthFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 0, "1": 1, "5": 5, "0": 0, "-2": 0, "2.5": 0, "abc": 0, " 4 ": 4, "+3": 0} {
		got := MaxSpawnDepthFromEnv(func(k string) string {
			if k == MaxSpawnDepthEnv {
				return v
			}
			return ""
		})
		if got != want {
			t.Errorf("%q → %d, want %d", v, got, want)
		}
	}
	if MaxSpawnDepthFromEnv(nil) != 0 {
		t.Error("nil getenv")
	}
}
