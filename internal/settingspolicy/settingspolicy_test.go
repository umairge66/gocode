package settingspolicy

import (
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/platform"
)

func TestGetPath(t *testing.T) {
	root := map[string]any{
		"permissions": map[string]any{"defaultMode": "acceptEdits"},
		"flat":        1.0,
	}
	if v, ok := GetPath(root, []string{"permissions", "defaultMode"}); !ok || v != "acceptEdits" {
		t.Fatalf("nested get = %v, %v", v, ok)
	}
	if v, ok := GetPath(root, []string{"flat"}); !ok || v != 1.0 {
		t.Fatalf("flat get = %v, %v", v, ok)
	}
	if _, ok := GetPath(root, []string{"missing"}); ok {
		t.Fatal("missing key should not be found")
	}
	if _, ok := GetPath(root, []string{"flat", "deeper"}); ok {
		t.Fatal("descend into non-object should not be found")
	}
}

func TestSetPathCreatesAndClones(t *testing.T) {
	root := map[string]any{}
	SetPath(root, []string{"a", "b", "c"}, 42.0)
	if v, ok := GetPath(root, []string{"a", "b", "c"}); !ok || v != 42.0 {
		t.Fatalf("set deep = %v, %v", v, ok)
	}

	// Copy-on-write: a nested object shared before the set must not be mutated.
	shared := map[string]any{"keep": true}
	root2 := map[string]any{"a": shared}
	SetPath(root2, []string{"a", "new"}, "x")
	if _, exists := shared["new"]; exists {
		t.Fatal("set mutated the shared nested object (copy-on-write violated)")
	}
	if v, ok := GetPath(root2, []string{"a", "new"}); !ok || v != "x" {
		t.Fatalf("set on cloned branch = %v, %v", v, ok)
	}
	if v, ok := GetPath(root2, []string{"a", "keep"}); !ok || v != true {
		t.Fatalf("set lost existing sibling = %v, %v", v, ok)
	}
}

func TestDeletePathPrunesEmptyAncestors(t *testing.T) {
	root := map[string]any{
		"a": map[string]any{"b": map[string]any{"c": 1.0}},
		"x": 2.0,
	}
	DeletePath(root, []string{"a", "b", "c"})
	if _, ok := root["a"]; ok {
		t.Fatal("empty ancestor chain should have been pruned")
	}
	if root["x"] != 2.0 {
		t.Fatal("unrelated key should survive")
	}

	// Non-empty ancestor must be retained.
	root2 := map[string]any{"a": map[string]any{"b": 1.0, "c": 2.0}}
	DeletePath(root2, []string{"a", "b"})
	if v, ok := GetPath(root2, []string{"a", "c"}); !ok || v != 2.0 {
		t.Fatalf("non-empty ancestor pruned incorrectly = %v, %v", v, ok)
	}
}

func TestResolveEffectiveSources(t *testing.T) {
	ordered := []Source{
		SourcePolicy, SourceManaged, SourceFlag, SourceLocal, SourceProject, SourceUser, SourceDefault,
	}
	got := ResolveEffectiveSources([]Source{SourceUser}, ordered)
	// flag + policy are force-added; result keeps canonical order.
	want := []Source{SourcePolicy, SourceFlag, SourceUser}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effective sources = %v, want %v", got, want)
	}
}

func TestNormalizeSkillName(t *testing.T) {
	if got := NormalizeSkillName("review"); got != "gocode-skills:review" {
		t.Fatalf("bare name = %q", got)
	}
	if got := NormalizeSkillName("gocode-skills:review"); got != "gocode-skills:review" {
		t.Fatalf("already-prefixed name changed = %q", got)
	}
}

func TestResolveModelOverrides(t *testing.T) {
	overrides := map[string]any{"opus": "pinned"}

	// Override tier is higher-priority than availableModels -> kept.
	result := map[string]any{}
	tiers := []map[string]any{
		{"modelOverrides": overrides},
		{"availableModels": []any{"a"}},
	}
	ResolveModelOverrides(result, tiers, false)
	if _, ok := result["modelOverrides"]; !ok {
		t.Fatal("override above availableModels should be kept")
	}

	// Override tier is lower-priority than availableModels -> dropped.
	result = map[string]any{"modelOverrides": overrides}
	tiers = []map[string]any{
		{"availableModels": []any{"a"}},
		{"modelOverrides": overrides},
	}
	ResolveModelOverrides(result, tiers, false)
	if _, ok := result["modelOverrides"]; ok {
		t.Fatal("override below availableModels should be dropped")
	}
}

