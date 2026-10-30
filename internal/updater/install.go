package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// syscallEBUSY / syscallEPERM are the errno sentinels an atomic rename may fail
// with when the destination executable is running (held open). errors.Is sees
// these through the *os.PathError / *os.LinkError the rename returns.
var (
	syscallEBUSY = syscall.EBUSY
	syscallEPERM = syscall.EPERM
)

// -----------------------------------------------------------------------------
// Download + checksum verification.
// -----------------------------------------------------------------------------

// downloadTimeout bounds one asset download.
const downloadTimeout = 10 * time.Minute

// ChecksumMismatchError reports that a downloaded asset's SHA-256 did not match
// its checksums.txt entry. The partial download has already been removed.
type ChecksumMismatchError struct {
	Asset    string
	Expected string
	Got      string
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("updater: checksum mismatch for %s: expected %s, got %s; refusing to install", e.Asset, e.Expected, e.Got)
}

// MissingChecksumError reports that checksums.txt has no entry for the asset.
type MissingChecksumError struct{ Asset string }

func (e *MissingChecksumError) Error() string {
	return fmt.Sprintf("updater: %s has no entry for %s; refusing to install", ChecksumsAssetName, e.Asset)
}

// StagedChecksumMismatchError reports that a staged binary no longer matches the
// verified checksum at the moment of install, so the install is refused.
type StagedChecksumMismatchError struct{}

func (e *StagedChecksumMismatchError) Error() string {
	return "updater: staged binary no longer matches the verified checksum; refusing to install"
}

