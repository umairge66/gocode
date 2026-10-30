package pluginfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/plugins"
)

// --- fakes -------------------------------------------------------------------

// fakeDoer serves canned responses keyed by URL. It never touches the network.
type fakeDoer struct {
	responses map[string][]byte
	status    map[string]int
	calls     []string
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	d.calls = append(d.calls, url)
	body, ok := d.responses[url]
	if !ok {
		return nil, fmt.Errorf("fakeDoer: no canned response for %s", url)
	}
	code := 200
	if d.status != nil {
		if c, ok := d.status[url]; ok {
			code = c
		}
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// fakeGit records invocations and simulates clone/rev-parse against a temp dir.
// headSHA is what `rev-parse HEAD` reports after a checkout.
type fakeGit struct {
	headSHA   string
	failClone bool
	calls     [][]string
}

func (g *fakeGit) Run(ctx context.Context, cwd string, args []string) (GitResult, error) {
	g.calls = append(g.calls, args)
	switch args[0] {
	case "clone":
		if g.failClone {
			return GitResult{Code: 128, Stderr: "fatal: repository not found"}, nil
		}
		// The destination is the last argument; populate it so the clone
		// "materializes" a working tree.
		dest := args[len(args)-1]
		if err := os.MkdirAll(filepath.Join(dest, ".gocode-plugin"), 0o755); err != nil {
			return GitResult{}, err
		}
		if err := os.WriteFile(filepath.Join(dest, "plugin.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
			return GitResult{}, err
		}
		return GitResult{Code: 0}, nil
	case "rev-parse":
		return GitResult{Code: 0, Stdout: g.headSHA + "\n"}, nil
	default: // fetch, checkout, sparse-checkout, read-tree
		return GitResult{Code: 0}, nil
	}
}

// --- helpers -----------------------------------------------------------------

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// --- archive sha256: pass + mismatch ----------------------------------------

func TestFetchArchive_TarGz_SHA256Pass(t *testing.T) {
	data := buildTarGz(t, map[string]string{
		"plugin.json":       `{"name":"demo"}`,
		"commands/hello.md": "# hello",
	})
	url := "https://example.test/demo.tgz"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: data}}}
	dest := filepath.Join(t.TempDir(), "out")

	got, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: url, SHA256: sha256hex(data)}, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != dest {
		t.Fatalf("got dest %q, want %q", got, dest)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "plugin.json")); err != nil || string(b) != `{"name":"demo"}` {
		t.Fatalf("plugin.json = %q, err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "commands", "hello.md")); err != nil {
		t.Fatalf("nested file not extracted: %v", err)
	}
}

func TestFetchArchive_SHA256Mismatch_Rejects(t *testing.T) {
	data := buildTarGz(t, map[string]string{"plugin.json": `{"name":"demo"}`})
	url := "https://example.test/demo.tgz"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: data}}}
	dest := filepath.Join(t.TempDir(), "out")

	badPin := strings.Repeat("0", 64)
	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: url, SHA256: badPin}, dest)
	if err == nil {
		t.Fatal("expected integrity error, got nil")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("error = %v, want integrity failure", err)
	}
	// Partial dest must not survive.
	if _, statErr := os.Stat(filepath.Join(dest, "plugin.json")); !os.IsNotExist(statErr) {
		t.Fatalf("partial extraction left behind: %v", statErr)
	}
}

// --- zip extract -------------------------------------------------------------

func TestFetchArchive_Zip_Extract(t *testing.T) {
	data := buildZip(t, map[string]string{
		"plugin.json":     `{"name":"z"}`,
		"skills/SKILL.md": "skill",
	})
	url := "https://example.test/z.zip"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: data}}}
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceArchive, URL: url, SHA256: sha256hex(data)}, dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "SKILL.md")); err != nil {
		t.Fatalf("zip nested file not extracted: %v", err)
	}
}

func TestExtract_RejectsZipSlip(t *testing.T) {
	data := buildZip(t, map[string]string{"../evil.txt": "pwn"})
	dest := t.TempDir()
	if err := extractArchive(data, dest); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected zip-slip rejection, got %v", err)
	}
}

// --- url manifest ------------------------------------------------------------

func TestFetchURL_ManifestSHA256(t *testing.T) {
	manifest := []byte(`{"name":"mkt","owner":{"name":"o"},"plugins":[]}`)
	url := "https://example.test/marketplace.json"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: manifest}}}
	dest := filepath.Join(t.TempDir(), "mkt")

	got, err := f.Fetch(plugins.Source{Kind: plugins.SourceURL, URL: url, SHA256: sha256hex(manifest)}, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(got, plugins.MarketplaceFileName))
	if err != nil || !bytes.Equal(b, manifest) {
		t.Fatalf("manifest = %q, err=%v", b, err)
	}
}

