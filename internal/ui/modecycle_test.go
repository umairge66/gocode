package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestModeCycleNextIn(t *testing.T) {
	walk := func(c ModeCycle) []PermissionMode {
		var out []PermissionMode
		m := ModeDefault
		for i := 0; i < 6; i++ {
			out = append(out, m)
			m = m.NextIn(c)
			if m == ModeDefault {
				break
			}
		}
		return out
	}
	cases := []struct {
		c    ModeCycle
		want []PermissionMode
	}{
		{ModeCycle{}, []PermissionMode{ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto}},
		{ModeCycle{NoAuto: true}, []PermissionMode{ModeDefault, ModeAcceptEdits, ModePlan}},
		{ModeCycle{Bypass: true}, []PermissionMode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass, ModeAuto}},
		{ModeCycle{Bypass: true, NoAuto: true}, []PermissionMode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass}},
	}
	for _, tc := range cases {
		got := walk(tc.c)
		if len(got) != len(tc.want) {
			t.Fatalf("%+v: cycle %v, want %v", tc.c, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%+v: cycle %v, want %v", tc.c, got, tc.want)
			}
		}
	}
	if m := PermissionMode("dontAsk").NextIn(ModeCycle{}); m != ModeDefault {
		t.Fatalf("dontAsk -> %q", m)
	}
}

func TestShiftTabFollowsModeCycle(t *testing.T) {
	m := NewModel(80)
	var reported []PermissionMode
	m.OnCycleMode = func(p PermissionMode) { reported = append(reported, p) }
	m.Update(SetModeCycle{Cycle: ModeCycle{NoAuto: true}})
	m.Update(SetPermissionMode{Mode: ModePlan})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if len(reported) != 1 || reported[0] != ModeDefault {
		t.Fatalf("plan with auto unavailable cycled to %v", reported)
	}
	m.Update(SetModeCycle{Cycle: ModeCycle{}})
	m.Update(SetPermissionMode{Mode: ModePlan})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if reported[len(reported)-1] != ModeAuto {
		t.Fatalf("plan cycled to %v", reported)
	}
}
