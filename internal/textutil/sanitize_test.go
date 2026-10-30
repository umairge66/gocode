package textutil

import "testing"

// cp builds a string from the given Unicode code points. It lets the tests name
// non-ASCII and invisible characters by number rather than embedding them.
func cp(rs ...rune) string { return string(rs) }

func TestStripANSI(t *testing.T) {
	esc := cp(0x1b)
	if got := StripANSI(esc + "[31mred" + esc + "[0m"); got != "red" {
		t.Errorf("StripANSI = %q want red", got)
	}
	if got := StripANSI("plain"); got != "plain" {
		t.Errorf("StripANSI(plain) = %q", got)
	}
}
