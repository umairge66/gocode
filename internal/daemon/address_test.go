package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"
)

// sha256Hex8 reproduces the discriminator derivation independently so the
// known-vector test pins the exact algorithm rather than restating it.
func sha256Hex8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// TestInstanceDiscriminatorDeterministic asserts the discriminator is stable
// for the same config dir and differs across dirs/users.
func TestInstanceDiscriminatorDeterministic(t *testing.T) {
	const dirA = "/home/alice/.config/gocode"
	const dirB = "/home/bob/.config/gocode"

	a1 := InstanceDiscriminator(dirA)
	a2 := InstanceDiscriminator(dirA)
	if a1 != a2 {
		t.Fatalf("not deterministic: %q != %q", a1, a2)
	}
	if len(a1) != 8 {
		t.Fatalf("discriminator length = %d, want 8", len(a1))
	}
	for _, c := range a1 {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("discriminator %q is not lowercase hex", a1)
		}
	}
	if b := InstanceDiscriminator(dirB); b == a1 {
		t.Fatalf("different config dirs produced the same discriminator %q", a1)
	}
}

// TestInstanceDiscriminatorKnownVector pins the exact derivation
// (sha256(resolve(dir))[:8]) so a future refactor cannot silently change the
// socket address and strand existing daemons.
func TestInstanceDiscriminatorKnownVector(t *testing.T) {
	// resolveDir leaves an absolute, already-clean path unchanged, so this is
	// sha256(dir)[:8].
	dir := "/tmp/x"
	if runtime.GOOS == "windows" {
		dir = `C:\tmp\x`
	}
	got := InstanceDiscriminator(dir)
	if want := sha256Hex8(dir); got != want {
		t.Fatalf("discriminator = %q, want %q", got, want)
	}
}

// TestControlPipeName checks the Windows pipe-name pattern.
func TestControlPipeName(t *testing.T) {
	got := ControlPipeName("0123456789abcdef")
	want := `\\.\pipe\cc-daemon-0123456789abcdef-control`
	if got != want {
		t.Fatalf("pipe name = %q, want %q", got, want)
	}
}

// TestControlAddressDeterministic asserts the shared address helper (used by
// both ListenControl/server and DialControl/NewControlClient) returns the same
// endpoint for the same config dir on repeated calls.
func TestControlAddressDeterministic(t *testing.T) {
	cfg := t.TempDir()
	ep1, err := ControlAddress(cfg)
	if err != nil {
		t.Fatalf("ControlAddress: %v", err)
	}
	ep2, err := ControlAddress(cfg)
	if err != nil {
		t.Fatalf("ControlAddress: %v", err)
	}
	if ep1 != ep2 {
		t.Fatalf("ControlAddress not deterministic: %+v != %+v", ep1, ep2)
	}
}
