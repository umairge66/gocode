package envsec

import "testing"

func TestIsCredentialEnvVar(t *testing.T) {
	yes := []string{
		"AWS_SECRET_ACCESS_KEY", "SLACK_WEBHOOK_URL",
		"INPUT_AWS_SECRET_ACCESS_KEY", "INPUT_AWS_SESSION_TOKEN",
	}
	no := []string{
		"PATH", "HOME", "FOO_TOKEN_NOT_LISTED", "input_aws_session_token", // not upper-cased: exact match only
		"AWS_SESSION_TOKEN_SUFFIX",
	}
	for _, n := range yes {
		if !IsCredentialEnvVar(n) {
			t.Errorf("IsCredentialEnvVar(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsCredentialEnvVar(n) {
			t.Errorf("IsCredentialEnvVar(%q) = true, want false", n)
		}
	}
}
