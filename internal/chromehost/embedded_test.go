package chromehost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/extension"
)

func TestEmbeddedExtensionIdentity(t *testing.T) {
	id, err := EmbeddedExtensionID()
	if err != nil {
		t.Fatal(err)
	}
	if id != testExtensionID {
		t.Errorf("embedded id = %q, want %q", id, testExtensionID)
	}
	if EmbeddedExtensionVersion() == "" {
		t.Error("embedded version is empty")
	}
	if StoreExtensionID != "" && !ValidExtensionID(StoreExtensionID) {
		t.Errorf("StoreExtensionID %q is not a valid extension id", StoreExtensionID)
	}
}

func TestTrustedExtensionIDs(t *testing.T) {
	const store = "abcdefghijklmnopabcdefghijklmnop"
	const extra = "ponmlkjihgfedcbaponmlkjihgfedcba"
	env := map[string]string{EnvExtensionIDs: " " + extra + ", " + store + " "}
	ids, err := TrustedExtensionIDs(testExtensionID, store, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{testExtensionID, store, extra}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}

	ids, err = TrustedExtensionIDs(testExtensionID, "", nil)
	if err != nil || len(ids) != 1 || ids[0] != testExtensionID {
		t.Errorf("no store id: ids = %v, err = %v", ids, err)
	}

	env[EnvExtensionIDs] = "*"
	if _, err := TrustedExtensionIDs(testExtensionID, "", func(k string) string { return env[k] }); err == nil {
		t.Error("a wildcard id from the environment was accepted")
	}
	if _, err := TrustedExtensionIDs(testExtensionID, "not-an-id", nil); err == nil {
		t.Error("a malformed store id was accepted")
	}
}

func TestWriteExtension_FreshAndUpgrade(t *testing.T) {
	root := t.TempDir()
	dir := ExtensionDir(root)

	if _, err := InstalledExtensionVersion(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version before write: err = %v, want not-exist", err)
	}
	if err := WriteExtension(dir); err != nil {
		t.Fatal(err)
	}
	checkWritten(t, dir)

	// Simulate an older copy with an extra stale file, then upgrade.
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := InstalledExtensionVersion(dir); v != "0.0.1" {
		t.Fatalf("stale version = %q", v)
	}
	if err := WriteExtension(dir); err != nil {
		t.Fatal(err)
	}
	checkWritten(t, dir)
	if _, err := os.Stat(filepath.Join(dir, "stale.js")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file from the older copy survived the upgrade")
	}

	// Nothing but the extension directory is left in the data root.
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != ExtensionDirName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("data root holds %v, want only %s", names, ExtensionDirName)
	}
}

func checkWritten(t *testing.T, dir string) {
	t.Helper()
	v, err := InstalledExtensionVersion(dir)
	if err != nil || v != EmbeddedExtensionVersion() {
		t.Fatalf("installed version = %q, %v; want %q", v, err, EmbeddedExtensionVersion())
	}
	for _, name := range extension.RuntimeFiles {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := extension.ReadFile(name)
		if string(got) != string(want) {
			t.Errorf("%s differs from the embedded file", name)
		}
		if runtime.GOOS != "windows" {
			fi, _ := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", name, fi.Mode().Perm())
			}
		}
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(dir)
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.3.0", 0},
		{"0.3", "0.3.0", 0},
		{"0.2.9", "0.3.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1", "0.99", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestExtensionVersionNotice(t *testing.T) {
	if n := ExtensionVersionNotice("0.3.0", "0.3.0"); n != "" {
		t.Errorf("same version: %q", n)
	}
	if n := ExtensionVersionNotice("", "0.3.0"); n != "" {
		t.Errorf("unknown version: %q", n)
	}
	if n := ExtensionVersionNotice("0.2.0", "0.3.0"); !strings.Contains(n, "gocode chrome install") || !strings.Contains(n, "Reload") {
		t.Errorf("older extension notice = %q", n)
	}
	if n := ExtensionVersionNotice("0.4.0", "0.3.0"); !strings.Contains(n, "Update gocode") {
		t.Errorf("newer extension notice = %q", n)
	}
}

func TestExtensionVersionCheck_NoticeOnce(t *testing.T) {
	calls := 0
	c := &ExtensionVersionCheck{
		Expected: "0.3.0",
		Call: func(_ context.Context, method string, _ any) (json.RawMessage, error) {
			calls++
			if method != MethodHostInfo {
				t.Errorf("method = %q", method)
			}
			return json.RawMessage(`{"extensionVersion":"0.2.0"}`), nil
		},
	}
	if n := c.Notice(context.Background()); n == "" {
		t.Fatal("no notice for an older extension")
	}
	if n := c.Notice(context.Background()); n != "" {
		t.Errorf("notice repeated: %q", n)
	}
	if calls != 1 {
		t.Errorf("host asked %d times, want 1", calls)
	}
}

func TestExtensionVersionCheck_GivesUpOnOlderHost(t *testing.T) {
	calls := 0
	c := &ExtensionVersionCheck{
		Expected: "0.3.0",
		Call: func(context.Context, string, any) (json.RawMessage, error) {
			calls++
			return nil, errors.New("unknown method")
		},
	}
	for i := 0; i < 10; i++ {
		if n := c.Notice(context.Background()); n != "" {
			t.Fatalf("notice without an answer: %q", n)
		}
	}
	if calls != maxVersionCheckAttempts {
		t.Errorf("host asked %d times, want %d", calls, maxVersionCheckAttempts)
	}
}
