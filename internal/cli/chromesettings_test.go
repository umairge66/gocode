package cli

import "testing"

func TestScreenshotAfterActionsSetting(t *testing.T) {
	on := map[string]any{"chrome": map[string]any{"screenshotAfterActions": true}}
	off := map[string]any{"chrome": map[string]any{"screenshotAfterActions": false}}
	cases := []struct {
		env      string
		settings map[string]any
		want     bool
	}{
		{"", nil, true},
		{"", map[string]any{"chrome": map[string]any{}}, true},
		{"", on, true},
		{"", off, false},
		{"0", on, false},
		{"false", nil, false},
		{"1", off, true},
		{"true", off, true},
		{"maybe", off, false}, // neither truthy nor falsy: the setting decides
	}
	for _, tc := range cases {
		if got := screenshotAfterActionsFrom(tc.env, tc.settings); got != tc.want {
			t.Errorf("env %q settings %v: got %v, want %v", tc.env, tc.settings, got, tc.want)
		}
	}
}
