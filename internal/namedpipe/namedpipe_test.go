package namedpipe

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	ok := []string{
		`\\.\pipe\cc-daemon-0123456789abcdef-control`,
		`\\.\pipe\gocode-chrome-host-1a2b3c4d`,
		`\\.\pipe\x`,
	}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{
		``,
		`\\.\pipe\`,
		`\\server\pipe\x`,                      // remote (SMB) pipe
		`\\?\pipe\x`,                           // not the \\.\pipe\ namespace
		`\\.\pipe\a\b`,                         // nested separators
		`\\.\pipe\a/b`,                         // forward slash
		`/tmp/cc-daemon/c.sock`,                // a unix path
		`\\.\pipe\` + strings.Repeat("a", 250), // > 256 chars
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", n)
		}
	}
}

func TestSecurityDescriptor(t *testing.T) {
	const sid = "S-1-5-21-1004336348-1177238915-682003330-1001"
	got, err := SecurityDescriptor(sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)"; got != want {
		t.Fatalf("SDDL = %q, want %q", got, want)
	}
	got, err = SecurityDescriptor(sid, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "D:P(A;;GA;;;" + sid + ")"; got != want {
		t.Fatalf("SDDL without SYSTEM = %q, want %q", got, want)
	}
	// The DACL must be protected and must not grant anyone else.
	for _, forbidden := range []string{"WD", "AN", "BA", "AU", "IU", "NU"} {
		if strings.Contains(got, ";;;"+forbidden+")") {
			t.Errorf("SDDL %q grants %s", got, forbidden)
		}
	}
	if !strings.HasPrefix(got, "D:P") {
		t.Errorf("SDDL %q is not a protected DACL", got)
	}
}

func TestSecurityDescriptorRejectsNonSID(t *testing.T) {
	for _, s := range []string{
		"",
		"WD",
		"S-1-5-21-1)(A;;GA;;;WD", // SDDL injection attempt
		"S-1",
		"s-1-5-18",
	} {
		if _, err := SecurityDescriptor(s, true); err == nil {
			t.Errorf("SecurityDescriptor(%q) accepted a non-SID", s)
		}
	}
}

func TestAddr(t *testing.T) {
	a := Addr(`\\.\pipe\x`)
	if a.Network() != "pipe" || a.String() != `\\.\pipe\x` {
		t.Fatalf("Addr = %q/%q", a.Network(), a.String())
	}
}
