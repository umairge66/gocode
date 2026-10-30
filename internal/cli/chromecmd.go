package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/chromehost"
)

// This file wires gocode's LOCAL Chrome-control feature (milestone 1):
//
//   - `gocode --chrome-native-host` runs the native-messaging host: it serves
//     the stdio protocol to our MV3 extension (internal/chromehost.Host) until
//     the extension/Chrome closes the pipe or the process is interrupted.
//     stdout is the protocol channel — nothing but frames is written there.
//
//   - `gocode chrome install|uninstall|status` writes (and removes) the
//     extension built into gocode under <data root>/chrome-extension for
//     "Load unpacked", and manages the per-OS/per-browser native-messaging
//     *host manifest* (com.gocode.gocode.json) that tells Chrome how to launch
//     the host and which extension ids may connect to it.
//
// There is no network, no account and no telemetry anywhere here: Chrome runs
// the host locally over stdio, and its allowed_origins check (the extension id
// computed from the manifest key) is the entire trust boundary — no port is
// opened and no token is shared.

// nativeHostName is the native-messaging host id. It must match the name the
// extension passes to chrome.runtime.connectNative and the manifest filename.
const nativeHostName = "com.gocode.gocode"

// runChromeHost runs the native-messaging host over stdin/stdout. It is the
// handler for `gocode --chrome-native-host`, the flag Chrome invokes via the
// installed host manifest. In milestone 1 nothing drives the host's Send seam
// (the live session that will is milestone 2); the host serves the inbound
// handshake/events and keeps stdout clean for the protocol.
//
// Mirrors runMcpServe: stdout carries only protocol frames, every diagnostic
// goes to stderr, and there is no banner.
func runChromeHost(env *Env) int {
	if env.Stdin == nil {
		fmt.Fprintln(env.Stderr, "Error: --chrome-native-host requires a stdin stream (it is launched by Chrome native messaging).")
		return 1
	}

	h := chromehost.NewHost(env.Stdin, env.Stdout)
	h.OnError(func(err error) {
		fmt.Fprintf(env.Stderr, "chrome-native-host: %v\n", err)
	})
	// extVersion is the version the connected extension reported in its
	// hello; the control socket hands it to gocode sessions (MethodHostInfo)
	// so they can tell the user when the extension needs a reload.
	var (
		extMu      sync.Mutex
		extVersion string
	)
	h.OnEvent(func(_ context.Context, ev chromehost.Event) {
		if ev.Method == chromehost.MethodHello {
			var hello chromehost.Hello
			if err := json.Unmarshal(ev.Raw, &hello); err == nil {
				extMu.Lock()
				extVersion = hello.Version
				extMu.Unlock()
				fmt.Fprintf(env.Stderr, "chrome-native-host: extension connected (%s %s)\n", hello.Name, hello.Version)
				if notice := chromehost.ExtensionVersionNotice(hello.Version, chromehost.EmbeddedExtensionVersion()); notice != "" {
					fmt.Fprintf(env.Stderr, "chrome-native-host: %s\n", notice)
				}
				return
			}
		}
		fmt.Fprintf(env.Stderr, "chrome-native-host: event %q\n", ev.Method)
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Milestone 2: ALSO listen on the per-user LOCAL control socket so the live
	// gocode session (a separate process) can drive this host. The socket reuses
	// the daemon's runtime-dir layout + same-uid peer-cred authorizer, binds under
	// a distinct basename (chrome-host.sock), and takes over any stale socket left
	// by a prior host (singleton: the newest host owns the socket). It relays each
	// request to the extension via h.Send. stdout stays the native-messaging
	// protocol channel — the socket is a separate fd and all logs go to stderr.
	cfgDir := chromehost.DefaultConfigDir(env.Getenv, os.UserHomeDir)
	cs := &chromehost.ControlServer{
		Relay: h,
		Logf:  func(format string, args ...any) { fmt.Fprintf(env.Stderr, format+"\n", args...) },
		Info: func() chromehost.HostInfo {
			extMu.Lock()
			defer extMu.Unlock()
			return chromehost.HostInfo{ExtensionVersion: extVersion}
		},
	}
	go func() {
		if err := cs.ListenAndServe(ctx, cfgDir); err != nil && ctx.Err() == nil {
			fmt.Fprintf(env.Stderr, "chrome-native-host: control socket: %v\n", err)
		}
	}()

	if err := h.Serve(ctx); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintf(env.Stderr, "chrome-native-host: %v\n", err)
		return 1
	}
	return 0
}

// runChromeCmd dispatches the `gocode chrome <install|uninstall|status>` group.
func runChromeCmd(env *Env, rest []string) int {
	sub := secondPositionalAfter(rest, "chrome")
	ci := defaultChromeInstaller(env)
	switch sub {
	case "install":
		opts := parseChromeFlags(rest)
		return ci.install(opts)
	case "uninstall":
		opts := parseChromeFlags(rest)
		return ci.uninstall(opts)
	case "status":
		opts := parseChromeFlags(rest)
		return ci.status(opts)
	case "":
		fmt.Fprintln(env.Stderr, "Usage: gocode chrome <install|uninstall|status> [--browser <name>] [--extension-dir <path>] [--extension-id <id>]")
		return 1
	default:
		fmt.Fprintf(env.Stderr, "gocode chrome: unknown subcommand %q (expected install, uninstall or status)\n", sub)
		return 1
	}
}

// chromeFlags are the options shared by the chrome subcommands.
type chromeFlags struct {
	browser      string
	extensionDir string
	extensionID  string
}

func parseChromeFlags(rest []string) chromeFlags {
	f := chromeFlags{browser: "chrome"}
	// Narrow to the tokens after the `chrome` verb.
	scope := rest
	if i := indexOf(rest, "chrome"); i != -1 {
		scope = rest[i+1:]
	}
	takeValue := func(i int, inline string, hasInline bool) (string, int) {
		if hasInline {
			return inline, i
		}
		if i+1 < len(scope) {
			return scope[i+1], i + 1
		}
		return "", i
	}
	for i := 0; i < len(scope); i++ {
		arg := scope[i]
		name, inline, hasInline := arg, "", false
		if eq := strings.Index(arg, "="); eq >= 0 && strings.HasPrefix(arg, "--") {
			name, inline, hasInline = arg[:eq], arg[eq+1:], true
		}
		switch name {
		case "--browser":
			f.browser, i = takeValue(i, inline, hasInline)
		case "--extension-dir":
			f.extensionDir, i = takeValue(i, inline, hasInline)
		case "--extension-id":
			f.extensionID, i = takeValue(i, inline, hasInline)
		}
	}
	if f.browser == "" {
		f.browser = "chrome"
	}
	return f
}

// secondPositionalAfter returns the first non-flag token that follows `verb`.
func secondPositionalAfter(rest []string, verb string) string {
	i := indexOf(rest, verb)
	if i == -1 {
		return ""
	}
	for _, x := range rest[i+1:] {
		if !strings.HasPrefix(x, "-") {
			return x
		}
	}
	return ""
}

// nativeHostManifest is the Chrome native-messaging host manifest written by
// `chrome install`. See
// https://developer.chrome.com/docs/apps/nativeMessaging for the schema.
type nativeHostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// chromeInstaller holds the injectable seams so install/uninstall/status can be
// unit-tested into a temp directory with a synthetic binary path.
type chromeInstaller struct {
	stdout, stderr io.Writer
	goos           string
	// nativeHostDir resolves the NativeMessagingHosts directory for a browser.
	nativeHostDir func(browser string) (string, error)
	// binPath resolves the absolute path to the gocode executable (Path field).
	binPath func() (string, error)
	// dataRoot resolves the gocode data root; the embedded extension is
	// written to its chrome-extension/ directory.
	dataRoot func() (string, error)
	// getenv reads environment variables (EnvExtensionIDs). nil reads none.
	getenv func(string) string
	// storeID is the Chrome Web Store extension id ("" while unpublished).
	storeID string
	// registry writes the Windows HKCU native-host keys (only used when
	// goos == "windows").
	registry nativeHostRegistry
}

// nativeHostRegistry is the seam over the Windows registry: keys are full
// `HKCU\...` paths, and only their default (unnamed) REG_SZ value is used.
// GetDefault and DeleteKey return errRegistryKeyAbsent for a missing key.
type nativeHostRegistry interface {
	SetDefault(key, value string) error
	GetDefault(key string) (string, error)
	DeleteKey(key string) error
}

// errRegistryKeyAbsent reports a registry key/value that does not exist.
var errRegistryKeyAbsent = errors.New("registry key not found")

// windowsBrowserKey is one browser's HKCU NativeMessagingHosts parent key.
type windowsBrowserKey struct {
	browser string
	key     string
}

// windowsNativeHostKeys lists every Chromium-family browser's per-user
// NativeMessagingHosts key. Install registers the (single,
// browser-independent) Windows manifest under ALL of
// them, so the extension works in whichever of these browsers loads it.
var windowsNativeHostKeys = []windowsBrowserKey{
	{"chrome", `HKCU\Software\Google\Chrome\NativeMessagingHosts`},
	{"brave", `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts`},
	{"arc", `HKCU\Software\ArcBrowser\Arc\NativeMessagingHosts`},
	{"edge", `HKCU\Software\Microsoft\Edge\NativeMessagingHosts`},
	{"chromium", `HKCU\Software\Chromium\NativeMessagingHosts`},
	{"vivaldi", `HKCU\Software\Vivaldi\NativeMessagingHosts`},
	{"opera", `HKCU\Software\Opera Software\Opera Stable\NativeMessagingHosts`},
}

func defaultChromeInstaller(env *Env) *chromeInstaller {
	return &chromeInstaller{
		stdout: env.Stdout,
		stderr: env.Stderr,
		goos:   runtime.GOOS,
		nativeHostDir: func(browser string) (string, error) {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return nativeHostDirFor(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"), browser)
		},
		binPath: os.Executable,
		dataRoot: func() (string, error) {
			return chromehost.DefaultConfigDir(env.Getenv, os.UserHomeDir), nil
		},
		getenv:   env.Getenv,
		storeID:  chromehost.StoreExtensionID,
		registry: defaultNativeHostRegistry(),
	}
}

// extractedExtensionDir is where install writes the embedded extension.
func (ci *chromeInstaller) extractedExtensionDir() (string, error) {
	if ci.dataRoot == nil {
		return "", errors.New("no gocode data directory")
	}
	root, err := ci.dataRoot()
	if err != nil {
		return "", err
	}
	return chromehost.ExtensionDir(root), nil
}

func (ci *chromeInstaller) install(f chromeFlags) int {
	if f.extensionID != "" && !chromehost.ValidExtensionID(f.extensionID) {
		fmt.Fprintf(ci.stderr, "gocode chrome install: invalid extension id %q (want 32 letters a-p)\n", f.extensionID)
		return 1
	}

	// Without --extension-dir, write the extension built into this binary to
	// the gocode data directory (replacing an older copy) and load it from
	// there; --extension-dir points at a checkout's extension/ instead.
	extDir := f.extensionDir
	extracted := false
	if extDir == "" {
		d, err := ci.extractedExtensionDir()
		if err != nil {
			fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
			return 1
		}
		prev, _ := chromehost.InstalledExtensionVersion(d)
		if err := chromehost.WriteExtension(d); err != nil {
			fmt.Fprintf(ci.stderr, "gocode chrome install: write extension: %v\n", err)
			return 1
		}
		extDir, extracted = d, true
		version := chromehost.EmbeddedExtensionVersion()
		fmt.Fprintf(ci.stdout, "Wrote the gocode Chrome extension (version %s):\n  %s\n", version, d)
		if prev != "" && prev != version {
			fmt.Fprintf(ci.stdout, "  (replaced version %s)\n", prev)
		}
		fmt.Fprintln(ci.stdout)
	}

	primary := f.extensionID
	if primary == "" {
		key, err := readManifestKey(filepath.Join(extDir, "manifest.json"))
		if err != nil {
			fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
			return 1
		}
		primary, err = chromehost.ExtensionIDFromKey(key)
		if err != nil {
			fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
			return 1
		}
	}
	ids, err := chromehost.TrustedExtensionIDs(primary, ci.storeID, ci.getenv)
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
		return 1
	}

	bin, err := ci.binPath()
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: cannot resolve gocode binary path: %v\n", err)
		return 1
	}
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}

	dir, err := ci.nativeHostDir(f.browser)
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: create %s: %v\n", dir, err)
		return 1
	}

	manifest := nativeHostManifest{
		Name:           nativeHostName,
		Description:    "gocode local browser control host",
		Path:           bin,
		Type:           "stdio",
		AllowedOrigins: make([]string, 0, len(ids)),
	}
	for _, id := range ids {
		manifest.AllowedOrigins = append(manifest.AllowedOrigins, "chrome-extension://"+id+"/")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: %v\n", err)
		return 1
	}
	data = append(data, '\n')

	path := filepath.Join(dir, nativeHostName+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome install: write %s: %v\n", path, err)
		return 1
	}

	fmt.Fprintf(ci.stdout, "Wrote native-messaging host manifest:\n  %s\n", path)
	fmt.Fprintf(ci.stdout, "  host:          %s\n  binary:        %s\n", nativeHostName, bin)
	ci.printAllowedIDs(manifest.AllowedOrigins)
	code := 0
	if ci.goos == "windows" {
		code = ci.registerWindows(f.browser, path)
	}
	fmt.Fprintf(ci.stdout, "\nNext steps:\n")
	fmt.Fprintf(ci.stdout, "  1. Open chrome://extensions and turn on Developer mode (top right).\n")
	fmt.Fprintf(ci.stdout, "  2. Click \"Load unpacked\" and choose this folder:\n       %s\n", extDir)
	fmt.Fprintf(ci.stdout, "     If it is already loaded from there, click Reload on \"gocode browser control\" instead.\n")
	fmt.Fprintf(ci.stdout, "  3. Restart Chrome so it picks up the host manifest.\n")
	if ci.storeID != "" {
		fmt.Fprintf(ci.stdout, "  Instead of steps 1-2 you can install \"gocode browser control\" from the Chrome Web Store\n  (https://chromewebstore.google.com/detail/%s); it updates by itself.\n", ci.storeID)
	}
	if extracted {
		fmt.Fprintf(ci.stdout, "\nAfter updating gocode, run `gocode chrome install` again and click Reload in chrome://extensions.\n")
	} else {
		fmt.Fprintf(ci.stdout, "\nAfter changing the files in %s, click Reload in chrome://extensions.\n", extDir)
	}
	return code
}

