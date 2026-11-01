package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/chromehost"
)

// The committed manifest key -> id vector (same as chromehost's test vector).
const (
	wantManifestKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAweaAPDU0d7iUk3uB1Ba555IxVIlvoe3ElgwfeCS0BBDJvwGPshOSUJ5vmA3W8TlK+EKEQytMerIIwFRWp5ofBkChnhVxj1sLRg1gtBAa9BExiRgMxJlK/Y4+5qYlpE340wMTwfs5E3b59uHZN1yo8lw3/Z9WoSJ+YJZhG5LtnCqz6mzHh4cz0mF6MjXrlalsin8UN8BhHlopT7GPC1q4EuIAk/yxbXqodU29RKeIApM10N+aklBhj/ogkdd7tbprkhh0SZPDGIOKsqJcmVvTb92jlcOcfiWfIdsjcLcuA4S7PJ9kHx0rMbAMzXAuZE8k2ppLBEOGxGCwkDg+j5AInwIDAQAB"
	wantExtensionID = "enlbkekddkndgijidajppabineoekbfh"
)

func newTestInstaller(dir, bin string) (*chromeInstaller, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	ci := &chromeInstaller{
		stdout:        &out,
		stderr:        &errb,
		goos:          "linux",
		nativeHostDir: func(string) (string, error) { return dir, nil },
		binPath:       func() (string, error) { return bin, nil },
		dataRoot:      func() (string, error) { return filepath.Join(dir, "gocode-data"), nil },
	}
	return ci, &out, &errb
}

// writeFixtureExtension writes a minimal extension/ with a manifest carrying
// the committed key.
func writeFixtureExtension(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := map[string]any{
		"manifest_version": 3,
		"name":             "gocode browser control",
		"version":          "0.1.0",
		"key":              wantManifestKey,
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestChromeInstall_WritesWellFormedManifest(t *testing.T) {
	hostDir := t.TempDir()
	extDir := writeFixtureExtension(t)
	bin := filepath.Join(t.TempDir(), "gocode")

	ci, out, errb := newTestInstaller(hostDir, bin)
	code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir})
	if code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}

	path := filepath.Join(hostDir, "com.gocode.gocode.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	var m nativeHostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	if m.Name != "com.gocode.gocode" {
		t.Errorf("name = %q, want com.gocode.gocode", m.Name)
	}
	if m.Type != "stdio" {
		t.Errorf("type = %q, want stdio", m.Type)
	}
	if m.Path != bin {
		t.Errorf("path = %q, want %q (absolute binary path)", m.Path, bin)
	}
	wantOrigin := "chrome-extension://" + wantExtensionID + "/"
	if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != wantOrigin {
		t.Errorf("allowed_origins = %v, want [%s]", m.AllowedOrigins, wantOrigin)
	}
	if !strings.Contains(out.String(), wantExtensionID) {
		t.Errorf("install output did not mention the extension id: %q", out.String())
	}
}

func TestChromeInstall_ExtensionIDOverride(t *testing.T) {
	hostDir := t.TempDir()
	extDir := writeFixtureExtension(t)
	ci, _, errb := newTestInstaller(hostDir, "/bin/gocode")
	code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir, extensionID: "abcdefghijklmnopabcdefghijklmnop"})
	if code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	data, _ := os.ReadFile(filepath.Join(hostDir, "com.gocode.gocode.json"))
	var m nativeHostManifest
	_ = json.Unmarshal(data, &m)
	if m.AllowedOrigins[0] != "chrome-extension://abcdefghijklmnopabcdefghijklmnop/" {
		t.Errorf("override not honored: %v", m.AllowedOrigins)
	}
}

// An injected --extension-id must not widen allowed_origins.
func TestChromeInstall_RejectsMalformedExtensionID(t *testing.T) {
	hostDir := t.TempDir()
	extDir := writeFixtureExtension(t)
	ci, _, _ := newTestInstaller(hostDir, "/bin/gocode")
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir, extensionID: "*"}); code == 0 {
		t.Fatal("install accepted a wildcard extension id")
	}
	if _, err := os.Stat(filepath.Join(hostDir, "com.gocode.gocode.json")); err == nil {
		t.Fatal("manifest written for a malformed extension id")
	}
}

func TestChromeStatusAndUninstall(t *testing.T) {
	hostDir := t.TempDir()
	extDir := writeFixtureExtension(t)
	ci, out, errb := newTestInstaller(hostDir, "/opt/gocode/gocode")

	// status before install -> not installed (exit 1).
	if code := ci.status(chromeFlags{browser: "chrome"}); code != 1 {
		t.Fatalf("status before install = %d, want 1", code)
	}
	out.Reset()

	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	out.Reset()

	if code := ci.status(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("status after install = %d, want 0", code)
	}
	if !strings.Contains(out.String(), wantExtensionID) {
		t.Errorf("status did not report the id: %q", out.String())
	}
	out.Reset()

	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}
	if _, err := os.Stat(filepath.Join(hostDir, "com.gocode.gocode.json")); !os.IsNotExist(err) {
		t.Errorf("manifest still present after uninstall: %v", err)
	}
	// uninstall again -> no-op, still exit 0.
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Errorf("second uninstall = %d, want 0", code)
	}
}

