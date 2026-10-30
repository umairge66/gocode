package updater

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// CurrentVersion is the version of this build. It is "dev" in source; release
// builds stamp it with
//
//	go build -ldflags "-X github.com/umairge66/gocode/internal/updater.CurrentVersion=1.2.3"
//
// (a leading "v" is tolerated). While it is "dev" or otherwise not a semantic
// version, the updater cannot compare versions and only reinstalls on --force.
var CurrentVersion = "dev"

// DevBuildReason is the reason reported for an unversioned build.
const DevBuildReason = "development build; cannot compare versions"

// Updater checks the configured GitHub repository for a newer release and
// installs it over the running executable.
//
// All network access is through Client (an injectable Doer), so an Updater can
// be driven entirely from fakes in tests.
type Updater struct {
	Client *Client

	// Current is the running version; defaults to CurrentVersion.
	Current string
	// Executable is the file to replace; defaults to ExecutablePath().
	Executable string
	// StagingDir is the parent for the per-run temporary download directory;
	// empty uses os.TempDir().
	StagingDir string
	// GOOS / GOARCH select the platform asset; default runtime values.
	GOOS, GOARCH string
	// MaxVersion, when set, pins updates to at most this version.
	MaxVersion string
	// Restart, when non-nil, hands control to the freshly installed binary
	// after a successful replace (on unix a successful restart does not
	// return). Left nil, Apply returns normally.
	Restart *Restarter
	// TrustedKeys, when non-nil, replaces the compiled-in ReleasePublicKeys
	// (tests). A non-nil empty slice means "no key" and refuses to apply.
	TrustedKeys []releasesig.PublicKey
	// VerifiedKey is set by a successful Apply to the key that signed the
	// installed release.
	VerifiedKey releasesig.PublicKey
}

