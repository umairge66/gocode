package ids

import (
	"testing"
)

// --- name validators and UUIDs --------------------------------------------

func TestUUIDv5KnownVector(t *testing.T) {
	// RFC 4122 DNS namespace + "www.example.com".
	got := UUIDv5("www.example.com", "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	want := "2ed6657d-e927-568b-95e1-2665a8aea6a2"
	if got != want {
		t.Errorf("UUIDv5 = %q want %q", got, want)
	}
}

func TestValidateUUID(t *testing.T) {
	if _, ok := ValidateUUID("2ed6657d-e927-568b-95e1-2665a8aef6f0"); !ok {
		t.Error("expected valid UUID")
	}
	if _, ok := ValidateUUID("not-a-uuid"); ok {
		t.Error("expected invalid")
	}
}

func TestRandomUUIDShape(t *testing.T) {
	u := RandomUUID()
	if _, ok := ValidateUUID(u); !ok {
		t.Errorf("RandomUUID produced invalid UUID %q", u)
	}
	if u[14] != '4' {
		t.Errorf("RandomUUID not version 4: %q", u)
	}
}