func TestFetchURL_SHA256Mismatch_Rejects(t *testing.T) {
	manifest := []byte(`{"name":"mkt"}`)
	url := "https://example.test/marketplace.json"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: manifest}}}
	dest := filepath.Join(t.TempDir(), "mkt")

	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceURL, URL: url, SHA256: strings.Repeat("a", 64)}, dest)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, plugins.MarketplaceFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("partial manifest left behind: %v", statErr)
	}
}

// --- git clone + commit pin --------------------------------------------------

func TestFetchGit_GitHub_NoPin(t *testing.T) {
	g := &fakeGit{headSHA: "deadbeef"}
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "repo")

	got, err := f.Fetch(plugins.Source{Kind: plugins.SourceGitHub, Repo: "acme/widget", Ref: "main"}, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != dest {
		t.Fatalf("got %q want %q", got, dest)
	}
	// clone URL and --branch must be present.
	clone := g.calls[0]
	joined := strings.Join(clone, " ")
	if !strings.Contains(joined, "https://github.com/acme/widget.git") || !strings.Contains(joined, "--branch main") {
		t.Fatalf("clone args = %v", clone)
	}
	if !strings.Contains(joined, "--depth 1") {
		t.Fatalf("expected --depth 1, got %v", clone)
	}
}

func TestFetchGit_CommitPin_Match(t *testing.T) {
	pin := strings.Repeat("a", 40)
	g := &fakeGit{headSHA: pin}
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "repo")

	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceGit, URL: "https://git.test/r.git", Ref: pin}, dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Must have verified via rev-parse HEAD.
	var sawRevParse, sawCheckout bool
	for _, c := range g.calls {
		if c[0] == "rev-parse" {
			sawRevParse = true
		}
		if c[0] == "checkout" {
			sawCheckout = true
		}
	}
	if !sawRevParse || !sawCheckout {
		t.Fatalf("expected checkout + rev-parse, calls=%v", g.calls)
	}
}

func TestFetchGit_CommitPin_Mismatch_Rejects(t *testing.T) {
	pin := strings.Repeat("a", 40)
	g := &fakeGit{headSHA: strings.Repeat("b", 40)} // HEAD != pin
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "repo")

	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceGit, URL: "https://git.test/r.git", Ref: pin}, dest)
	if err == nil || !strings.Contains(err.Error(), "SHA pin verification failed") {
		t.Fatalf("expected pin mismatch rejection, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("mismatched clone not cleaned up: %v", statErr)
	}
}

func TestFetchGit_Subdir_SparseCheckout(t *testing.T) {
	g := &fakeGit{headSHA: "x"}
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "repo")

	got, err := f.Fetch(plugins.Source{Kind: plugins.SourceGitSubdir, URL: "https://git.test/r.git", Path: "packages/plugin"}, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := filepath.Join(dest, "packages/plugin"); got != want {
		t.Fatalf("subdir path = %q, want %q", got, want)
	}
	var sawSparse bool
	for _, c := range g.calls {
		if c[0] == "sparse-checkout" && strings.Join(c, " ") == "sparse-checkout set --cone -- packages/plugin" {
			sawSparse = true
		}
	}
	if !sawSparse {
		t.Fatalf("expected sparse-checkout set, calls=%v", g.calls)
	}
}

func TestFetchGit_CloneFailure_CleansUp(t *testing.T) {
	g := &fakeGit{failClone: true}
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "repo")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceGitHub, Repo: "a/b"}, dest); err == nil {
		t.Fatal("expected clone failure")
	}
}

// --- file / directory passthrough -------------------------------------------

func TestFetch_FilePassthrough(t *testing.T) {
	f := New() // seams unused for local sources
	src := plugins.Source{Kind: plugins.SourceFile, Path: "/some/local/marketplace.json"}
	got, err := f.Fetch(src, "/ignored/dest")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != src.Path {
		t.Fatalf("got %q want %q", got, src.Path)
	}
}

func TestFetch_DirectoryPassthrough(t *testing.T) {
	f := New()
	src := plugins.Source{Kind: plugins.SourceDirectory, Path: "/some/local/dir"}
	got, err := f.Fetch(src, "/ignored")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "/some/local/dir" {
		t.Fatalf("got %q", got)
	}
}