func (u *Updater) current() string {
	if u.Current != "" {
		return u.Current
	}
	return CurrentVersion
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

// CheckOptions selects what to check for.
type CheckOptions struct {
	// Channel is "stable" (default) or "latest" (prereleases included).
	Channel string
	// Tag, when set, targets that exact release instead of a channel (the
	// `install <version>` path). Downgrades are allowed for an explicit tag.
	Tag string
	// Force marks the release as available even when it is not newer (or the
	// running build is unversioned).
	Force bool
}

// CheckResult is the outcome of Check.
type CheckResult struct {
	// Current is the running version (normalized when it parses; may be "dev").
	Current string
	// Latest is the normalized version of Release.
	Latest string
	// Release is the resolved release.
	Release *Release
	// Available reports whether Apply should be called.
	Available bool
	// DevBuild reports that the running version is not a semantic version.
	DevBuild bool
	// Reason explains the decision.
	Reason string
}

// Check resolves the target release and decides whether to install it. It
// performs only GitHub API metadata requests — no asset downloads.
func (u *Updater) Check(ctx context.Context, opts CheckOptions) (CheckResult, error) {
	if u.Client == nil {
		return CheckResult{}, fmt.Errorf("updater: no client configured")
	}
	var rel *Release
	var err error
	if opts.Tag != "" {
		rel, err = u.Client.ReleaseByTag(ctx, opts.Tag)
	} else {
		rel, err = u.Client.LatestRelease(ctx, opts.Channel)
	}
	if err != nil {
		return CheckResult{}, err
	}
	latest, ok := rel.Version()
	if !ok {
		return CheckResult{}, fmt.Errorf("updater: release tag %q is not a semantic version", rel.TagName)
	}

	cur := u.current()
	res := CheckResult{Current: cur, Latest: latest, Release: rel}

	curV, curOK := ParseVersion(cur)
	if !curOK {
		res.DevBuild = true
		if opts.Force {
			res.Available, res.Reason = true, fmt.Sprintf("force install of %s over development build", latest)
		} else {
			res.Reason = DevBuildReason
		}
		return res, nil
	}
	curNorm := curV.Normalized()
	res.Current = curNorm

	// Honour a version pin: a channel release above MaxVersion is replaced by
	// the pinned release (when the current build is still below it).
	if opts.Tag == "" && u.MaxVersion != "" && GreaterThan(latest, u.MaxVersion) {
		if AtLeast(curNorm, u.MaxVersion) && !opts.Force {
			res.Reason = fmt.Sprintf("current %s is at or above maxVersion %s", curNorm, u.MaxVersion)
			return res, nil
		}
		pinned, err := u.Client.ReleaseByTag(ctx, u.MaxVersion)
		if err != nil {
			return CheckResult{}, err
		}
		if v, ok := pinned.Version(); ok {
			res.Release, res.Latest = pinned, v
			latest = v
		}
	}

	res.Available, res.Reason = DecideUpdate(curNorm, latest, DecideOptions{
		Force:            opts.Force,
		AlreadyInstalled: true,
		AllowDowngrade:   opts.Tag != "",
	})
	return res, nil
}

// DecideOptions tunes the update decision.
type DecideOptions struct {
	// Force installs even when the target is not newer.
	Force bool
	// AlreadyInstalled reports that current is installed and runnable; when
	// true and not forcing, an identical target is skipped.
	AlreadyInstalled bool
	// AllowDowngrade permits installing a target older than current (an
	// explicitly requested version). Otherwise an older target is skipped.
	AllowDowngrade bool
	// MaxVersion, when non-empty, caps the target: a target above it is pulled
	// down to MaxVersion, and if current is already at or above MaxVersion the
	// update is skipped.
	MaxVersion string
	// MinimumVersion, when non-empty, blocks installing a target below it.
	MinimumVersion string
}

// DecideUpdate reports whether installing target over current is warranted, and
// a short reason: an update is available when target > current; an identical
// installed target is skipped unless forcing; an older target only installs
// with AllowDowngrade or Force.
func DecideUpdate(current, target string, opts DecideOptions) (available bool, reason string) {
	effective := target

	if opts.MaxVersion != "" && GreaterThan(effective, opts.MaxVersion) {
		if AtLeast(current, opts.MaxVersion) {
			return false, fmt.Sprintf("current %s is at or above maxVersion %s", current, opts.MaxVersion)
		}
		effective = opts.MaxVersion
	}

	if opts.MinimumVersion != "" && LessThan(effective, opts.MinimumVersion) {
		return false, fmt.Sprintf("target %s is below minimumVersion %s", effective, opts.MinimumVersion)
	}

	c := cmpOrZero(effective, current)
	switch {
	case opts.Force:
		return true, fmt.Sprintf("force install of %s", effective)
	case c > 0:
		return true, fmt.Sprintf("upgrade %s -> %s", current, effective)
	case c < 0 && opts.AllowDowngrade:
		return true, fmt.Sprintf("downgrade %s -> %s", current, effective)
	case c < 0:
		return false, fmt.Sprintf("current %s is newer than %s", current, effective)
	case opts.AlreadyInstalled:
		return false, fmt.Sprintf("already at %s", effective)
	default:
		return true, fmt.Sprintf("reinstall %s", effective)
	}
}

// Apply downloads the platform asset of rel, verifies the release signature
// over checksums.txt against the trusted keys (mandatory; a build without a
// key refuses with ErrNoSigningKey), verifies the asset against the now
// trusted checksums.txt (mandatory), extracts the binary from an archive if
// needed, atomically replaces the executable, and — when Restart is set —
// hands control to the new binary. It returns the installed path.
func (u *Updater) Apply(ctx context.Context, rel *Release) (string, error) {
	if u.Client == nil || rel == nil {
		return "", fmt.Errorf("updater: nothing to apply")
	}
	keys, err := u.trustedKeys()
	if err != nil {
		return "", err
	}
	goos, goarch := u.goos(), u.goarch()
	asset, kind, err := SelectAsset(rel.Assets, goos, goarch)
	if err != nil {
		return "", err
	}
	exe := u.Executable
	if exe == "" {
		if exe, err = ExecutablePath(); err != nil {
			return "", err
		}
	}

	staging, err := os.MkdirTemp(u.StagingDir, "gocode-update-")
	if err != nil {
		return "", fmt.Errorf("updater: creating staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	sums, key, err := u.Client.fetchVerifiedChecksums(ctx, rel, staging, keys)
	if err != nil {
		cleanup()
		return "", err
	}
	u.VerifiedKey = key
	downloaded := filepath.Join(staging, filepath.Base(asset.Name))
	checksum, err := u.Client.DownloadVerified(ctx, asset, sums, downloaded)
	if err != nil {
		cleanup()
		return "", err
	}

	bin := downloaded
	if kind != ArchiveNone {
		bin = filepath.Join(staging, "extracted-"+BinaryName(goos))
		if err := ExtractBinary(downloaded, kind, BinaryName(goos), bin); err != nil {
			cleanup()
			return "", err
		}
		if checksum, err = FileChecksum(bin); err != nil {
			cleanup()
			return "", err
		}
	}

	if err := replaceExecutable(goos, bin, exe, checksum); err != nil {
		cleanup()
		return "", err
	}
	// Clean up before restarting: a successful unix exec never returns.
	cleanup()

	if u.Restart != nil {
		if err := u.Restart.Restart(exe); err != nil {
			return exe, fmt.Errorf("updater: installed %s but restart failed: %w", exe, err)
		}
	}
	return exe, nil
}
