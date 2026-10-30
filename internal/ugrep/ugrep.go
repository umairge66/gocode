// Package ugrep locates the ugrep binary that gocode searches with: the Grep
// tool's content matching and the `grep` command of the model's shell.
//
// gocode ships a ugrep binary for its own GOOS/GOARCH embedded in the
// executable when one is vendored for that platform (bin/<goos>_<goarch>/, see
// third_party/ugrep/UPDATING.md). On first use the embedded binary is written
// to <user cache dir>/gocode/ugrep/<version>/ugrep[.exe] (temp file + rename,
// mode 0755) and its sha256 is checked against the checksum embedded next to
// it; a file that does not verify is never executed — it is rewritten instead.
//
// Resolution order (ResolveDetailed):
//
//  1. search.useUgrep == false                → built-in Go engine
//  2. GOCODE_UGREP_PATH, else search.ugrepPath (must be an executable file)
//  3. ugrep on PATH, when search.preferSystemUgrep is set
//  4. the bundled binary
//  5. ugrep on PATH
//  6. built-in Go engine
//
// Nothing here talks to the network: ugrep is never downloaded at runtime.
package ugrep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Source says where the resolved search engine came from.
type Source string

const (
	// SourceBuiltin means no ugrep is used: the built-in Go engine runs.
	SourceBuiltin Source = "built-in"
	// SourceConfigured is an explicit path from GOCODE_UGREP_PATH or the
	// search.ugrepPath setting.
	SourceConfigured Source = "configured"
	// SourceSystem is a ugrep found on PATH.
	SourceSystem Source = "system"
	// SourceBundled is the binary embedded in gocode.
	SourceBundled Source = "bundled"
)

// EnvUgrepPath names an explicit ugrep binary; it wins over the
// search.ugrepPath setting.
const EnvUgrepPath = "GOCODE_UGREP_PATH"

// Bundle is an embedded ugrep binary plus its identity.
type Bundle struct {
	Version string // e.g. "7.8.5" (from bin/VERSION)
	SHA256  string // lowercase hex sha256 of Data
	Name    string // "ugrep" or "ugrep.exe"
	Data    []byte
}

// Bundled returns the binary embedded for this GOOS/GOARCH, or nil when this
// build carries none (no binary vendored for the platform, or the
// gocode_nobundledugrep build tag).
func Bundled() *Bundle {
	bundledOnce.Do(func() { bundledValue = loadBundle(bundleFS, bundleDir) })
	return bundledValue
}

var (
	bundledOnce  sync.Once
	bundledValue *Bundle
)

// loadBundle reads <dir>/ugrep[.exe], <dir>/ugrep.sha256 and bin/VERSION from
// fsys; any of them missing means no bundle.
func loadBundle(fsys embed.FS, dir string) *Bundle {
	if dir == "" {
		return nil
	}
	name := ExeName()
	data, err := fsys.ReadFile(dir + "/" + name)
	if err != nil || len(data) == 0 {
		return nil
	}
	sum, err := fsys.ReadFile(dir + "/ugrep.sha256")
	if err != nil {
		return nil
	}
	version, err := fsys.ReadFile("bin/VERSION")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(sum))
	if len(fields) == 0 {
		return nil
	}
	return &Bundle{
		Version: strings.TrimSpace(string(version)),
		SHA256:  strings.ToLower(fields[0]),
		Name:    name,
		Data:    data,
	}
}

// BundledVersion returns the embedded ugrep version, or "" when none.
func BundledVersion() string {
	if b := Bundled(); b != nil {
		return b.Version
	}
	return ""
}

// Config is the input to ResolveDetailed. The zero value resolves with the defaults
// (ugrep on, bundled preferred) against the real environment.
type Config struct {
	// UseUgrep false selects the built-in Go engine; nil means true.
	UseUgrep *bool
	// UgrepPath is the search.ugrepPath setting.
	UgrepPath string
	// PreferSystem is the search.preferSystemUgrep setting.
	PreferSystem bool

	// Getenv reads the environment (nil: os.Getenv).
	Getenv func(string) string
	// LookPath finds a program on PATH (nil: exec.LookPath).
	LookPath func(string) (string, error)
	// CacheDir returns the user cache root (nil: os.UserCacheDir); the
	// binary goes to <root>/gocode/ugrep/<version>/.
	CacheDir func() (string, error)
	// Bundle returns the embedded binary (nil: Bundled). Returning nil means
	// "no bundled binary".
	Bundle func() *Bundle
}

func (c Config) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c Config) lookPath(name string) (string, error) {
	if c.LookPath != nil {
		return c.LookPath(name)
	}
	return exec.LookPath(name)
}

func (c Config) bundle() *Bundle {
	if c.Bundle != nil {
		return c.Bundle()
	}
	return Bundled()
}

func (c Config) cacheRoot() (string, error) {
	if c.CacheDir != nil {
		return c.CacheDir()
	}
	return os.UserCacheDir()
}

// ExeName is the ugrep file name on this platform.
func ExeName() string {
	if runtime.GOOS == "windows" {
		return "ugrep.exe"
	}
	return "ugrep"
}

// ErrNotFound reports that no usable ugrep was found; the caller uses the
// built-in engine.
var ErrNotFound = errors.New("ugrep not available")

// Resolution is the full result of ResolveDetailed.
type Resolution struct {
	Path    string
	Source  Source
	Version string // known for bundled; "" otherwise (see ProbeVersion)
	Notes   []string
	Err     error
}

