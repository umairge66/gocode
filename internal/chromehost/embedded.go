package chromehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/extension"
)

// StoreExtensionID is the id the Chrome Web Store assigned to the published
// gocode extension, or "" while it is not published.
//
// After the first publication, copy the 32-letter id (a-p) shown on the
// item's page in the Chrome Web Store Developer Dashboard (also the last path
// segment of its chromewebstore.google.com URL) into this constant and release
// a new gocode version. From then on `gocode chrome install` trusts both the
// store-installed extension and the unpacked one (whose id comes from the
// "key" in extension/manifest.json). The id never changes across store
// updates, so this is a one-time edit.
const StoreExtensionID = ""

// EnvExtensionIDs names an environment variable holding extra extension ids
// (comma- or space-separated) that `gocode chrome install` adds to the host
// manifest's allowed_origins, e.g. for a self-built fork of the extension.
const EnvExtensionIDs = "GOCODE_CHROME_EXTENSION_IDS"

// ExtensionDirName is the directory under the gocode data root that
// `gocode chrome install` writes the embedded extension into.
const ExtensionDirName = "chrome-extension"

// ExtensionDir returns where the embedded extension is written for the given
// gocode data root.
func ExtensionDir(dataRoot string) string {
	return filepath.Join(dataRoot, ExtensionDirName)
}

// EmbeddedExtensionVersion is the version of the extension built into this
// gocode binary.
func EmbeddedExtensionVersion() string { return extension.Version() }

// EmbeddedExtensionID is the id Chrome gives the embedded extension when it is
// loaded unpacked (derived from its manifest key).
func EmbeddedExtensionID() (string, error) {
	return ExtensionIDFromKey(extension.Info().Key)
}

// TrustedExtensionIDs returns the ids a host manifest should allow: primary
// (the unpacked extension's id), then store (StoreExtensionID in production)
// when set, then any ids from EnvExtensionIDs. Duplicates are dropped; a malformed id is an error so
// allowed_origins can never be widened by a typo.
func TrustedExtensionIDs(primary, store string, getenv func(string) string) ([]string, error) {
	var out []string
	add := func(id, source string) error {
		if id == "" {
			return nil
		}
		if !ValidExtensionID(id) {
			return fmt.Errorf("invalid extension id %q from %s (want 32 letters a-p)", id, source)
		}
		for _, have := range out {
			if have == id {
				return nil
			}
		}
		out = append(out, id)
		return nil
	}
	if err := add(primary, "the extension manifest"); err != nil {
		return nil, err
	}
	if err := add(store, "the Chrome Web Store id"); err != nil {
		return nil, err
	}
	if getenv != nil {
		for _, id := range strings.FieldsFunc(getenv(EnvExtensionIDs), func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if err := add(id, EnvExtensionIDs); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// WriteExtension writes the embedded extension's runtime files to dir,
// replacing whatever is there. The files are first written to a fresh
// directory next to dir, which then takes dir's place, so Chrome never sees a
// half-written extension. dir and its files are readable by the owner only.
func WriteExtension(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".new-")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	for _, name := range extension.RuntimeFiles {
		data, err := extension.ReadFile(name)
		if err != nil {
			cleanup()
			return err
		}
		path := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			cleanup()
			return err
		}
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		cleanup()
		return err
	}

	// Move the previous copy aside first: a directory cannot be renamed over
	// a non-empty one on every OS.
	var old string
	if _, err := os.Lstat(dir); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dir, old); err != nil {
			cleanup()
			return fmt.Errorf("replace %s: %w", dir, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		cleanup()
		return fmt.Errorf("install %s: %w", dir, err)
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// InstalledExtensionVersion reads the version from dir/manifest.json. It
// returns an error wrapping fs.ErrNotExist when no extension is there.
func InstalledExtensionVersion(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", err
	}
	m, err := extension.ParseManifest(b)
	if err != nil {
		return "", err
	}
	return m.Version, nil
}

// CompareVersions compares two dotted extension versions ("0.3.0") field by
// field, numerically; a missing field counts as 0 and a non-numeric field
// compares as 0. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(strings.TrimSpace(as[i]))
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(strings.TrimSpace(bs[i]))
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// ExtensionVersionNotice returns the message to show when the connected
// extension's version differs from the embedded one, or "" when they match
// (or the connected version is unknown).
func ExtensionVersionNotice(connected, embedded string) string {
	if connected == "" || embedded == "" {
		return ""
	}
	switch CompareVersions(connected, embedded) {
	case -1:
		return fmt.Sprintf("The gocode Chrome extension in your browser is version %s, but this gocode expects %s. "+
			"Run `gocode chrome install`, then click Reload on \"gocode browser control\" in chrome://extensions. "+
			"(If you installed it from the Chrome Web Store, it updates by itself.)", connected, embedded)
	case 1:
		return fmt.Sprintf("The gocode Chrome extension in your browser is version %s, newer than this gocode expects (%s). "+
			"Update gocode, or run `gocode chrome install` and click Reload in chrome://extensions to use the matching extension.", connected, embedded)
	}
	return ""
}

// HostInfo is the reply to MethodHostInfo: what the native host knows about
// the extension connected to it.
type HostInfo struct {
	// ExtensionVersion is the version from the extension's hello, or "" when
	// no hello has arrived yet.
	ExtensionVersion string `json:"extensionVersion"`
}

// MethodHostInfo is a control request the native host answers itself (it is
// not relayed to the extension) with a HostInfo.
const MethodHostInfo = "gocode.host/info"

// ExtensionVersionCheck compares the connected extension's version with the
// embedded one, once per process, and yields the mismatch notice once.
type ExtensionVersionCheck struct {
	// Call sends a control request to the native host (Client.Call).
	Call func(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Expected is the embedded extension version.
	Expected string

	mu       sync.Mutex
	done     bool
	attempts int
}

// maxVersionCheckAttempts bounds how often Notice asks a host that has not
// answered (an older host, or no hello yet) before giving up for the process.
const maxVersionCheckAttempts = 3

// versionCheckTimeout bounds one version query so it never delays a tool
// result noticeably.
const versionCheckTimeout = 2 * time.Second

// Notice returns the version-mismatch message the first time a mismatch is
// seen and "" otherwise. It asks the host at most a few times and never
// returns an error: a check that cannot be made is skipped.
func (c *ExtensionVersionCheck) Notice(ctx context.Context) string {
	if c == nil || c.Call == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return ""
	}
	c.attempts++
	if c.attempts >= maxVersionCheckAttempts {
		c.done = true
	}
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	raw, err := c.Call(ctx, MethodHostInfo, nil)
	if err != nil {
		return ""
	}
	var info HostInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.ExtensionVersion == "" {
		return ""
	}
	c.done = true
	return ExtensionVersionNotice(info.ExtensionVersion, c.Expected)
}
