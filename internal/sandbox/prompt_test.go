package sandbox

import (
	"strings"
	"testing"
)

func TestPromptSection(t *testing.T) {
	if s := fakeManager("linux", true).PromptSection(false); s != "" {
		t.Fatalf("sandbox off: no section, got %q", s)
	}
	m := fakeManager("linux", true, layer("userSettings", sb(map[string]any{
		"enabled": true,
		"network": map[string]any{"allowedDomains": []any{"github.com"}, "deniedDomains": []any{"evil.com"}},
	})))
	s := m.PromptSection(true)
	for _, want := range []string{
		"## Bash command sandbox\nBy default, Bash commands run inside an OS-level sandbox (Linux bubblewrap) applied to each command separately",
		"How the sandbox is configured in this session:\nFilesystem: {\"read\":{\"denyOnly\":[",
		`"write":{"allowOnly":["/proj","$TMPDIR"]`,
		"Network: {\"deniedHosts\":[\"evil.com\"]}",
		" - You should always default to running commands within the sandbox.",
		"  - The user *explicitly* asks you to bypass sandbox",
		" - For temporary files, always use the `$TMPDIR` environment variable.",
		" - If a clipboard utility",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("section misses %q\n%s", want, s)
		}
	}
	strict := fakeManager("linux", true, layer("userSettings", sb(map[string]any{"enabled": true, "allowUnsandboxedCommands": false})))
	if s := strict.PromptSection(false); !strings.Contains(s, "The `dangerouslyDisableSandbox` parameter is disabled in this session's configuration") || strings.Contains(s, "clipboard") {
		t.Errorf("strict section:\n%s", s)
	}
}
