package ui

import (
	"strings"
	"testing"
)

func TestShellsPill(t *testing.T) {
	for n, want := range map[int]string{0: "", 1: "1 shell", 3: "3 shells"} {
		if got := (Footer{BackgroundShells: n}).ShellsPill(); got != want {
			t.Errorf("ShellsPill(%d) = %q, want %q", n, got, want)
		}
	}
	f := Footer{BackgroundShells: 2, TokensIn: 1200, TokensOut: 30}
	if got := renderFooter(f, 80); !strings.Contains(got, "2 shells · ") || !strings.Contains(got, f.TokenReadout()) {
		t.Errorf("footer = %q", got)
	}
	if got := renderFooter(Footer{BackgroundShells: 1}, 40); strings.TrimSpace(got) != "1 shell" {
		t.Errorf("footer without tokens = %q", got)
	}
	vs := Reduce(ViewState{}, SetBackgroundShells{Count: 4})
	if vs.Footer.BackgroundShells != 4 {
		t.Errorf("reducer kept %d shells", vs.Footer.BackgroundShells)
	}
}