func TestFetch_BarePathPassthrough(t *testing.T) {
	f := New()
	src := plugins.Source{Path: "plugins/formatter"} // Kind=="" bare source
	got, err := f.Fetch(src, "/ignored")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "plugins/formatter" {
		t.Fatalf("got %q", got)
	}
}

// --- npm direct tarball ------------------------------------------------------

func TestFetchNPM_DirectTarball(t *testing.T) {
	data := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"n"}`})
	url := "https://registry.test/pkg/-/pkg-1.0.0.tgz"
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{url: data}}}
	dest := filepath.Join(t.TempDir(), "npm")

	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "pkg", Version: url, SHA256: sha256hex(data)}, dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "package", "plugin.json")); err != nil {
		t.Fatalf("npm tarball not extracted: %v", err)
	}
}

// --- npm registry resolution + integrity ------------------------------------

func sriSHA512(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

func sha1hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// packument builds a registry packument JSON with the given dist-tags and
// per-version dist metadata.
func packument(t *testing.T, name string, distTags map[string]string, versions map[string]map[string]string) []byte {
	t.Helper()
	vmap := map[string]any{}
	for ver, dist := range versions {
		vmap[ver] = map[string]any{
			"name":    name,
			"version": ver,
			"dist": map[string]any{
				"tarball":   dist["tarball"],
				"integrity": dist["integrity"],
				"shasum":    dist["shasum"],
			},
		}
	}
	b, err := json.Marshal(map[string]any{
		"name":      name,
		"dist-tags": distTags,
		"versions":  vmap,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchNPM_RegistryResolve_DistTag(t *testing.T) {
	tar100 := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a","v":"1.0.0"}`})
	tar200 := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a","v":"2.0.0"}`})
	tb100 := "https://registry.npmjs.org/a/-/a-1.0.0.tgz"
	tb200 := "https://registry.npmjs.org/a/-/a-2.0.0.tgz"
	pm := packument(t, "a",
		map[string]string{"latest": "2.0.0", "legacy": "1.0.0"},
		map[string]map[string]string{
			"1.0.0": {"tarball": tb100, "integrity": sriSHA512(tar100)},
			"2.0.0": {"tarball": tb200, "integrity": sriSHA512(tar200)},
		})
	doer := &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pm,
		tb100:                          tar100,
		tb200:                          tar200,
	}}
	f := &Fetcher{HTTP: doer}

	// Default spec → "latest" dist-tag → 2.0.0.
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a"}, dest); err != nil {
		t.Fatalf("Fetch latest: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "package", "plugin.json")); !strings.Contains(string(b), `"2.0.0"`) {
		t.Fatalf("latest resolved to wrong version: %q", b)
	}

	// Named dist-tag → 1.0.0.
	dest2 := filepath.Join(t.TempDir(), "out2")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Version: "legacy"}, dest2); err != nil {
		t.Fatalf("Fetch legacy: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest2, "package", "plugin.json")); !strings.Contains(string(b), `"1.0.0"`) {
		t.Fatalf("legacy resolved to wrong version: %q", b)
	}
}

func TestFetchNPM_RegistryResolve_ExplicitAndRange(t *testing.T) {
	t110 := buildTarGz(t, map[string]string{"package/p.json": `{"v":"1.1.0"}`})
	t120 := buildTarGz(t, map[string]string{"package/p.json": `{"v":"1.2.0"}`})
	t200 := buildTarGz(t, map[string]string{"package/p.json": `{"v":"2.0.0"}`})
	tb := func(v string) string { return "https://registry.npmjs.org/a/-/a-" + v + ".tgz" }
	pm := packument(t, "a",
		map[string]string{"latest": "2.0.0"},
		map[string]map[string]string{
			"1.1.0": {"tarball": tb("1.1.0"), "integrity": sriSHA512(t110)},
			"1.2.0": {"tarball": tb("1.2.0"), "integrity": sriSHA512(t120)},
			"2.0.0": {"tarball": tb("2.0.0"), "integrity": sriSHA512(t200)},
		})
	doer := &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pm,
		tb("1.1.0"):                    t110,
		tb("1.2.0"):                    t120,
		tb("2.0.0"):                    t200,
	}}
	f := &Fetcher{HTTP: doer}

	// Explicit version.
	dest := filepath.Join(t.TempDir(), "o")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Version: "1.1.0"}, dest); err != nil {
		t.Fatalf("Fetch explicit: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "package", "p.json")); !strings.Contains(string(b), `"1.1.0"`) {
		t.Fatalf("explicit resolved wrong: %q", b)
	}

	// Caret range picks highest satisfying (^1.1.0 → 1.2.0, not 2.0.0).
	dest2 := filepath.Join(t.TempDir(), "o2")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Version: "^1.1.0"}, dest2); err != nil {
		t.Fatalf("Fetch range: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest2, "package", "p.json")); !strings.Contains(string(b), `"1.2.0"`) {
		t.Fatalf("range resolved wrong: %q", b)
	}
}