// downloadAsset streams asset into destPath, refusing more than asset.Size
// bytes when the size is known, and returns the lowercase hex SHA-256 of what
// was written. On any failure the partial file is removed.
//
// With a token configured and an API asset URL available, the API endpoint is
// used (Accept: application/octet-stream), which also works for private
// repositories; otherwise the public browser_download_url is fetched without
// credentials.
func (c *Client) downloadAsset(parent context.Context, asset Asset, destPath string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, downloadTimeout)
	defer cancel()

	u, accept := asset.BrowserDownloadURL, "application/octet-stream"
	if c.Token != "" && asset.URL != "" && c.isAPIURL(asset.URL) {
		u = asset.URL
	}
	if u == "" {
		return "", fmt.Errorf("updater: asset %s has no download URL", asset.Name)
	}
	if c.requireHTTPS() && !strings.HasPrefix(strings.ToLower(u), "https://") {
		return "", fmt.Errorf("updater: refusing non-HTTPS download URL for %s", asset.Name)
	}
	req, err := c.newRequest(ctx, u, accept)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("updater: downloading %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("updater: unexpected status %d downloading %s", resp.StatusCode, asset.Name)
	}
	if c.requireHTTPS() && resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.Scheme != "https" {
		// A redirect downgraded the transfer to plain HTTP: the binary and
		// its checksums.txt could both be swapped in transit.
		return "", fmt.Errorf("updater: refusing %s: download was redirected to a non-HTTPS URL", asset.Name)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var src io.Reader = resp.Body
	if asset.Size > 0 {
		src = io.LimitReader(resp.Body, asset.Size+1)
	}
	n, copyErr := io.Copy(io.MultiWriter(f, h), src)
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		_ = os.Remove(destPath)
		return "", fmt.Errorf("updater: downloading %s: %w", asset.Name, copyErr)
	case closeErr != nil:
		_ = os.Remove(destPath)
		return "", closeErr
	case asset.Size > 0 && n != asset.Size:
		_ = os.Remove(destPath)
		return "", fmt.Errorf("updater: %s size mismatch: release lists %d bytes, received %d", asset.Name, asset.Size, n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DownloadVerified downloads asset into destPath and verifies its SHA-256
// against sums (parsed checksums.txt). A missing entry or a mismatch removes
// the download and returns MissingChecksumError / ChecksumMismatchError. The
// verified checksum is returned.
func (c *Client) DownloadVerified(ctx context.Context, asset Asset, sums map[string]string, destPath string) (string, error) {
	want, ok := sums[asset.Name]
	if !ok {
		return "", &MissingChecksumError{Asset: asset.Name}
	}
	got, err := c.downloadAsset(ctx, asset, destPath)
	if err != nil {
		return "", err
	}
	if got != want {
		_ = os.Remove(destPath)
		return "", &ChecksumMismatchError{Asset: asset.Name, Expected: want, Got: got}
	}
	return got, nil
}

// FileChecksum returns the lowercase hex SHA-256 of the file at path.
func FileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// -----------------------------------------------------------------------------
// Atomic install.
// -----------------------------------------------------------------------------

// atomicRetryDelays is the backoff schedule for retrying an EBUSY/EPERM rename.
var atomicRetryDelays = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2000 * time.Millisecond}

// AtomicReplace installs srcPath at destPath atomically: copy to a unique temp
// file beside the destination, chmod it executable, re-verify its checksum
// against expectedChecksum (when supplied), then rename it over the
// destination. A rename failing with EBUSY/EPERM (a running executable held
// open) is retried on the atomicRetryDelays backoff schedule.
//
// An empty expectedChecksum skips the staged re-verification (the caller
// already trusts the source).
func AtomicReplace(srcPath, destPath, expectedChecksum string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	var lastErr error
	for attempt := 1; attempt <= len(atomicRetryDelays)+1; attempt++ {
		tmp := fmt.Sprintf("%s.tmp.%d.%d.%d", destPath, os.Getpid(), time.Now().UnixNano(), attempt)
		err := func() error {
			if err := copyFile(srcPath, tmp); err != nil {
				return err
			}
			if err := os.Chmod(tmp, 0o755); err != nil {
				return err
			}
			if expectedChecksum != "" {
				got, err := FileChecksum(tmp)
				if err != nil {
					return err
				}
				if got != expectedChecksum {
					return &StagedChecksumMismatchError{}
				}
			}
			return os.Rename(tmp, destPath)
		}()
		if err == nil {
			return nil
		}
		lastErr = err
		_ = os.Remove(tmp)

		// A staged checksum mismatch is deterministic; do not retry it.
		var mism *StagedChecksumMismatchError
		if errors.As(err, &mism) {
			return err
		}
		delay, retry := atomicRetryFor(err, attempt)
		if !retry {
			return err
		}
		time.Sleep(delay)
	}
	return lastErr
}

// atomicRetryFor reports whether err is a retryable locked-file error and, if
// so, how long to wait before the next attempt.
func atomicRetryFor(err error, attempt int) (time.Duration, bool) {
	if attempt-1 >= len(atomicRetryDelays) {
		return 0, false
	}
	if !errors.Is(err, syscallEBUSY) && !errors.Is(err, syscallEPERM) {
		return 0, false
	}
	return atomicRetryDelays[attempt-1], true
}

// copyFile copies src to dst, truncating dst, preserving no metadata beyond the
// 0755 mode the caller later enforces.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// -----------------------------------------------------------------------------
// Install-target path resolution.
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ExecutablePath returns the running binary's path with symlinks resolved —
// the file a self-update replaces.
func ExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("updater: cannot locate the running executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// replaceExecutable installs srcPath over destPath via AtomicReplace. On
// Windows a running .exe cannot be overwritten but can be renamed, so the
// current binary is first moved aside to "<dest>.old" (any stale .old is
// removed first) and restored if the replace fails.
func replaceExecutable(goos, srcPath, destPath, checksum string) error {
	if goos != "windows" {
		return AtomicReplace(srcPath, destPath, checksum)
	}
	old := destPath + ".old"
	_ = os.Remove(old)
	movedAside := false
	if _, err := os.Stat(destPath); err == nil {
		if err := os.Rename(destPath, old); err != nil {
			return fmt.Errorf("updater: moving the running executable aside: %w", err)
		}
		movedAside = true
	}
	if err := AtomicReplace(srcPath, destPath, checksum); err != nil {
		if movedAside {
			_ = os.Rename(old, destPath)
		}
		return err
	}
	return nil
}

// requireHTTPS reports whether downloads must stay on HTTPS: always, unless the
// API base itself was explicitly pointed at a plain-HTTP (test/mirror) server.
func (c *Client) requireHTTPS() bool {
	return strings.HasPrefix(strings.ToLower(c.apiBase()), "https://")
}
