package envutil

import (
	"reflect"
	"testing"
)

func TestEnvNameSpellings(t *testing.T) {
	for cur, legacy := range map[string]string{
		"GOCODE_CODE_SIMPLE":             "gocode_CODE_SIMPLE",
		"GOCODE_CODE_DISABLE_GOCODE_MDS": "gocode_CODE_DISABLE_gocode_MDS",
		"GOCODE_PLUGIN_ROOT":             "gocode_PLUGIN_ROOT",
	} {
		if got := LegacyEnvName(cur); got != legacy {
			t.Errorf("LegacyEnvName(%s) = %s, want %s", cur, got, legacy)
		}
		if got := CurrentEnvName(legacy); got != cur {
			t.Errorf("CurrentEnvName(%s) = %s, want %s", legacy, got, cur)
		}
	}
	for _, not := range []string{"PATH", "gocode", "gocode_md", "GOCODE_"} {
		if LegacyEnvName(not) != "" || CurrentEnvName(not) != "" {
			t.Errorf("%q should not map", not)
		}
	}
}

// An old-spelling variable becomes visible under the new name, without
// overriding a new-spelling value that is already set.
func TestAliasLegacyEnv(t *testing.T) {
	env := map[string]string{"GOCODE_CODE_SIMPLE": "keep"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	set := func(k, v string) error { env[k] = v; return nil }
	AliasLegacyEnv([]string{"gocode_CODE_SIMPLE=old", "gocode_CODE_MAX_TURNS=7", "HOME=/h"}, lookup, set)
	want := map[string]string{"GOCODE_CODE_SIMPLE": "keep", "GOCODE_CODE_MAX_TURNS": "7"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

func TestWithLegacyEnv(t *testing.T) {
	got := WithLegacyEnv([]string{"gocode=1", "GOCODE_PID=42"})
	want := []string{"gocode=1", "GOCODE_PID=42", "gocode_PID=42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