func TestFetchNPM_SHA512Integrity_PassAndReject(t *testing.T) {
	good := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a"}`})
	tb := "https://registry.npmjs.org/a/-/a-1.0.0.tgz"

	// Pass: integrity matches the served tarball.
	pmGood := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "integrity": sriSHA512(good)}})
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pmGood, tb: good,
	}}}
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a"}, filepath.Join(t.TempDir(), "ok")); err != nil {
		t.Fatalf("expected integrity pass, got %v", err)
	}

	// Reject: integrity is for different bytes than the served tarball.
	pmBad := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "integrity": sriSHA512([]byte("other"))}})
	f2 := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pmBad, tb: good,
	}}}
	_, err := f2.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a"}, filepath.Join(t.TempDir(), "bad"))
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity rejection, got %v", err)
	}
}

func TestFetchNPM_Shasum_PassAndReject(t *testing.T) {
	good := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a"}`})
	tb := "https://registry.npmjs.org/a/-/a-1.0.0.tgz"

	// Pass: no integrity, shasum (hex sha1) matches.
	pmGood := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "shasum": sha1hex(good)}})
	f := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pmGood, tb: good,
	}}}
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a"}, filepath.Join(t.TempDir(), "ok")); err != nil {
		t.Fatalf("expected shasum pass, got %v", err)
	}

	// Reject: shasum does not match.
	pmBad := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "shasum": sha1hex([]byte("other"))}})
	f2 := &Fetcher{HTTP: &fakeDoer{responses: map[string][]byte{
		"https://registry.npmjs.org/a": pmBad, tb: good,
	}}}
	_, err := f2.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a"}, filepath.Join(t.TempDir(), "bad"))
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected shasum rejection, got %v", err)
	}
}

func TestFetchNPM_ScopedNameEncoding(t *testing.T) {
	data := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"@scope/p"}`})
	tb := "https://registry.npmjs.org/@scope/p/-/p-1.0.0.tgz"
	pm := packument(t, "@scope/p", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "integrity": sriSHA512(data)}})
	// The packument must be requested at the %2f-encoded path.
	encodedURL := "https://registry.npmjs.org/@scope%2fp"
	doer := &fakeDoer{responses: map[string][]byte{encodedURL: pm, tb: data}}
	f := &Fetcher{HTTP: doer}

	dest := filepath.Join(t.TempDir(), "scoped")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "@scope/p"}, dest); err != nil {
		t.Fatalf("Fetch scoped: %v", err)
	}
	// Confirm the encoded URL was the one actually requested.
	var sawEncoded bool
	for _, c := range doer.calls {
		if c == encodedURL {
			sawEncoded = true
		}
	}
	if !sawEncoded {
		t.Fatalf("scoped packument not requested at %q; calls=%v", encodedURL, doer.calls)
	}
}

func TestFetchNPM_CustomRegistry(t *testing.T) {
	data := buildTarGz(t, map[string]string{"package/plugin.json": `{"name":"a"}`})
	tb := "https://npm.internal.test/a/-/a-1.0.0.tgz"
	pm := packument(t, "a", map[string]string{"latest": "1.0.0"},
		map[string]map[string]string{"1.0.0": {"tarball": tb, "integrity": sriSHA512(data)}})
	doer := &fakeDoer{responses: map[string][]byte{
		"https://npm.internal.test/a": pm, tb: data,
	}}
	f := &Fetcher{HTTP: doer}
	dest := filepath.Join(t.TempDir(), "reg")
	if _, err := f.Fetch(plugins.Source{Kind: plugins.SourceNPM, Package: "a", Registry: "https://npm.internal.test"}, dest); err != nil {
		t.Fatalf("Fetch custom registry: %v", err)
	}
}

// --- unsupported -------------------------------------------------------------

func TestFetch_UnsupportedKind(t *testing.T) {
	f := New()
	_, err := f.Fetch(plugins.Source{Kind: plugins.SourceCommand, Command: "echo hi"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not fetchable") {
		t.Fatalf("expected not-fetchable error, got %v", err)
	}
}
