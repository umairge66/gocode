package pathclass

import "testing"

func TestWinNormalizeApproxDots(t *testing.T) {
	cases := map[string]string{
		`/x/../??/y`:  `\??\y`,
		`/./??/y`:     `\??\y`,
		`\a\.\b\..\c`: `\a\c`,
		`/..`:         `\`,
		`a/../../b/`:  `..\b\`,
		`C:/a/../b`:   `C:\b`,
		`C:a/..`:      `C:.`,
		`//srv//sh`:   `\\srv\sh`,
		``:            `.`,
	}
	for in, want := range cases {
		if got := winNormalizeApprox(in); got != want {
			t.Errorf("winNormalizeApprox(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNTDevicePathAfterDotResolution(t *testing.T) {
	for _, p := range []string{`\??\C:\x`, `/x/../??/C:/y`, `/./??/z`} {
		if !IsNTDevicePath(p) {
			t.Errorf("%q should be an NT device path", p)
		}
	}
	for _, p := range []string{`x/../??/y`, `/a??b/c`, `C:/??/x`} {
		if IsNTDevicePath(p) {
			t.Errorf("%q should not be an NT device path", p)
		}
	}
}
