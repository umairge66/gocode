package statusline

import (
	"testing"
)

func TestConfig_PaddingValue(t *testing.T) {
	n := func(i int) *int { return &i }
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil", nil, 0},
		{"unset", &Config{Type: "command", Command: "c"}, 0},
		{"two", &Config{Padding: n(2)}, 2},
		{"negative clamped", &Config{Padding: n(-3)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.PaddingValue(); got != tc.want {
				t.Errorf("PaddingValue() = %d, want %d", got, tc.want)
			}
		})
	}
}
