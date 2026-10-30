package pluginfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/plugins"
)

// fetchArchive downloads an archive source, verifies its sha256 against the pin
// (rejecting a mismatch), and extracts the tar.gz/zip into dest. On any
// failure after the directory is created, the partial dest is removed.
func (f *Fetcher) fetchArchive(ctx context.Context, src plugins.Source, dest string) (string, error) {
	if strings.TrimSpace(src.URL) == "" {
		return "", fmt.Errorf("pluginfetch: archive source requires a URL")
	}
	data, err := f.download(ctx, src.URL, src.Headers, strings.TrimSpace(src.SHA256) != "")
	if err != nil {
		return "", err
	}
	if err := verifySHA256(data, src.SHA256, src.URL); err != nil {
		return "", err
	}
	if err := mkdirAll(dest); err != nil {
		return "", err
	}
	if err := extractArchive(data, dest); err != nil {
		// Delete the partial extraction so a failed install leaves no tree.
		_ = os.RemoveAll(dest)
		return "", err
	}
	return dest, nil
}

// fetchURL downloads a url marketplace source, verifies its manifest sha256
// against the pin (the manifestSha256 check), and writes it as marketplace.json
// under dest. On a mismatch the partial file is removed.
func (f *Fetcher) fetchURL(ctx context.Context, src plugins.Source, dest string) (string, error) {
	if strings.TrimSpace(src.URL) == "" {
		return "", fmt.Errorf("pluginfetch: url source requires a URL")
	}
	data, err := f.download(ctx, src.URL, src.Headers, strings.TrimSpace(src.SHA256) != "")
	if err != nil {
		return "", err
	}
	if err := verifySHA256(data, src.SHA256, src.URL); err != nil {
		return "", err
	}
	if err := mkdirAll(dest); err != nil {
		return "", err
	}
	out := filepath.Join(dest, plugins.MarketplaceFileName)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("pluginfetch: write %s: %w", out, err)
	}
	return dest, nil
}

// verifySHA256 compares the sha256 of data against want (hex, case-insensitive).
// An empty want skips the check. A mismatch is the archive/url integrity
// rejection.
func verifySHA256(data []byte, want, src string) error {
	if strings.TrimSpace(want) == "" {
		return nil
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, strings.TrimSpace(want)) {
		return fmt.Errorf("pluginfetch: integrity check failed for %s: expected sha256 %s, got %s; not installed", src, strings.ToLower(strings.TrimSpace(want)), got)
	}
	return nil
}

// extractArchive detects the container (gzip/tar.gz or zip) by magic bytes and
// extracts it into dest.
func extractArchive(data []byte, dest string) error {
	b := &extractBudget{bytes: maxExtractedBytes, entries: maxExtractedEntries}
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		return extractTarGz(data, dest, b)
	case len(data) >= 4 && data[0] == 'P' && data[1] == 'K' && (data[2] == 0x03 || data[2] == 0x05 || data[2] == 0x07):
		return extractZip(data, dest, b)
	default:
		return fmt.Errorf("pluginfetch: unrecognized archive format (not gzip or zip)")
	}
}

// extractTarGz extracts a gzip-compressed tar into dest, rejecting path
// traversal ("tar slip").
func extractTarGz(data []byte, dest string, b *extractBudget) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("pluginfetch: open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	wrote := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("pluginfetch: read tar: %w", err)
		}
		if err := b.entry(); err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("pluginfetch: mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("pluginfetch: mkdir %s: %w", filepath.Dir(target), err)
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			if err := writeFile(target, tr, mode, b); err != nil {
				return err
			}
			wrote++
		default:
			// Skip symlinks, devices, etc.: only directories and regular
			// files are materialized.
		}
	}
	if wrote == 0 {
		return fmt.Errorf("pluginfetch: archive contained no files")
	}
	return nil
}

// extractZip extracts a zip into dest, rejecting path traversal ("zip slip").
func extractZip(data []byte, dest string, b *extractBudget) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("pluginfetch: open zip: %w", err)
	}
	wrote := 0
	for _, entry := range zr.File {
		if err := b.entry(); err != nil {
			return err
		}
		target, err := safeJoin(dest, entry.Name)
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name, "/") || entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("pluginfetch: mkdir %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("pluginfetch: mkdir %s: %w", filepath.Dir(target), err)
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("pluginfetch: open zip entry %s: %w", entry.Name, err)
		}
		mode := entry.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		err = writeFile(target, rc, mode, b)
		rc.Close()
		if err != nil {
			return err
		}
		wrote++
	}
	if wrote == 0 {
		return fmt.Errorf("pluginfetch: archive contained no files")
	}
	return nil
}

// Extraction limits: the download is capped, but a compressed archive can
// inflate by orders of magnitude (a "zip bomb"), so the extracted total and the
// entry count are bounded too. Variables so tests can shrink them.
var (
	maxExtractedBytes   int64 = 1 << 30 // 1 GiB
	maxExtractedEntries       = 100000
)

type extractBudget struct {
	bytes   int64
	entries int
}

func (b *extractBudget) entry() error {
	b.entries--
	if b.entries < 0 {
		return fmt.Errorf("pluginfetch: archive has more than %d entries; refusing to extract", maxExtractedEntries)
	}
	return nil
}

func writeFile(target string, r io.Reader, mode os.FileMode, b *extractBudget) error {
	// O_EXCL|O_NOFOLLOW-equivalent: never write through a pre-existing entry
	// (a duplicate archive member or a planted symlink).
	if fi, err := os.Lstat(target); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("pluginfetch: archive entry %s collides with a non-regular file", target)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("pluginfetch: create %s: %w", target, err)
	}
	n, err := io.Copy(out, io.LimitReader(r, b.bytes+1))
	b.bytes -= n
	if err == nil && b.bytes < 0 {
		err = fmt.Errorf("archive expands beyond %d bytes; refusing to extract", maxExtractedBytes)
	}
	if err != nil {
		out.Close()
		_ = os.Remove(target)
		return fmt.Errorf("pluginfetch: write %s: %w", target, err)
	}
	return out.Close()
}

// safeJoin joins name onto dest and rejects any path that escapes dest, guarding
// against archive path-traversal.
func safeJoin(dest, name string) (string, error) {
	slashed := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(slashed, "/") {
		return "", fmt.Errorf("pluginfetch: archive entry %q is an absolute path", name)
	}
	target := filepath.Join(dest, filepath.FromSlash(slashed))
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("pluginfetch: archive entry %q escapes the destination directory", name)
	}
	return target, nil
}