func TestDeepMergeTable(t *testing.T) {
	cases := []struct {
		name         string
		dst, src     map[string]any
		appendArrays bool // false=replaceCustomizer, true=appendCustomizer
		want         map[string]any
	}{
		{
			// mergeWith(dst, src): src wins conflicts within one call; the
			// orchestrator gets top-wins by layering (see the layering test).
			name: "nested maps merge recursively, src wins conflicts",
			dst:  map[string]any{"a": map[string]any{"x": 1.0, "y": 2.0}, "k": "dst"},
			src:  map[string]any{"a": map[string]any{"y": 99.0, "z": 3.0}, "k2": "src"},
			want: map[string]any{
				"a":  map[string]any{"x": 1.0, "y": 99.0, "z": 3.0},
				"k":  "dst",
				"k2": "src",
			},
		},
		{
			name: "alwaysSpecialKeys member key (availableModels) takes special combine: src replaces dst",
			dst:  map[string]any{"availableModels": []any{"old-1", "old-2"}},
			src:  map[string]any{"availableModels": []any{"new-1"}},
			want: map[string]any{"availableModels": []any{"new-1"}},
		},
		{
			name: "non-member array key takes default union (src then dst, deduped)",
			dst:  map[string]any{"plugins": []any{"a", "b"}},
			src:  map[string]any{"plugins": []any{"b", "c"}},
			// arrayUnionDefault: uniq(src+dst) = uniq(["b","c","a","b"]) = ["b","c","a"]
			want: map[string]any{"plugins": []any{"b", "c", "a"}},
		},
		{
			name:         "replaceOnlySpecialKeys key (awsPairs) under replaceCustomizer (appendArrays=false) takes special replace",
			appendArrays: false,
			dst:          map[string]any{"awsPairs": []any{map[string]any{"accessKeyIdVar": "X"}}},
			src:          map[string]any{"awsPairs": []any{map[string]any{"accessKeyIdVar": "Y"}}},
			// specialCombine: src is array, key==awsPairs but dst vars (X) not canonical -> src
			want: map[string]any{"awsPairs": []any{map[string]any{"accessKeyIdVar": "Y"}}},
		},
		{
			name:         "replaceOnlySpecialKeys key (ripgrep) under appendCustomizer (appendArrays=true) falls through to default union",
			appendArrays: true,
			dst:          map[string]any{"ripgrep": []any{"a", "b"}},
			src:          map[string]any{"ripgrep": []any{"c"}},
			// appendCustomizer: ripgrep not in alwaysSpecialKeys -> arrayUnionDefault union src+dst = ["c","a","b"]
			want: map[string]any{"ripgrep": []any{"c", "a", "b"}},
		},
		{
			name:         "replaceOnlySpecialKeys key (ripgrep) under replaceCustomizer takes special replace",
			appendArrays: false,
			dst:          map[string]any{"ripgrep": []any{"a", "b"}},
			src:          map[string]any{"ripgrep": []any{"c"}},
			// replaceCustomizer: ripgrep in replaceOnlySpecialKeys -> specialCombine replaces with src
			want: map[string]any{"ripgrep": []any{"c"}},
		},
		{
			name: "fallbackModel arrays replace (not unioned)",
			dst:  map[string]any{"fallbackModel": []any{"old"}},
			src:  map[string]any{"fallbackModel": []any{"new"}},
			want: map[string]any{"fallbackModel": []any{"new"}},
		},
		{
			name: "primitive source overrides primitive dst",
			dst:  map[string]any{"mode": "a"},
			src:  map[string]any{"mode": "b"},
			want: map[string]any{"mode": "b"},
		},
		{
			name: "new key from src is added",
			dst:  map[string]any{"a": 1.0},
			src:  map[string]any{"b": 2.0},
			want: map[string]any{"a": 1.0, "b": 2.0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			DeepMerge(tc.dst, tc.src, tc.appendArrays)
			if !reflect.DeepEqual(tc.dst, tc.want) {
				t.Fatalf("DeepMerge result =\n  %#v\nwant\n  %#v", tc.dst, tc.want)
			}
		})
	}
}