func TestNativeHostDirFor_PerOS(t *testing.T) {
	cases := []struct {
		goos, browser, want string
	}{
		{"linux", "chrome", "/home/u/.config/google-chrome/NativeMessagingHosts"},
		{"linux", "chromium", "/home/u/.config/chromium/NativeMessagingHosts"},
		{"linux", "chrome-beta", "/home/u/.config/google-chrome-beta/NativeMessagingHosts"},
		{"darwin", "chrome", "/home/u/Library/Application Support/Google/Chrome/NativeMessagingHosts"},
		{"darwin", "chromium", "/home/u/Library/Application Support/Chromium/NativeMessagingHosts"},
	}
	for _, c := range cases {
		got, err := nativeHostDirFor(c.goos, "/home/u", "", c.browser)
		if err != nil {
			t.Fatalf("nativeHostDirFor(%s,%s): %v", c.goos, c.browser, err)
		}
		if want := filepath.FromSlash(c.want); got != want {
			t.Errorf("nativeHostDirFor(%s,%s) = %q, want %q", c.goos, c.browser, got, want)
		}
	}
	// Windows uses %LOCALAPPDATA% and ignores the browser for the file location.
	got, err := nativeHostDirFor("windows", `C:\Users\u`, `C:\Users\u\AppData\Local`, "chrome")
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if got != filepath.Join(`C:\Users\u\AppData\Local`, "gocode", "NativeMessagingHosts") {
		t.Errorf("windows dir = %q", got)
	}
	if _, err := nativeHostDirFor("linux", "/home/u", "", "bogus"); err == nil {
		t.Error("expected error for unknown browser")
	}
}

// runChromeHost serves the native-messaging protocol over pipes, routes a Hello
// to stderr, exits cleanly on EOF, and keeps stdout carrying nothing but
// protocol frames. A full ping round-trip through a chromehost.Host is covered
// in internal/chromehost; here the host is receive-only (M1 has no Send driver),
// so stdout must stay byte-empty.
func TestRunChromeHost_ServesCleanlyAndRoutesHello(t *testing.T) {
	extToHostR, extToHostW := io.Pipe()
	var stdout bytes.Buffer
	var stderr lockedBuffer

	env := &Env{
		Getenv: func(string) string { return "" },
		Setenv: func(string, string) {},
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  extToHostR,
	}

	done := make(chan int, 1)
	go func() { done <- runChromeHost(env) }()

	// Extension sends its Hello handshake.
	hello := chromehost.Hello{Method: chromehost.MethodHello, Name: "gocode browser control", Version: "0.1.0"}
	if err := chromehost.WriteMessage(extToHostW, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	// Close the pipe -> host sees EOF and returns.
	extToHostW.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runChromeHost = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runChromeHost did not return on EOF")
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout not clean (protocol channel): %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "extension connected") {
		t.Errorf("Hello not logged to stderr: %q", stderr.String())
	}
}

// --- test helpers -----------------------------------------------------------

// lockedBuffer is a tiny goroutine-safe buffer for stderr collected across the
// serve goroutine and the test goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestChromeHelpSurfaceRegistered(t *testing.T) {
	cmd := findSubcommand("chrome")
	if cmd == nil {
		t.Fatal("chrome subcommand not registered in RootProgram")
	}
	var names []string
	for _, s := range cmd.Subcommands {
		names = append(names, s.Name)
	}
	for _, want := range []string{"install", "uninstall", "status"} {
		if !contains(names, want) {
			t.Errorf("chrome help missing subcommand %q (have %v)", want, names)
		}
	}
	// --help for chrome must render without error.
	env, out, _ := newTestEnv()
	if code, ok := handleHelpRequest(env, []string{"chrome", "--help"}); !ok || code != 0 {
		t.Fatalf("chrome --help: ok=%v code=%d", ok, code)
	}
	if !strings.Contains(out.String(), "install") {
		t.Errorf("chrome --help did not list install: %q", out.String())
	}
}

