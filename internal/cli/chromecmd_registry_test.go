package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRegistry is an in-memory nativeHostRegistry for exercising the Windows
// install/uninstall/status paths on any OS.
type fakeRegistry struct {
	values  map[string]string
	failSet map[string]bool
	failDel map[string]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{values: map[string]string{}, failSet: map[string]bool{}, failDel: map[string]bool{}}
}

func (r *fakeRegistry) SetDefault(key, value string) error {
	if r.failSet[key] {
		return errors.New("access is denied")
	}
	r.values[key] = value
	return nil
}

func (r *fakeRegistry) GetDefault(key string) (string, error) {
	v, ok := r.values[key]
	if !ok {
		return "", errRegistryKeyAbsent
	}
	return v, nil
}

func (r *fakeRegistry) DeleteKey(key string) error {
	if r.failDel[key] {
		return errors.New("access is denied")
	}
	if _, ok := r.values[key]; !ok {
		return errRegistryKeyAbsent
	}
	delete(r.values, key)
	return nil
}

func newWindowsTestInstaller(t *testing.T) (*chromeInstaller, *fakeRegistry, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	hostDir := t.TempDir()
	ci, out, errb := newTestInstaller(hostDir, `C:\Program Files\gocode\gocode.exe`)
	ci.goos = "windows"
	reg := newFakeRegistry()
	ci.registry = reg
	return ci, reg, out, errb, filepath.Join(hostDir, nativeHostName+".json")
}

// The registry keys are the per-browser HKCU keys plus the host name.
func TestWindowsNativeHostRegistryKeys(t *testing.T) {
	want := map[string]string{
		"chrome":   `HKCU\Software\Google\Chrome\NativeMessagingHosts\com.gocode.gocode`,
		"brave":    `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\com.gocode.gocode`,
		"arc":      `HKCU\Software\ArcBrowser\Arc\NativeMessagingHosts\com.gocode.gocode`,
		"edge":     `HKCU\Software\Microsoft\Edge\NativeMessagingHosts\com.gocode.gocode`,
		"chromium": `HKCU\Software\Chromium\NativeMessagingHosts\com.gocode.gocode`,
		"vivaldi":  `HKCU\Software\Vivaldi\NativeMessagingHosts\com.gocode.gocode`,
		"opera":    `HKCU\Software\Opera Software\Opera Stable\NativeMessagingHosts\com.gocode.gocode`,
	}
	if len(windowsNativeHostKeys) != len(want) {
		t.Fatalf("%d browser keys, want %d", len(windowsNativeHostKeys), len(want))
	}
	for b, k := range want {
		if got := windowsRegistryKey(b); got != k {
			t.Errorf("windowsRegistryKey(%s) = %q, want %q", b, got, k)
		}
	}
	// Chrome channels and unknown names fall back to Google Chrome's key.
	for _, b := range []string{"chrome-beta", "chrome-canary", "bogus", ""} {
		if got := windowsRegistryKey(b); got != want["chrome"] {
			t.Errorf("windowsRegistryKey(%q) = %q, want Chrome's key", b, got)
		}
	}
}

func TestChromeInstallWindowsWritesRegistry(t *testing.T) {
	ci, reg, out, errb, manifest := newWindowsTestInstaller(t)
	extDir := writeFixtureExtension(t)
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	for _, bk := range windowsNativeHostKeys {
		key := bk.key + `\` + nativeHostName
		if got := reg.values[key]; got != manifest {
			t.Errorf("%s default value = %q, want the manifest path %q", key, got, manifest)
		}
	}
	if strings.Contains(out.String(), "reg add") {
		t.Errorf("install printed the manual reg add fallback although every write succeeded:\n%s", out.String())
	}

	out.Reset()
	if code := ci.status(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("status = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "registry:") {
		t.Errorf("status did not report the registry key:\n%s", out.String())
	}
}

// A failed write for the selected browser fails install and prints the reg add
// fallback for exactly the failed key.
func TestChromeInstallWindowsRegistryFailureFallsBack(t *testing.T) {
	ci, reg, out, _, manifest := newWindowsTestInstaller(t)
	chromeKey := windowsRegistryKey("chrome")
	reg.failSet[chromeKey] = true
	extDir := writeFixtureExtension(t)
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 1 {
		t.Fatalf("install with the selected key failing = %d, want 1", code)
	}
	wantHint := `reg add "` + chromeKey + `" /ve /t REG_SZ /d "` + manifest + `" /f`
	if !strings.Contains(out.String(), wantHint) {
		t.Errorf("missing fallback hint %q in:\n%s", wantHint, out.String())
	}
	if strings.Contains(out.String(), `reg add "`+windowsRegistryKey("edge")) {
		t.Errorf("fallback printed for a key that was written:\n%s", out.String())
	}
}

// A failure for a browser other than the selected one is reported but does
// not fail install.
func TestChromeInstallWindowsOtherBrowserFailureIsNonFatal(t *testing.T) {
	ci, reg, out, _, _ := newWindowsTestInstaller(t)
	reg.failSet[windowsRegistryKey("opera")] = true
	extDir := writeFixtureExtension(t)
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, want 0", code)
	}
	if !strings.Contains(out.String(), `reg add "`+windowsRegistryKey("opera")) {
		t.Errorf("no fallback hint for the failed opera key:\n%s", out.String())
	}
}

func TestChromeUninstallWindowsDeletesRegistry(t *testing.T) {
	ci, reg, out, errb, _ := newWindowsTestInstaller(t)
	extDir := writeFixtureExtension(t)
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	out.Reset()
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("uninstall = %d, stderr=%q", code, errb.String())
	}
	if len(reg.values) != 0 {
		t.Errorf("registry keys left after uninstall: %v", reg.values)
	}
	// Status now reports not installed.
	if code := ci.status(chromeFlags{browser: "chrome"}); code != 1 {
		t.Errorf("status after uninstall = %d, want 1", code)
	}
	// Uninstalling again is a clean no-op.
	out.Reset()
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Errorf("second uninstall = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "No Windows registry entries to remove") {
		t.Errorf("second uninstall output:\n%s", out.String())
	}
}

// Registry entries are removed even when the manifest file is already gone,
// and a delete failure prints the reg delete fallback and exits 1.
func TestChromeUninstallWindowsWithoutManifest(t *testing.T) {
	ci, reg, out, _, _ := newWindowsTestInstaller(t)
	chromeKey := windowsRegistryKey("chrome")
	edgeKey := windowsRegistryKey("edge")
	reg.values[chromeKey] = `C:\old\com.gocode.gocode.json`
	reg.values[edgeKey] = `C:\old\com.gocode.gocode.json`
	reg.failDel[edgeKey] = true
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 1 {
		t.Fatalf("uninstall with a failing delete = %d, want 1", code)
	}
	if _, ok := reg.values[chromeKey]; ok {
		t.Error("chrome key not removed when the manifest was already gone")
	}
	if !strings.Contains(out.String(), `reg delete "`+edgeKey+`" /f`) {
		t.Errorf("missing reg delete fallback:\n%s", out.String())
	}
}

// Status flags a registry key that points somewhere else.
func TestChromeStatusWindowsStaleRegistry(t *testing.T) {
	ci, reg, out, errb, _ := newWindowsTestInstaller(t)
	extDir := writeFixtureExtension(t)
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	reg.values[windowsRegistryKey("chrome")] = `C:\elsewhere\host.json`
	out.Reset()
	if code := ci.status(chromeFlags{browser: "chrome"}); code != 1 {
		t.Fatalf("status with a stale key = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "not this manifest") {
		t.Errorf("status output:\n%s", out.String())
	}
}