func TestDeepMergeUndefinedSkip(t *testing.T) {
	// An undefined source value must not overwrite an existing
	// key, but is written when the key is absent (assignMergeValue).
	dst := map[string]any{"keep": "existing"}
	src := map[string]any{"keep": jsUndefined, "fresh": jsUndefined}
	DeepMerge(dst, src, false)
	if dst["keep"] != "existing" {
		t.Fatalf("undefined source overwrote existing key: %v", dst["keep"])
	}
	if v, ok := dst["fresh"]; !ok || !isUndefined(v) {
		t.Fatalf("undefined source should populate absent key: %v, %v", v, ok)
	}

	// Under a alwaysSpecialKeys key, specialCombine returns dst unchanged for an undefined source.
	dst2 := map[string]any{"availableModels": []any{"a"}}
	DeepMerge(dst2, map[string]any{"availableModels": jsUndefined}, false)
	if !reflect.DeepEqual(dst2["availableModels"], []any{"a"}) {
		t.Fatalf("specialCombine should leave dst untouched for undefined src: %v", dst2["availableModels"])
	}
}

func TestDeepMergePrecedenceLayering(t *testing.T) {
	// Mirrors the orchestrator: merge lower tiers first (lowest priority
	// applied first), then the top tier last, so the top tier wins conflicts.
	result := map[string]any{}
	low := map[string]any{"a": 1.0, "shared": map[string]any{"x": "low"}}
	mid := map[string]any{"a": 2.0, "b": 2.0, "shared": map[string]any{"y": "mid"}}
	top := map[string]any{"a": 3.0, "shared": map[string]any{"x": "top"}}

	DeepMerge(result, low, false)
	DeepMerge(result, mid, false)
	DeepMerge(result, top, false)

	want := map[string]any{
		"a":      3.0,
		"b":      2.0,
		"shared": map[string]any{"x": "top", "y": "mid"},
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("layered merge =\n  %#v\nwant\n  %#v", result, want)
	}
}

func TestDeepMergeAwsPairsSuppressor(t *testing.T) {
	// dst references a canonical AWS var not present in src -> specialCombine concats a
	// parent-pair suppressor entry.
	dst := map[string]any{"awsPairs": []any{map[string]any{"accessKeyIdVar": "AWS_ACCESS_KEY_ID"}}}
	src := map[string]any{"awsPairs": []any{map[string]any{"accessKeyIdVar": "MY_KEY"}}}
	DeepMerge(dst, src, false) // replaceCustomizer: awsPairs in replaceOnlySpecialKeys -> specialCombine
	got, _ := dst["awsPairs"].([]any)
	if len(got) != 2 {
		t.Fatalf("expected src pair + 1 suppressor, got %#v", got)
	}
	if !reflect.DeepEqual(got[0], map[string]any{"accessKeyIdVar": "MY_KEY"}) {
		t.Fatalf("src pair should come first: %#v", got[0])
	}
	sup, _ := got[1].(map[string]any)
	if sup["accessKeyIdVar"] != "AWS_ACCESS_KEY_ID" || sup["secretAccessKeyVar"] != "_PARENT_PAIR_SUPPRESSOR_1_" {
		t.Fatalf("unexpected suppressor entry: %#v", sup)
	}
}

// testDeps provides a simple, deterministic implementation of the
// merge primitives so the orchestration can be exercised end to end.
func testDeps(plat string) MergeDeps {
	return MergeDeps{
		DeepMerge:            DeepMerge,
		StripSecrets:         func(s map[string]any) map[string]any { return cloneShallow(s) },
		ApplyRestrictiveCaps: func(result map[string]any, tiers []map[string]any) {},
		CurrentPlatform:      func() string { return plat },
	}
}

