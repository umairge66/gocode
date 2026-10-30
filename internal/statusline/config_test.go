package statusline

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConfig_UnmarshalSettingsShape(t *testing.T) {
	raw := `{"type":"command","command":"my-status.sh","padding":2,"refreshInterval":5,"hideVimModeIndicator":true}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Type != "command" || c.Command != "my-status.sh" {
		t.Errorf("type/command = %q/%q", c.Type, c.Command)
	}
	if c.Padding == nil || *c.Padding != 2 {
		t.Errorf("padding = %v, want 2", c.Padding)
	}
	if c.RefreshInterval == nil || *c.RefreshInterval != 5 {
		t.Errorf("refreshInterval = %v, want 5", c.RefreshInterval)
	}
	if !c.HideVimModeIndicator {
		t.Error("hideVimModeIndicator = false, want true")
	}
}

func TestConfig_IsCommand(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil", nil, false},
		{"empty type", &Config{Command: "c"}, false},
		{"wrong type", &Config{Type: "builtin", Command: "c"}, false},
		{"no command", &Config{Type: "command"}, false},
		{"ok", &Config{Type: "command", Command: "c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsCommand(); got != tc.want {
				t.Errorf("IsCommand() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfig_RefreshIntervalDuration(t *testing.T) {
	n := func(i int) *int { return &i }
	cases := []struct {
		name string
		cfg  *Config
		want time.Duration
	}{
		{"nil config", nil, 0},
		{"unset", &Config{Type: "command", Command: "c"}, 0},
		{"5s", &Config{RefreshInterval: n(5)}, 5 * time.Second},
		{"clamped up from 0", &Config{RefreshInterval: n(0)}, 1 * time.Second},
		{"clamped up from negative", &Config{RefreshInterval: n(-10)}, 1 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RefreshIntervalDuration(); got != tc.want {
				t.Errorf("RefreshIntervalDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}
