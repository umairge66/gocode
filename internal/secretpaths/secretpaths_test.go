package secretpaths

import (
	"reflect"
	"testing"
)

func TestStripSecretsMasksCredentialPathsLeavesOthers(t *testing.T) {
	in := map[string]any{
		// Secret/non-inheritable keys that must be stripped.
		"apiKeyHelper":           "echo $KEY",
		"awsCredentialExport":    "aws-export",
		"forceLoginMethod":       "console",
		"env":                    map[string]any{"ANTHROPIC_API_KEY": "sk-secret"},
		"modelPicker":            map[string]any{"replaceBuiltInOptions": true},
		"policyHelper":           "helper",
		"managedSourcesBehavior": "merge",
		// Ordinary settings that must survive untouched.
		"model":       "claude-opus",
		"permissions": map[string]any{"defaultMode": "ask"},
	}

	out := StripSecrets(in)

	for _, k := range []string{
		"apiKeyHelper", "awsCredentialExport", "forceLoginMethod", "env",
		"modelPicker", "policyHelper", "managedSourcesBehavior",
	} {
		if _, ok := out[k]; ok {
			t.Errorf("secret key %q should have been stripped, but is present", k)
		}
	}

	if out["model"] != "claude-opus" {
		t.Errorf("non-secret key model = %v, want claude-opus", out["model"])
	}
	perms, ok := out["permissions"].(map[string]any)
	if !ok || perms["defaultMode"] != "ask" {
		t.Errorf("non-secret nested key permissions.defaultMode not preserved: %#v", out["permissions"])
	}
}

func TestStripSecretsDoesNotMutateInputAndClonesDeeply(t *testing.T) {
	nested := map[string]any{"defaultMode": "ask"}
	in := map[string]any{
		"apiKeyHelper": "x",
		"permissions":  nested,
	}

	out := StripSecrets(in)

	// Input is untouched (secret key still present in the original).
	if _, ok := in["apiKeyHelper"]; !ok {
		t.Fatalf("StripSecrets mutated its input: apiKeyHelper was removed")
	}
	// Retained nested values are deep-cloned, not shared by reference.
	outPerms := out["permissions"].(map[string]any)
	outPerms["defaultMode"] = "bypassPermissions"
	if nested["defaultMode"] != "ask" {
		t.Errorf("mutating the result leaked into the input: nested=%#v", nested)
	}
}

func TestStripSecretsNil(t *testing.T) {
	out := StripSecrets(nil)
	if out == nil {
		t.Fatal("StripSecrets(nil) returned nil, want empty map for nil input")
	}
	if len(out) != 0 {
		t.Errorf("StripSecrets(nil) = %#v, want empty map", out)
	}
}

func TestSecretPathsRegistryExact(t *testing.T) {
	// Guards the SecretPaths list (order and contents) against drift.
	want := []string{
		"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh",
		"otelHeadersHelper", "proxyAuthHelper",
		"forceLoginOrgUUID", "forceLoginMethod", "forceLoginGatewayUrl",
		"gatewayInternalNetworks",
		"parentSettingsBehavior", "env", "modelPicker",
		"policyHelper", "policyHelpers",
		"managedSourcesBehavior", "wslInheritsWindowsSettings",
	}
	if !reflect.DeepEqual(SecretPaths, want) {
		t.Errorf("SecretPaths registry drifted:\n got = %#v\nwant = %#v", SecretPaths, want)
	}
}
