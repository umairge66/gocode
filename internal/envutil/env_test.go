package envutil

import (
	"testing"
)

func TestMaxTurnsFromEnv(t *testing.T) {
	for in, want := range map[string]int{"": 0, "  ": 0, "5": 5, " 12 ": 12, "1,000": 1000, "1e3": 1000} {
		got, err := MaxTurnsFromEnv(in)
		if err != nil || got != want {
			t.Errorf("MaxTurnsFromEnv(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-3", "2.5", "abc", "Infinity"} {
		if _, err := MaxTurnsFromEnv(in); err == nil {
			t.Errorf("MaxTurnsFromEnv(%q) should fail", in)
		}
	}
	_, err := MaxTurnsFromEnv("abc")
	if err == nil || err.Error() != `GOCODE_CODE_MAX_TURNS must be a positive integer; got "abc"` {
		t.Errorf("message: %v", err)
	}
}