// TestDefaultMergeDepsWiring confirms the merge primitives are
// wired: StripSecrets -> secretpaths.StripSecrets (removes credential paths)
// and CurrentPlatform -> platform.CurrentPlatform.
func TestDefaultMergeDepsWiring(t *testing.T) {
	deps := DefaultMergeDeps(func(result map[string]any, tiers []map[string]any) {})

	// StripSecrets must remove a known credential key and keep others.
	stripped := deps.StripSecrets(map[string]any{
		"apiKeyHelper": "echo $KEY",
		"model":        "claude-opus",
	})
	if _, ok := stripped["apiKeyHelper"]; ok {
		t.Errorf("StripSecrets did not remove credential key apiKeyHelper: %#v", stripped)
	}
	if stripped["model"] != "claude-opus" {
		t.Errorf("StripSecrets dropped non-secret key model: %#v", stripped)
	}

	// CurrentPlatform must delegate to internal/platform.CurrentPlatform.
	if deps.CurrentPlatform == nil {
		t.Fatal("CurrentPlatform not wired")
	}
	if got, want := deps.CurrentPlatform(), platform.CurrentPlatform(); got != want {
		t.Errorf("CurrentPlatform() = %q, want %q (platform.CurrentPlatform)", got, want)
	}

	// End-to-end: a lower tier's credential key must not be inherited into the
	// merged admin result.
	tiers := []map[string]any{
		{"managedSourcesBehavior": "merge", "model": "top"},
		{"apiKeyHelper": "leak", "model": "low", "cleanupPeriodDays": 30.0},
	}
	res := MergeAdminTiers(tiers, MergeBehavior, false, 0, deps)
	if !res.Merged {
		t.Fatal("expected tiers to be merged")
	}
	if _, ok := res.Admin["apiKeyHelper"]; ok {
		t.Errorf("credential key apiKeyHelper leaked from lower tier into merged admin: %#v", res.Admin)
	}
	if res.Admin["cleanupPeriodDays"] != 30.0 {
		t.Errorf("non-secret lower-tier key cleanupPeriodDays not inherited: %#v", res.Admin)
	}
}

func TestMergeAdminTiersFirstWins(t *testing.T) {
	tiers := []map[string]any{
		{"a": 1.0},
		{"a": 2.0, "b": 3.0},
	}
	got := MergeAdminTiers(tiers, FirstWins, false, 0, testDeps("linux"))
	if got.Merged {
		t.Fatal("first-wins should not merge")
	}
	if !reflect.DeepEqual(got.Admin, map[string]any{"a": 1.0}) {
		t.Fatalf("first-wins admin = %v", got.Admin)
	}
}

func TestMergeAdminTiersMerge(t *testing.T) {
	tiers := []map[string]any{
		{"a": 1.0},
		{"a": 99.0, "b": 2.0},
	}
	got := MergeAdminTiers(tiers, MergeBehavior, false, 0, testDeps("linux"))
	if !got.Merged {
		t.Fatal("merge behavior with 2 tiers should merge")
	}
	// Top tier wins on conflict; parent contributes new keys.
	if got.Admin["a"] != 1.0 || got.Admin["b"] != 2.0 {
		t.Fatalf("merged admin = %v", got.Admin)
	}
}

func TestMergeStripsNonInheritable(t *testing.T) {
	tiers := []map[string]any{
		{"x": 1.0},
		{"permissions": map[string]any{"defaultMode": "bypassPermissions", "other": "keep"}},
	}
	got := MergeAdminTiers(tiers, MergeBehavior, false, 0, testDeps("linux"))
	if v, ok := GetPath(got.Admin, []string{"permissions", "defaultMode"}); ok {
		t.Fatalf("non-inheritable defaultMode leaked from lower tier: %v", v)
	}
	if v, ok := GetPath(got.Admin, []string{"permissions", "other"}); !ok || v != "keep" {
		t.Fatalf("sibling of stripped key lost = %v, %v", v, ok)
	}
}

func TestMergeSandboxPlatformScope(t *testing.T) {
	// enabledPlatforms names the current platform -> list cleared, sandbox kept.
	tiers := []map[string]any{
		{"x": 1.0},
		{"sandbox": map[string]any{"enabledPlatforms": []any{"linux"}, "mode": "strict"}},
	}
	got := MergeAdminTiers(tiers, MergeBehavior, false, 0, testDeps("linux"))
	if _, ok := GetPath(got.Admin, []string{"sandbox", "enabledPlatforms"}); ok {
		t.Fatal("enabledPlatforms should be cleared for the current platform")
	}
	if v, ok := GetPath(got.Admin, []string{"sandbox", "mode"}); !ok || v != "strict" {
		t.Fatalf("sandbox block should survive = %v, %v", v, ok)
	}

	// enabledPlatforms names only other platforms -> whole sandbox dropped.
	tiers = []map[string]any{
		{"x": 1.0},
		{"sandbox": map[string]any{"enabledPlatforms": []any{"windows"}, "mode": "strict"}},
	}
	got = MergeAdminTiers(tiers, MergeBehavior, false, 0, testDeps("linux"))
	if _, ok := got.Admin["sandbox"]; ok {
		t.Fatal("sandbox for other platforms should be dropped")
	}
}

func TestMergeEmpty(t *testing.T) {
	got := MergeAdminTiers(nil, MergeBehavior, false, 0, testDeps("linux"))
	if got.Admin != nil || got.Merged {
		t.Fatalf("empty tiers = %v", got)
	}
}
