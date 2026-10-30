package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// ChecksumsAssetName is the release asset holding SHA-256 sums for every other
// asset, in sha256sum / goreleaser format.
const ChecksumsAssetName = "checksums.txt"

// ArchiveKind classifies a platform asset.
type ArchiveKind int

const (
	ArchiveNone  ArchiveKind = iota // raw executable
	ArchiveTarGz                    // .tar.gz containing the binary
	ArchiveZip                      // .zip containing the binary
)

// maxBinaryBytes bounds a binary extracted from an archive (zip-bomb guard).
const maxBinaryBytes = 1 << 30

// maxChecksumsBytes bounds checksums.txt.
const maxChecksumsBytes = 1 << 20

// BinaryName returns the executable file name for goos: "gocode.exe" on
// Windows, "gocode" elsewhere. Archives must contain a file with this name.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "gocode.exe"
	}
	return "gocode"
}

// AssetBaseName returns "gocode_<goos>_<goarch>", the platform asset stem.
func AssetBaseName(goos, goarch string) string {
	return "gocode_" + goos + "_" + goarch
}

// SelectAsset picks the platform asset for goos/goarch from a release, trying
// in order: the raw binary (gocode_<goos>_<goarch>, plus ".exe" on Windows),
// then <stem>.tar.gz, then <stem>.zip.
func SelectAsset(assets []Asset, goos, goarch string) (Asset, ArchiveKind, error) {
	stem := AssetBaseName(goos, goarch)
	raw := stem
	if goos == "windows" {
		raw += ".exe"
	}
	wanted := []struct {
		name string
		kind ArchiveKind
	}{
		{raw, ArchiveNone},
		{stem + ".tar.gz", ArchiveTarGz},
		{stem + ".zip", ArchiveZip},
	}
	for _, w := range wanted {
		for _, a := range assets {
			if a.Name == w.name {
				return a, w.kind, nil
			}
		}
	}
	return Asset{}, ArchiveNone, fmt.Errorf("updater: release has no asset for %s/%s (expected one of %s, %s.tar.gz, %s.zip)",
		goos, goarch, raw, stem, stem)
}

// findAsset returns the asset named name.
func findAsset(assets []Asset, name string) (Asset, bool) {
	for _, a := range assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ParseChecksums parses sha256sum / goreleaser output: one "<hex>  <name>"
// per line (a "*" binary-mode marker before the name is accepted). Blank lines
// are skipped; any malformed line is an error.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !isHex64(fields[0]) {
			return nil, fmt.Errorf("updater: malformed checksums line %d", line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrUnsafeArchivePath reports an archive entry whose path would escape the
// extraction root (absolute path or ".." component — "zip slip").
var ErrUnsafeArchivePath = errors.New("updater: archive contains an unsafe path")

// safeEntryName validates an archive entry name and returns it cleaned.
func safeEntryName(name string) (string, error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if n == "" || strings.HasPrefix(n, "/") || (len(n) >= 2 && n[1] == ':') {
		return "", fmt.Errorf("%w: %q", ErrUnsafeArchivePath, name)
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrUnsafeArchivePath, name)
		}
	}
	return path.Clean(n), nil
}

// ExtractBinary extracts the single file named binName (at any directory depth)
// from the archive at archivePath into destPath (mode 0755). Every entry name
// is validated first: an absolute or ".."-containing path rejects the whole
// archive. Only a regular file is accepted as the binary; nothing else is
// written to disk.
func ExtractBinary(archivePath string, kind ArchiveKind, binName, destPath string) error {
	switch kind {
	case ArchiveTarGz:
		return extractTarGz(archivePath, binName, destPath)
	case ArchiveZip:
		return extractZip(archivePath, binName, destPath)
	default:
		return fmt.Errorf("updater: not an archive")
	}
}

func extractTarGz(archivePath, binName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("updater: invalid .tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("updater: invalid .tar.gz: %w", err)
		}
		name, err := safeEntryName(hdr.Name)
		if err != nil {
			_ = os.Remove(destPath)
			return err
		}
		if path.Base(name) != binName {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			_ = os.Remove(destPath)
			return fmt.Errorf("updater: archive entry %q is not a regular file", hdr.Name)
		}
		if found {
			_ = os.Remove(destPath)
			return fmt.Errorf("updater: archive contains more than one %s", binName)
		}
		if err := writeBinary(tr, destPath); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("updater: archive does not contain %s", binName)
	}
	return nil
}

func extractZip(archivePath, binName, destPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("updater: invalid .zip: %w", err)
	}
	defer zr.Close()
	var target *zip.File
	for _, zf := range zr.File {
		name, err := safeEntryName(zf.Name)
		if err != nil {
			return err
		}
		if path.Base(name) != binName {
			continue
		}
		if !zf.Mode().IsRegular() {
			return fmt.Errorf("updater: archive entry %q is not a regular file", zf.Name)
		}
		if target != nil {
			return fmt.Errorf("updater: archive contains more than one %s", binName)
		}
		target = zf
	}
	if target == nil {
		return fmt.Errorf("updater: archive does not contain %s", binName)
	}
	rc, err := target.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeBinary(rc, destPath)
}

// writeBinary copies at most maxBinaryBytes from r into destPath (0755),
// removing the file on any failure.
func writeBinary(r io.Reader, destPath string) error {
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(r, maxBinaryBytes+1))
	closeErr := out.Close()
	switch {
	case copyErr != nil:
		_ = os.Remove(destPath)
		return copyErr
	case closeErr != nil:
		_ = os.Remove(destPath)
		return closeErr
	case n > maxBinaryBytes:
		_ = os.Remove(destPath)
		return fmt.Errorf("updater: extracted binary exceeds %d bytes", int64(maxBinaryBytes))
	}
	return nil
}