// printAllowedIDs lists the extension ids in allowed_origins, naming where
// each one comes from when known.
func (ci *chromeInstaller) printAllowedIDs(origins []string) {
	if len(origins) == 0 {
		fmt.Fprintf(ci.stdout, "  allowed ids:   (none)\n")
		return
	}
	embedded, _ := chromehost.EmbeddedExtensionID()
	for i, o := range origins {
		id := extensionIDFromOrigin(o)
		label := ""
		switch {
		case ci.storeID != "" && id == ci.storeID:
			label = " (Chrome Web Store)"
		case id == embedded:
			label = " (unpacked)"
		}
		prefix := "  allowed ids:  "
		if i > 0 {
			prefix = "                "
		}
		fmt.Fprintf(ci.stdout, "%s %s%s\n", prefix, id, label)
	}
}

// registerWindows points every known browser's HKCU NativeMessagingHosts\
// com.gocode.gocode key at the manifest (default value = manifest path). A
// failure for the SELECTED browser makes install fail (exit 1) after printing
// the equivalent `reg add` command as a manual fallback; failures for the other
// browsers are reported but not fatal.
func (ci *chromeInstaller) registerWindows(browser, manifestPath string) int {
	selected := windowsRegistryKey(browser)
	code := 0
	var failed []string
	registered := 0
	for _, bk := range windowsNativeHostKeys {
		key := bk.key + `\` + nativeHostName
		if err := ci.registry.SetDefault(key, manifestPath); err != nil {
			fmt.Fprintf(ci.stderr, "gocode chrome install: register for %s (%s): %v\n", bk.browser, key, err)
			failed = append(failed, key)
			if key == selected {
				code = 1
			}
			continue
		}
		if registered == 0 {
			fmt.Fprintf(ci.stdout, "\nRegistered the host in the Windows registry (HKCU, no admin needed):\n")
		}
		registered++
		fmt.Fprintf(ci.stdout, "  %-9s %s\n", bk.browser, key)
	}
	if len(failed) > 0 {
		fmt.Fprintf(ci.stdout, "\nSome registry keys could not be written; register them manually with:\n")
		for _, key := range failed {
			fmt.Fprintf(ci.stdout, "  reg add \"%s\" /ve /t REG_SZ /d \"%s\" /f\n", key, manifestPath)
		}
	}
	return code
}

// unregisterWindows deletes every browser's com.gocode.gocode key. Missing
// keys are fine; any other failure is reported with a `reg delete` fallback
// and makes uninstall exit 1.
func (ci *chromeInstaller) unregisterWindows() int {
	code := 0
	removed := 0
	for _, bk := range windowsNativeHostKeys {
		key := bk.key + `\` + nativeHostName
		err := ci.registry.DeleteKey(key)
		switch {
		case err == nil:
			if removed == 0 {
				fmt.Fprintf(ci.stdout, "Removed Windows registry entries:\n")
			}
			removed++
			fmt.Fprintf(ci.stdout, "  %s\n", key)
		case errors.Is(err, errRegistryKeyAbsent):
		default:
			fmt.Fprintf(ci.stderr, "gocode chrome uninstall: delete %s: %v\n", key, err)
			fmt.Fprintf(ci.stdout, "Remove it manually with:\n  reg delete \"%s\" /f\n", key)
			code = 1
		}
	}
	if removed == 0 && code == 0 {
		fmt.Fprintf(ci.stdout, "No Windows registry entries to remove.\n")
	}
	return code
}

func (ci *chromeInstaller) uninstall(f chromeFlags) int {
	dir, err := ci.nativeHostDir(f.browser)
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome uninstall: %v\n", err)
		return 1
	}
	code := 0
	path := filepath.Join(dir, nativeHostName+".json")
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(ci.stderr, "gocode chrome uninstall: remove %s: %v\n", path, err)
			return 1
		}
		fmt.Fprintf(ci.stdout, "No host manifest to remove at %s\n", path)
	} else {
		fmt.Fprintf(ci.stdout, "Removed native-messaging host manifest:\n  %s\n", path)
	}
	if ci.goos == "windows" {
		// Remove the registry entries even when the manifest was already gone,
		// so a half-uninstalled host does not stay registered.
		code = ci.unregisterWindows()
	}
	if extDir, err := ci.extractedExtensionDir(); err == nil {
		if _, err := os.Stat(extDir); err == nil {
			if err := os.RemoveAll(extDir); err != nil {
				fmt.Fprintf(ci.stderr, "gocode chrome uninstall: remove %s: %v\n", extDir, err)
				code = 1
			} else {
				fmt.Fprintf(ci.stdout, "Removed the extension files:\n  %s\n", extDir)
				fmt.Fprintf(ci.stdout, "Remove \"gocode browser control\" in chrome://extensions as well.\n")
			}
		}
	}
	return code
}

func (ci *chromeInstaller) status(f chromeFlags) int {
	ci.extensionStatus()
	dir, err := ci.nativeHostDir(f.browser)
	if err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome status: %v\n", err)
		return 1
	}
	path := filepath.Join(dir, nativeHostName+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(ci.stdout, "Host manifest: not installed (no %s; run gocode chrome install)\n", path)
			return 1
		}
		fmt.Fprintf(ci.stderr, "gocode chrome status: read %s: %v\n", path, err)
		return 1
	}
	var m nativeHostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		fmt.Fprintf(ci.stderr, "gocode chrome status: %s is not valid JSON: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(ci.stdout, "Host manifest: %s\n", path)
	fmt.Fprintf(ci.stdout, "  host:          %s\n  binary:        %s\n", m.Name, m.Path)
	ci.printAllowedIDs(m.AllowedOrigins)
	if ci.goos == "windows" {
		key := windowsRegistryKey(f.browser)
		v, err := ci.registry.GetDefault(key)
		switch {
		case err == nil && strings.EqualFold(filepath.Clean(v), filepath.Clean(path)):
			fmt.Fprintf(ci.stdout, "  registry:      %s\n", key)
		case err == nil:
			fmt.Fprintf(ci.stdout, "  registry:      %s points at %s, not this manifest (re-run gocode chrome install)\n", key, v)
			return 1
		case errors.Is(err, errRegistryKeyAbsent):
			fmt.Fprintf(ci.stdout, "  registry:      not registered (%s missing; re-run gocode chrome install)\n", key)
			return 1
		default:
			fmt.Fprintf(ci.stderr, "gocode chrome status: read %s: %v\n", key, err)
			return 1
		}
	}
	return 0
}

// extensionStatus reports the extension files install wrote, comparing their
// version with the one built into this gocode.
func (ci *chromeInstaller) extensionStatus() {
	embedded := chromehost.EmbeddedExtensionVersion()
	dir, err := ci.extractedExtensionDir()
	if err != nil {
		fmt.Fprintf(ci.stdout, "Extension files: unknown location (%v)\n", err)
		return
	}
	installed, err := chromehost.InstalledExtensionVersion(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(ci.stdout, "Extension files: not written (built-in version %s; run gocode chrome install)\n", embedded)
		return
	case err != nil:
		fmt.Fprintf(ci.stdout, "Extension files: %s is unreadable (%v; run gocode chrome install)\n", dir, err)
		return
	}
	fmt.Fprintf(ci.stdout, "Extension files: %s\n", dir)
	fmt.Fprintf(ci.stdout, "  version:       %s (built into this gocode: %s)\n", installed, embedded)
	if installed != embedded {
		fmt.Fprintf(ci.stdout, "  warning:       outdated; run gocode chrome install, then Reload in chrome://extensions\n")
	}
}

// extensionIDFromOrigin extracts the bare id from a "chrome-extension://<id>/"
// origin, returning the origin unchanged if it is not in that form.
func extensionIDFromOrigin(origin string) string {
	s := strings.TrimPrefix(origin, "chrome-extension://")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return origin
	}
	return s
}

// nativeHostDirFor computes the NativeMessagingHosts directory for the given OS
// and browser. home is the user's home directory; localAppData is used on
// Windows (where there is no per-browser directory — the manifest lives under
// %LOCALAPPDATA% and is registered via the registry instead).
//
// Linux:   ~/.config/<browser-dir>/NativeMessagingHosts
// macOS:   ~/Library/Application Support/<browser-dir>/NativeMessagingHosts
// Windows: %LOCALAPPDATA%\gocode\NativeMessagingHosts
func nativeHostDirFor(goos, home, localAppData, browser string) (string, error) {
	if browser == "" {
		browser = "chrome"
	}
	switch goos {
	case "windows":
		base := localAppData
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "gocode", "NativeMessagingHosts"), nil
	case "darwin":
		seg, ok := map[string]string{
			"chrome":        filepath.Join("Google", "Chrome"),
			"chromium":      "Chromium",
			"chrome-beta":   filepath.Join("Google", "Chrome Beta"),
			"chrome-dev":    filepath.Join("Google", "Chrome Dev"),
			"chrome-canary": filepath.Join("Google", "Chrome Canary"),
		}[browser]
		if !ok {
			return "", fmt.Errorf("unknown browser %q", browser)
		}
		return filepath.Join(home, "Library", "Application Support", seg, "NativeMessagingHosts"), nil
	default: // linux and other unix
		seg, ok := map[string]string{
			"chrome":      "google-chrome",
			"chromium":    "chromium",
			"chrome-beta": "google-chrome-beta",
			"chrome-dev":  "google-chrome-unstable",
		}[browser]
		if !ok {
			return "", fmt.Errorf("unknown browser %q (linux supports chrome, chromium, chrome-beta, chrome-dev)", browser)
		}
		return filepath.Join(home, ".config", seg, "NativeMessagingHosts"), nil
	}
}

// windowsRegistryKey returns the HKCU registry key under which the host
// manifest path is registered for the given browser. Chrome's beta/dev/canary
// channels and any unknown name use Google Chrome's key (all Chrome channels
// on Windows read HKCU\Software\Google\Chrome\NativeMessagingHosts).
func windowsRegistryKey(browser string) string {
	for _, bk := range windowsNativeHostKeys {
		if bk.browser == browser {
			return bk.key + `\` + nativeHostName
		}
	}
	return windowsNativeHostKeys[0].key + `\` + nativeHostName
}

// readManifestKey reads the base64 "key" field from an extension manifest.json.
func readManifestKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read extension manifest %s: %w", path, err)
	}
	var m struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse extension manifest %s: %w", path, err)
	}
	if m.Key == "" {
		return "", fmt.Errorf("extension manifest %s has no \"key\" field; cannot compute a stable extension id (pass --extension-id to override)", path)
	}
	return m.Key, nil
}
