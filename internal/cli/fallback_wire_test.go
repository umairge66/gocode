package cli

import "testing"

func TestFallbackModelFlagParsed(t *testing.T) {
	d, rest := parseDispatchDefaults([]string{"--fallback-model", "a, b", "-p", "hi"})
	if d == nil || d.FallbackModel != "a, b" {
		t.Fatalf("defaults = %+v", d)
	}
	for _, r := range rest {
		if r == "--fallback-model" {
			t.Fatalf("flag left in rest: %v", rest)
		}
	}
	d, _ = parseDispatchDefaults([]string{"--fallback-model=x"})
	if d == nil || d.FallbackModel != "x" {
		t.Fatalf("inline form = %+v", d)
	}
}
