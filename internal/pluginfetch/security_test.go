package pluginfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/plugins"
)

// A tiny archive that inflates far beyond the extraction budget must be
// refused (decompression bomb), and leave no partial tree behind.
func TestExtractRejectsDecompressionBomb(t *testing.T) {
	old := maxExtractedBytes
	maxExtractedBytes = 1 << 20 // 1 MiB budget for the test
	defer func() { maxExtractedBytes = old }()

	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	size := int64(8 << 20)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	dest := filepath.Join(t.TempDir(), "out")
	if err := extractArchive(buf.Bytes(), dest); err == nil {
		t.Fatalf("8 MiB entry extracted despite 1 MiB budget")
	}
	if fi, err := os.Stat(filepath.Join(dest, "big.bin")); err == nil && fi.Size() > maxExtractedBytes {
		t.Fatalf("wrote %d bytes past the budget", fi.Size())
	}
}

func TestExtractRejectsTooManyEntries(t *testing.T) {
	old := maxExtractedEntries
	maxExtractedEntries = 10
	defer func() { maxExtractedEntries = old }()
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[strings.Repeat("a", i+1)] = "x"
	}
	if err := extractArchive(buildTarGz(t, files), filepath.Join(t.TempDir(), "o")); err == nil {
		t.Fatal("archive with 20 entries extracted despite a 10-entry cap")
	}
}

// A git-subdir path must stay inside the clone: "../.." or an absolute path
// would make the "plugin" an arbitrary local directory.
func TestGitSubdirRejectsEscapingPath(t *testing.T) {
	for _, p := range []string{"../../..", "/etc", "a/../../x"} {
		f := &Fetcher{Git: &fakeGit{}}
		dest := filepath.Join(t.TempDir(), "clone")
		got, err := f.FetchContext(context.Background(), plugins.Source{Kind: plugins.SourceGitSubdir, URL: "https://host/x.git", Path: p}, dest)
		if err == nil {
			t.Errorf("subdir %q accepted -> %s", p, got)
		}
	}
}

// symlinkGit clones a tree whose "plugin" subdir is a symlink out of the clone.
type symlinkGit struct{ target string }

func (g *symlinkGit) Run(ctx context.Context, cwd string, args []string) (GitResult, error) {
	if args[0] == "clone" {
		dest := args[len(args)-1]
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return GitResult{}, err
		}
		if err := os.Symlink(g.target, filepath.Join(dest, "plugin")); err != nil {
			return GitResult{}, err
		}
	}
	return GitResult{Code: 0}, nil
}

func TestGitSubdirRejectsSymlinkOutOfClone(t *testing.T) {
	outside := t.TempDir()
	f := &Fetcher{Git: &symlinkGit{target: outside}}
	dest := filepath.Join(t.TempDir(), "clone")
	got, err := f.FetchContext(context.Background(), plugins.Source{Kind: plugins.SourceGitSubdir, URL: "https://host/x.git", Path: "plugin"}, dest)
	if err == nil {
		t.Fatalf("symlinked subdir accepted -> %s", got)
	}
}

// Git transport-helper syntax ("ext::<cmd>", "fd::") runs arbitrary commands
// when the user's git config permits it; option-looking URLs are refused too.
func TestGitCloneURLRejectsDangerousTransports(t *testing.T) {
	for _, u := range []string{"ext::sh -c touch% /tmp/pwned", "fd::17", "--upload-pack=touch /tmp/x", "-oProxyCommand=x", "transport::https://h/x.git"} {
		g := &fakeGit{}
		f := &Fetcher{Git: g}
		_, err := f.FetchContext(context.Background(), plugins.Source{Kind: plugins.SourceGit, URL: u}, filepath.Join(t.TempDir(), "c"))
		if err == nil {
			t.Errorf("git URL %q accepted", u)
		}
		if len(g.calls) != 0 {
			t.Errorf("git invoked for %q: %v", u, g.calls)
		}
	}
	// ordinary URLs still work
	for _, u := range []string{"https://github.com/a/b.git", "git@github.com:a/b.git", "ssh://git@h/a.git"} {
		f := &Fetcher{Git: &fakeGit{}}
		if _, err := f.FetchContext(context.Background(), plugins.Source{Kind: plugins.SourceGit, URL: u}, filepath.Join(t.TempDir(), "c")); err != nil {
			t.Errorf("git URL %q refused: %v", u, err)
		}
	}
}
