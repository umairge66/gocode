package redact

import (
	"strings"
	"testing"
)

// TestRedact_RegistryProviderKeys: the recognisable key formats of the
// registry's model providers are redacted (fake keys, built at run time).
func TestRedact_RegistryProviderKeys(t *testing.T) {
	body := strings.Repeat("Ab3", 20)
	for _, key := range []string{
		"xai-" + body,
		"gsk_" + body,
		"csk-" + strings.ToLower(body),
		"avian-" + body,
		"hf_" + strings.Repeat("abcdefgh", 4) + "XY",
		"AIza" + strings.Repeat("x", 35),
	} {
		in := "export KEY=" + key + " # tail"
		out := Redact(in)
		if strings.Contains(out, key) || !strings.Contains(out, Placeholder) {
			t.Errorf("%q not redacted: %q", key[:6], out)
		}
		if !strings.HasSuffix(out, " # tail") {
			t.Errorf("surrounding text damaged: %q", out)
		}
	}
	// Ordinary identifiers with the same prefixes stay.
	for _, s := range []string{"xai-sdk", "gsk_test", "csk-config", "avian-docs"} {
		if Redact(s) != s {
			t.Errorf("%q falsely redacted", s)
		}
	}
}