// ResolveDetailed picks the ugrep binary per the package resolution order,
// with the version and one-line notes about candidates that were skipped (e.g.
// a configured path that does not exist). Source is SourceBuiltin with a nil
// Err when ugrep is turned off, and SourceBuiltin with Err set when no ugrep
// could be used (Err explains why; the built-in engine runs).
func ResolveDetailed(cfg Config) Resolution {
	var res Resolution
	if cfg.UseUgrep != nil && !*cfg.UseUgrep {
		res.Source = SourceBuiltin
		return res
	}

	explicit, explicitFrom := strings.TrimSpace(cfg.getenv(EnvUgrepPath)), EnvUgrepPath
	if explicit == "" {
		explicit, explicitFrom = strings.TrimSpace(cfg.UgrepPath), "search.ugrepPath"
	}
	if explicit != "" {
		if err := checkExecutable(explicit); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("ugrep: %s=%q is not usable (%v); trying other sources", explicitFrom, explicit, err))
		} else {
			res.Path, res.Source = explicit, SourceConfigured
			return res
		}
	}

	if cfg.PreferSystem {
		if p, err := cfg.lookPath("ugrep"); err == nil && p != "" {
			res.Path, res.Source = p, SourceSystem
			return res
		}
	}

	if b := cfg.bundle(); b != nil {
		root, err := cfg.cacheRoot()
		if err == nil {
			var p string
			p, err = Extract(b, root)
			if err == nil {
				res.Path, res.Source, res.Version = p, SourceBundled, b.Version
				return res
			}
		}
		res.Notes = append(res.Notes, fmt.Sprintf("ugrep: bundled ugrep %s unavailable (%v)", b.Version, err))
	}

	if p, err := cfg.lookPath("ugrep"); err == nil && p != "" {
		res.Path, res.Source = p, SourceSystem
		return res
	}
	res.Source = SourceBuiltin
	res.Err = ErrNotFound
	return res
}

// checkExecutable reports whether p names a regular, executable file.
func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return errors.New("not executable")
	}
	return nil
}

var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)

// extractMu serializes extraction within the process.
var extractMu sync.Mutex

// staleCacheDirs are directories under <cacheRoot>/gocode that earlier gocode
// releases extracted a search binary into; they are removed once ugrep is
// extracted.
var staleCacheDirs = []string{"ripgrep"}

// Extract writes b to <cacheRoot>/gocode/ugrep/<version>/<name> unless a copy
// whose sha256 matches is already there, and returns its path. The embedded
// bytes are checked against the embedded checksum first, the new file is
// written to a temp file in the same directory, verified, made 0755 and
// renamed into place, so a reader never sees a partial or unverified file. A
// cached file that fails verification is replaced. After success, directories
// of OTHER versions under <cacheRoot>/gocode/ugrep are removed (best effort).
func Extract(b *Bundle, cacheRoot string) (string, error) {
	if b == nil || len(b.Data) == 0 {
		return "", errors.New("no bundled ugrep")
	}
	if !versionRe.MatchString(b.Version) {
		return "", fmt.Errorf("bad bundled ugrep version %q", b.Version)
	}
	want := strings.ToLower(strings.TrimSpace(b.SHA256))
	if sum := sha256.Sum256(b.Data); hex.EncodeToString(sum[:]) != want {
		return "", errors.New("embedded ugrep does not match its embedded sha256 (corrupt build)")
	}
	if cacheRoot == "" {
		return "", errors.New("no user cache directory")
	}
	name := b.Name
	if name == "" {
		name = ExeName()
	}
	base := filepath.Join(cacheRoot, "gocode", "ugrep")
	dir := filepath.Join(base, b.Version)
	dst := filepath.Join(dir, name)

	extractMu.Lock()
	defer extractMu.Unlock()
	if fileSHA256(dst) == want {
		_ = ensureExecutable(dst)
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := writeVerified(dir, dst, b.Data, want); err != nil {
			return "", err
		}
	}
	CleanupOtherVersions(base, b.Version)
	for _, stale := range staleCacheDirs {
		_ = os.RemoveAll(filepath.Join(cacheRoot, "gocode", stale))
	}
	return dst, nil
}

func writeVerified(dir, dst string, data []byte, want string) error {
	tmp, err := os.CreateTemp(dir, ".ugrep-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if got := fileSHA256(tmpName); got != want {
		return fmt.Errorf("extracted ugrep failed verification (sha256 %s, want %s)", got, want)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Windows cannot replace a binary that is running; a concurrent
		// gocode may have just put a good copy there.
		if fileSHA256(dst) == want {
			return nil
		}
		return err
	}
	ok = true
	return nil
}

func ensureExecutable(p string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o111 == 0o111 {
		return nil
	}
	return os.Chmod(p, 0o755)
}

// fileSHA256 returns the hex sha256 of a regular file, or "" when it cannot be
// read (missing, a directory, a symlink).
func fileSHA256(p string) string {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CleanupOtherVersions removes every entry under base except keep. It is best
// effort: errors (e.g. a running ugrep.exe on Windows) are ignored.
func CleanupOtherVersions(base, keep string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// ProbeVersion runs `<path> --version` (5s timeout) and returns the version
// number from its "ugrep X.Y.Z ..." first line.
func ProbeVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	s := strings.TrimSpace(string(line))
	f := strings.Fields(s)
	if len(f) < 2 || f[0] != "ugrep" {
		return "", fmt.Errorf("unexpected --version output %q", s)
	}
	return f[1], nil
}
