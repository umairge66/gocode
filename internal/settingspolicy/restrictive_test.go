package settingspolicy

import (
	"reflect"
	"testing"
)

func TestApplyRestrictiveCaps(t *testing.T) {
	top := map[string]any{
		"maxEffortLevel":                "max",
		"crossSessionInbound":           "hold",
		"enableArtifact":                true,
		"strictPluginOnlyCustomization": []any{"skills"},
	}
	lower := map[string]any{
		"maxEffortLevel":                "medium",
		"crossSessionInbound":           "refuse",
		"disablePluginDirectory":        "yes", // normalized to true
		"permissions":                   map[string]any{"disableBypassPermissionsMode": "disable"},
		"autoUploadSessions":            true, // permissive, lower tier only
		"strictPluginOnlyCustomization": []any{"agents", "skills"},
		"sandbox":                       map[string]any{"network": map[string]any{"allowLocalBinding": true}},
	}
	result := map[string]any{}
	DeepMerge(result, lower, false)
	DeepMerge(result, top, false)
	ApplyRestrictiveCaps(result, []map[string]any{top, lower})

	checks := map[string]any{
		"maxEffortLevel":         "medium",
		"crossSessionInbound":    "refuse",
		"disablePluginDirectory": true,
		"enableArtifact":         true, // no tier sets the restrictive false; top keeps its own
	}
	for k, want := range checks {
		if result[k] != want {
			t.Errorf("%s = %v; want %v", k, result[k], want)
		}
	}
	if v, _ := GetPath(result, []string{"permissions", "disableBypassPermissionsMode"}); v != "disable" {
		t.Errorf("nested restrictive: %v", v)
	}
	if _, has := result["autoUploadSessions"]; has {
		t.Errorf("lower-only permissive value must be dropped")
	}
	if _, has := result["sandbox"]; has {
		t.Errorf("dropping the only sandbox key must prune the empty parents: %v", result["sandbox"])
	}
	if !reflect.DeepEqual(result["strictPluginOnlyCustomization"], []any{"skills", "agents"}) {
		t.Errorf("strictPluginOnlyCustomization union: %v", result["strictPluginOnlyCustomization"])
	}

	// A true anywhere wins over the list form.
	r2 := map[string]any{"strictPluginOnlyCustomization": []any{"skills"}}
	ApplyRestrictiveCaps(r2, []map[string]any{{"strictPluginOnlyCustomization": []any{"skills"}}, {"strictPluginOnlyCustomization": true}})
	if r2["strictPluginOnlyCustomization"] != true {
		t.Errorf("true must win: %v", r2["strictPluginOnlyCustomization"])
	}
}

func TestMergeAdminTiersAppliesRestrictiveCaps(t *testing.T) {
	tiers := []map[string]any{
		{"managedSourcesBehavior": "merge", "allowManagedHooksOnly": false},
		{"allowManagedHooksOnly": true},
	}
	res := MergeAdminTiers(tiers, MergeBehavior, false, 0, DefaultMergeDeps(nil))
	if res.Admin["allowManagedHooksOnly"] != true {
		t.Fatalf("DefaultMergeDeps(nil) must wire Ld: %v", res.Admin)
	}
}

func TestMergeLayered(t *testing.T) {
	dst := map[string]any{"a": []any{"x"}, "o": map[string]any{"k": 1.0, "j": 1.0}, "fallbackModel": []any{"m1"}}
	MergeLayered(dst, map[string]any{"a": []any{"y", "x"}, "o": map[string]any{"k": 2.0}, "fallbackModel": []any{"m2"}})
	want := map[string]any{"a": []any{"x", "y"}, "o": map[string]any{"k": 2.0, "j": 1.0}, "fallbackModel": []any{"m2"}}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("got %v", dst)
	}
}
