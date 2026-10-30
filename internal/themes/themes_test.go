package themes

import (
	"testing"
)

func TestLookupPaletteDarkLight(t *testing.T) {
	// dark theme: text is white (rgb(255,255,255)), error rgb(255,107,128).
	dark := LookupPalette(ThemeDark)
	if got := dark["text"]; got != "rgb(255,255,255)" {
		t.Errorf("dark text = %q, want rgb(255,255,255)", got)
	}
	if got := dark["error"]; got != "rgb(255,107,128)" {
		t.Errorf("dark error = %q, want rgb(255,107,128)", got)
	}

	// light theme: text is black, success rgb(44,122,57).
	light := LookupPalette(ThemeLight)
	if got := light["text"]; got != "rgb(0,0,0)" {
		t.Errorf("light text = %q, want rgb(0,0,0)", got)
	}
	if got := light["success"]; got != "rgb(44,122,57)" {
		t.Errorf("light success = %q, want rgb(44,122,57)", got)
	}

	// ANSI themes use ansi: values.
	if got := LookupPalette(ThemeDarkANSI)["error"]; got != "ansi:redBright" {
		t.Errorf("dark-ansi error = %q, want ansi:redBright", got)
	}
	if got := LookupPalette(ThemeLightANSI)["error"]; got != "ansi:red" {
		t.Errorf("light-ansi error = %q, want ansi:red", got)
	}
}

func TestLookupPaletteUnknownFallsBackToDark(t *testing.T) {
	// Unknown name, "auto" and "custom:" sentinels all resolve to dark.
	for _, name := range []string{"", "auto", "nope", "custom:mine"} {
		p := LookupPalette(name)
		if p["text"] != defaultDarkPalette["text"] {
			t.Errorf("LookupPalette(%q) did not fall back to dark palette", name)
		}
	}
	// Daltonized themes are distinct from the default.
	if LookupPalette(ThemeDarkDaltonized)["diffAdded"] == defaultDarkPalette["diffAdded"] {
		t.Errorf("dark-daltonized should differ from dark for diffAdded")
	}
}

func TestAllPalettesShareTokenSet(t *testing.T) {
	base := defaultDarkPalette
	for _, name := range ThemeNames {
		p := LookupPalette(name)
		if len(p) != len(base) {
			t.Errorf("palette %q has %d tokens, want %d", name, len(p), len(base))
		}
		for token := range base {
			if _, ok := p[token]; !ok {
				t.Errorf("palette %q missing token %q", name, token)
			}
		}
	}
}

func TestThemePredicates(t *testing.T) {
	if !IsLight(ThemeLight) || !IsLight(ThemeLightANSI) || IsLight(ThemeDark) {
		t.Errorf("IsLight misclassified a theme")
	}
}

func TestIsValidColorValue(t *testing.T) {
	good := []string{"rgb(0,0,0)", "rgb(255, 255, 255)", "#abcdef", "#abc", "ansi256(12)", "ansi:red", "ansi:blueBright"}
	for _, v := range good {
		if !IsValidColorValue(v) {
			t.Errorf("IsValidColorValue(%q) = false, want true", v)
		}
	}
	bad := []string{"", "red", "ansi:notacolor", "#xyz", "rgb(1,2)", "ansi256()"}
	for _, v := range bad {
		if IsValidColorValue(v) {
			t.Errorf("IsValidColorValue(%q) = true, want false", v)
		}
	}
}

func TestApplyOverrides(t *testing.T) {
	base := LookupPalette(ThemeDark)
	out := ApplyOverrides(base, map[string]string{
		"error":        "rgb(1,2,3)", // valid + known -> applied
		"unknownToken": "rgb(4,5,6)", // unknown -> ignored
		"success":      "notacolor",  // invalid value -> ignored
	})
	if out["error"] != "rgb(1,2,3)" {
		t.Errorf("override not applied: error = %q", out["error"])
	}
	if _, ok := out["unknownToken"]; ok {
		t.Errorf("unknown token leaked into palette")
	}
	if out["success"] != base["success"] {
		t.Errorf("invalid override should not change success")
	}
	// base must be untouched (copy semantics).
	if base["error"] == "rgb(1,2,3)" {
		t.Errorf("ApplyOverrides mutated the base palette")
	}
}
