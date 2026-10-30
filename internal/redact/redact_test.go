package redact

import (
	"strings"
	"testing"
)

// The sample tokens below are fake. Some are split into concatenated parts so
// the source never contains a token-shaped literal that secret scanners flag.
func TestRedactSimpleSecrets(t *testing.T) {
	cases := []struct {
		name, in, secret string
	}{
		{"github-pat", "token=ghp_0123456789abcdefghijklmnopqrstuvwxyz;", "ghp_0123456789abcdefghijklmnopqrstuvwxyz"},
		{"github-oauth", "gho_0123456789abcdefghijklmnopqrstuvwxyz", "gho_0123456789abcdefghijklmnopqrstuvwxyz"},
		{"aws", "id=AKIAIOSFODNN7EXAMPLE end", "AKIAIOSFODNN7EXAMPLE"},
		{"openai-legacy", "sk-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRST12", "sk-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRST12"},
		{"gitlab-pat", "glpat-abcdefghij1234567890XY", "glpat-abcdefghij1234567890XY"},
		{"slack-bot", "xox" + "b-1234567890-0987654321abcXYZ", "xox" + "b-1234567890-0987654321abcXYZ"},
		{"stripe", "sk_live_abcdef0123456789ABCDEF;", "sk_live_abcdef0123456789ABCDEF"},
		{"shopify", "shp" + "at_0123456789abcdef0123456789abcdef", "shp" + "at_0123456789abcdef0123456789abcdef"},
		{"npm", "npm_abcdefghijklmnopqrstuvwxyz0123456789 ", "npm_abcdefghijklmnopqrstuvwxyz0123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if strings.Contains(got, c.secret) {
				t.Fatalf("secret not redacted: input %q -> %q", c.in, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("no placeholder in output: %q", got)
			}
		})
	}
}

func TestGCPKeyTrailingBoundary(t *testing.T) {
	// 35 chars after AIza, followed by a space -> redacted.
	ok := "AIza" + strings.Repeat("a", 35)
	if got := Redact(ok + " tail"); strings.Contains(got, ok) {
		t.Fatalf("gcp key not redacted: %q", got)
	}
	// 36 word chars after AIza -> trailing (?![\w-]) fails, must NOT redact.
	longer := "AIza" + strings.Repeat("a", 36)
	if got := Redact(longer); !strings.Contains(got, longer) {
		t.Fatalf("over-redacted past trailing boundary: %q", got)
	}
}

func TestRedactPrivateKey(t *testing.T) {
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEpQIBAAKCAQEA", 8) + "\n-----END RSA PRIVATE KEY-----\nafter"
	got := Redact(in)
	if strings.Contains(got, "MIIEpQIBAAKCAQEA") {
		t.Fatalf("private key body not redacted: %q", got)
	}
	if !strings.HasPrefix(got, "before") || !strings.HasSuffix(got, "after") {
		t.Fatalf("surrounding text not preserved: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Free-text scanner and URL sanitizers.
// ---------------------------------------------------------------------------

func TestContainsSecretBearerURL(t *testing.T) {
	positives := []string{
		"https://user:pass@host.example.com/path",
		"ftp://admin:s3cr3t@files.internal/",
		"//user:password123@registry.example.com",
		"Authorization: Bearer abcdef0123456789GHIJ",
		"export GITHUB_TOKEN=ghp_abcdefghijklmnop",
		"https://hooks.slack.com/services/T000/B000/XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX",
	}
	for _, p := range positives {
		if !ContainsSecret(p) {
			t.Errorf("ContainsSecret(%q) = false, want true", p)
		}
	}
	negatives := []string{
		"https://example.com/path?q=1",
		"just some plain text",
		"git@github.com:org/repo.git",
		"host.example.com:8080/path",
	}
	for _, n := range negatives {
		if ContainsSecret(n) {
			t.Errorf("ContainsSecret(%q) = true, want false", n)
		}
	}
}

// ---------------------------------------------------------------------------
// Unicode normalization and grapheme behavior.
// ---------------------------------------------------------------------------

func TestStripInvisible(t *testing.T) {
	if got := StripInvisible(string([]rune{'a', 0x200B, 'b', 0xFEFF, 'c'})); got != "abc" {
		t.Errorf("StripInvisible = %q, want abc", got)
	}
}
