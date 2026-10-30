package envsec

import "testing"

// TestRedactScannerSatisfiesInterface verifies the wired scanner is usable
// through the SecretScanner interface.
func TestRedactScannerSatisfiesInterface(t *testing.T) {
	var s SecretScanner = RedactScanner{}
	if !s.ContainsSecret("https://ghp_0123456789abcdefghijklmnopqrstuvwx@github.com/o/r.git") {
		t.Fatal("SecretScanner.ContainsSecret should detect a ghp_ token in a URL")
	}
}

// TestContainsSecretGitHubToken checks a ghp_...-style GitHub token is detected
// through the redact-backed implementation.
func TestContainsSecretGitHubToken(t *testing.T) {
	token := "ghp_0123456789abcdefghijklmnopqrstuvwx"
	if !DefaultScanner.ContainsSecret("GITHUB_TOKEN=" + token) {
		t.Errorf("expected ghp_ token to be flagged as a secret")
	}
	if !ContainsSecret("https://x-access-token:" + token + "@github.com/o/r.git") {
		t.Errorf("expected userinfo-embedded ghp_ token to be flagged as a secret")
	}
	if ContainsSecret("just some ordinary log line without credentials") {
		t.Errorf("did not expect plain text to be flagged as a secret")
	}
}
