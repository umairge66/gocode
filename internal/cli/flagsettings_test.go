package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A --settings file and an inline --settings JSON object both feed the
// effective settings every reader uses.
func TestFlagSettingsReachSettingsReaders(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	vars := map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config")}
	newEnv := func(flag string) *Env {
		return &Env{
			Getenv:       func(k string) string { return vars[k] },
			Cwd:          func() (string, error) { return cwd, nil },
			FlagSettings: flag,
		}
	}

	if !respondToShellCommands(newEnv("")) {
		t.Fatal("respondToBashCommands should default to true")
	}

	file := filepath.Join(cwd, "flag.json")
	if err := os.WriteFile(file, []byte(`{"respondToBashCommands": false, "ui": {"theme": "prompty-light"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, flag := range map[string]string{
		"absolute path": file,
		"relative path": "flag.json",
	} {
		env := newEnv(flag)
		if respondToShellCommands(env) {
			t.Errorf("%s: respondToBashCommands from --settings was ignored", name)
		}
		if got := resolveUISettings(env).theme; got != "prompty-light" {
			t.Errorf("%s: ui.theme = %q, want prompty-light", name, got)
		}
	}

	inline := newEnv(`{"respondToBashCommands": false}`)
	if respondToShellCommands(inline) {
		t.Error("inline --settings JSON was ignored")
	}
}