// readHostManifest reads the host manifest install wrote into dir.
func readHostManifest(t *testing.T, dir string) nativeHostManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "com.gocode.gocode.json"))
	if err != nil {
		t.Fatalf("host manifest: %v", err)
	}
	var m nativeHostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChromeInstall_WritesEmbeddedExtensionAndTrustsStoreID(t *testing.T) {
	hostDir := t.TempDir()
	ci, out, errb := newTestInstaller(hostDir, "/opt/gocode/gocode")
	const store = "abcdefghijklmnopabcdefghijklmnop"
	const extra = "ponmlkjihgfedcbaponmlkjihgfedcba"
	ci.storeID = store
	ci.getenv = func(k string) string {
		if k == chromehost.EnvExtensionIDs {
			return extra
		}
		return ""
	}

	if code := ci.install(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	extDir := filepath.Join(hostDir, "gocode-data", "chrome-extension")
	if v, err := chromehost.InstalledExtensionVersion(extDir); err != nil || v != chromehost.EmbeddedExtensionVersion() {
		t.Fatalf("extension not written: %q, %v", v, err)
	}
	m := readHostManifest(t, hostDir)
	want := []string{
		"chrome-extension://" + wantExtensionID + "/",
		"chrome-extension://" + store + "/",
		"chrome-extension://" + extra + "/",
	}
	if strings.Join(m.AllowedOrigins, " ") != strings.Join(want, " ") {
		t.Errorf("allowed_origins = %v, want %v", m.AllowedOrigins, want)
	}
	for _, s := range []string{extDir, "Load unpacked", "chrome://extensions", "Reload", "Chrome Web Store", "(unpacked)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("install output lacks %q:\n%s", s, out.String())
		}
	}
}

func TestChromeInstall_ReplacesOlderExtensionAndStatusReportsIt(t *testing.T) {
	hostDir := t.TempDir()
	ci, out, errb := newTestInstaller(hostDir, "/opt/gocode/gocode")
	extDir := filepath.Join(hostDir, "gocode-data", "chrome-extension")

	if code := ci.install(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	// An older gocode wrote version 0.0.1.
	old := `{"manifest_version":3,"name":"gocode browser control","version":"0.0.1","key":"` + wantManifestKey + `"}`
	if err := os.WriteFile(filepath.Join(extDir, "manifest.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := ci.status(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(out.String(), "0.0.1") || !strings.Contains(out.String(), "outdated") || !strings.Contains(out.String(), "run gocode chrome install, then Reload in chrome://extensions") {
		t.Errorf("status does not flag the outdated extension:\n%s", out.String())
	}

	out.Reset()
	if code := ci.install(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("reinstall = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "replaced version 0.0.1") {
		t.Errorf("reinstall output does not mention the replaced version:\n%s", out.String())
	}
	if v, _ := chromehost.InstalledExtensionVersion(extDir); v != chromehost.EmbeddedExtensionVersion() {
		t.Errorf("version after reinstall = %q", v)
	}
	out.Reset()
	_ = ci.status(chromeFlags{browser: "chrome"})
	if strings.Contains(out.String(), "outdated") {
		t.Errorf("status still flags the extension after reinstall:\n%s", out.String())
	}
	if !strings.Contains(out.String(), wantExtensionID) {
		t.Errorf("status lacks the allowed id:\n%s", out.String())
	}

	// uninstall removes the host manifest and the extension files.
	out.Reset()
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}
	if _, err := os.Stat(extDir); !os.IsNotExist(err) {
		t.Errorf("extension files remain after uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostDir, "com.gocode.gocode.json")); !os.IsNotExist(err) {
		t.Errorf("host manifest remains after uninstall: %v", err)
	}
}

// --extension-dir loads a checkout's extension/ and writes nothing to the
// data directory; uninstall never deletes that checkout.
func TestChromeInstall_ExtensionDirSkipsWriting(t *testing.T) {
	hostDir := t.TempDir()
	extDir := writeFixtureExtension(t)
	ci, out, errb := newTestInstaller(hostDir, "/opt/gocode/gocode")
	if code := ci.install(chromeFlags{browser: "chrome", extensionDir: extDir}); code != 0 {
		t.Fatalf("install = %d, stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(hostDir, "gocode-data")); !os.IsNotExist(err) {
		t.Errorf("install with --extension-dir wrote to the data directory: %v", err)
	}
	if !strings.Contains(out.String(), extDir) {
		t.Errorf("next steps do not name %s:\n%s", extDir, out.String())
	}
	if code := ci.uninstall(chromeFlags{browser: "chrome"}); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}
	if _, err := os.Stat(filepath.Join(extDir, "manifest.json")); err != nil {
		t.Errorf("uninstall touched the --extension-dir checkout: %v", err)
	}
}

func TestDefaultChromeInstaller_ExtensionUnderDataRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cfg")
	env := &Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(k string) string {
		if k == "GOCODE_CONFIG_DIR" {
			return root
		}
		return ""
	}}
	ci := defaultChromeInstaller(env)
	got, err := ci.extractedExtensionDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "chrome-extension"); got != want {
		t.Errorf("extension dir = %q, want %q", got, want)
	}
	if ci.storeID != chromehost.StoreExtensionID {
		t.Errorf("storeID = %q, want StoreExtensionID", ci.storeID)
	}
}
