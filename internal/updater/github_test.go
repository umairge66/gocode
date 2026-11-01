package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// fakeGitHub is an httptest-backed GitHub: an API server for release metadata
// and a separate download server for asset bytes.
type fakeGitHub struct {
	api, dl *httptest.Server

	mu      sync.Mutex
	apiReqs []*http.Request
	dlReqs  []*http.Request
	latest  *Release
	list    []Release
	tags    map[string]*Release
	files   map[string][]byte
	apiCode int // forced status for every API request when non-zero
	apiHdr  http.Header
	apiBody string

	// sk signs every published checksums.txt (generated per test).
	sk releasesig.SecretKey
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tags: map[string]*Release{}, files: map[string][]byte{}}
	sk, err := releasesig.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.sk = sk
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.dl = httptest.NewServer(http.HandlerFunc(f.serveDL))
	t.Cleanup(func() { f.api.Close(); f.dl.Close() })
	return f
}

func (f *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.apiReqs = append(f.apiReqs, r.Clone(context.Background()))
	code, hdr, body := f.apiCode, f.apiHdr, f.apiBody
	f.mu.Unlock()
	if code != 0 {
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
		return
	}
	const prefix = "/repos/acme/tool"
	p := strings.TrimPrefix(r.URL.Path, prefix)
	var out any
	switch {
	case p == "/releases/latest" && f.latest != nil:
		out = f.latest
	case p == "/releases" && f.list != nil:
		out = f.list
	case strings.HasPrefix(p, "/releases/tags/") && f.tags[strings.TrimPrefix(p, "/releases/tags/")] != nil:
		out = f.tags[strings.TrimPrefix(p, "/releases/tags/")]
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGitHub) serveDL(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.dlReqs = append(f.dlReqs, r.Clone(context.Background()))
	b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

func (f *fakeGitHub) client(token string) *Client {
	return &Client{HTTP: f.api.Client(), Repo: "acme/tool", APIBaseURL: f.api.URL, Token: token}
}

// asset registers content for download and returns its Asset descriptor.
func (f *fakeGitHub) asset(name string, content []byte) Asset {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = content
	return Asset{Name: name, Size: int64(len(content)), BrowserDownloadURL: f.dl.URL + "/dl/" + name}
}

func (f *fakeGitHub) downloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.dlReqs {
		out = append(out, strings.TrimPrefix(r.URL.Path, "/dl/"))
	}
	return out
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func checksumsFor(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	for name, b := range entries {
		fmt.Fprintf(&buf, "%s  %s\n", sha(b), name)
	}
	return buf.Bytes()
}

// -----------------------------------------------------------------------------
// Release API
// -----------------------------------------------------------------------------

func TestLatestReleaseStableParse(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.latest = &Release{TagName: "v1.2.3", Assets: []Asset{{Name: "gocode_linux_amd64", Size: 4, BrowserDownloadURL: "x"}}}
	rel, err := gh.client("").LatestRelease(context.Background(), ChannelStable)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := rel.Version()
	if !ok || v != "1.2.3" {
		t.Fatalf("version = %q %v, want 1.2.3", v, ok)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Size != 4 {
		t.Fatalf("assets not parsed: %+v", rel.Assets)
	}
	r := gh.apiReqs[0]
	if r.URL.Path != "/repos/acme/tool/releases/latest" {
		t.Errorf("path = %s", r.URL.Path)
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" ||
		r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" ||
		r.Header.Get("User-Agent") != "gocode-updater" {
		t.Errorf("headers = %v", r.Header)
	}
}

func TestLatestChannelPicksHighestIncludingPrereleases(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.list = []Release{
		{TagName: "v1.2.3"},
		{TagName: "v2.0.0", Draft: true},
		{TagName: "v1.3.0-rc.2", Prerelease: true},
		{TagName: "v1.3.0-rc.10", Prerelease: true},
		{TagName: "nightly"},
		{TagName: "1.2.9"},
	}
	rel, err := gh.client("").LatestRelease(context.Background(), ChannelLatest)
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v1.3.0-rc.10" {
		t.Fatalf("picked %s, want v1.3.0-rc.10", rel.TagName)
	}
	if q := gh.apiReqs[0].URL.Query().Get("per_page"); q == "" {
		t.Errorf("per_page not set: %s", gh.apiReqs[0].URL)
	}
}

func TestTokenHeaderOnlyWhenSet(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.latest = &Release{TagName: "v1.0.0"}
	if _, err := gh.client("").LatestRelease(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if h := gh.apiReqs[0].Header.Get("Authorization"); h != "" {
		t.Errorf("Authorization sent without token: %q", h)
	}
	if _, err := gh.client("s3cret").LatestRelease(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if h := gh.apiReqs[1].Header.Get("Authorization"); h != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", h)
	}
	if got := TokenFromEnv(func(k string) string {
		if k == "GITHUB_TOKEN" {
			return " tok "
		}
		return ""
	}); got != "tok" {
		t.Errorf("TokenFromEnv = %q", got)
	}
}

func TestTokenNotSentToDownloadHost(t *testing.T) {
	gh := newFakeGitHub(t)
	a := gh.asset("checksums.txt", []byte("x"))
	if _, err := gh.client("s3cret").downloadAsset(context.Background(), a, filepath.Join(t.TempDir(), "c")); err != nil {
		t.Fatal(err)
	}
	if h := gh.dlReqs[0].Header.Get("Authorization"); h != "" {
		t.Errorf("token leaked to download host: %q", h)
	}
}

func TestRateLimitAndNotFoundErrors(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.apiCode = http.StatusForbidden
	gh.apiHdr = http.Header{"X-Ratelimit-Remaining": {"0"}}
	gh.apiBody = `{"message":"API rate limit exceeded"}`
	_, err := gh.client("").LatestRelease(context.Background(), "")
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("rate limit err = %v", err)
	}

	gh.apiCode, gh.apiHdr, gh.apiBody = http.StatusTooManyRequests, nil, ""
	if _, err := gh.client("").LatestRelease(context.Background(), ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429 err = %v", err)
	}

	gh.apiCode = http.StatusNotFound
	_, err = gh.client("").LatestRelease(context.Background(), "")
	if !errors.Is(err, ErrNoReleases) {
		t.Fatalf("404 err = %v", err)
	}
}

func TestReleaseByTagTriesVPrefix(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.tags["v1.0.0"] = &Release{TagName: "v1.0.0"}
	rel, err := gh.client("").ReleaseByTag(context.Background(), "1.0.0")
	if err != nil || rel.TagName != "v1.0.0" {
		t.Fatalf("rel=%v err=%v", rel, err)
	}
	gh.tags["2.0.0"] = &Release{TagName: "2.0.0"}
	rel, err = gh.client("").ReleaseByTag(context.Background(), "v2.0.0")
	if err != nil || rel.TagName != "2.0.0" {
		t.Fatalf("rel=%v err=%v", rel, err)
	}
	if _, err := gh.client("").ReleaseByTag(context.Background(), "9.9.9"); !errors.Is(err, ErrNoReleases) {
		t.Fatalf("missing tag err = %v", err)
	}
}

// -----------------------------------------------------------------------------
// Repository configuration
// -----------------------------------------------------------------------------

func TestResolveRepoPrecedence(t *testing.T) {
	old := DefaultRepo
	t.Cleanup(func() { DefaultRepo = old })
	DefaultRepo = "build/default"

	env := func(v string) func(string) string {
		return func(k string) string {
			if k == RepoEnvVar {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		env, settings, want string
		src                 RepoSource
	}{
		{"env/repo", "set/repo", "env/repo", RepoFromEnv},
		{"", "set/repo", "set/repo", RepoFromSettings},
		{"", "", "build/default", RepoFromBuild},
	}
	for _, c := range cases {
		got, src, err := ResolveRepo(env(c.env), c.settings)
		if err != nil || got != c.want || src != c.src {
			t.Errorf("ResolveRepo(%q,%q) = %q %s %v, want %q %s", c.env, c.settings, got, src, err, c.want, c.src)
		}
	}

	DefaultRepo = ""
	if _, _, err := ResolveRepo(env(""), ""); !errors.Is(err, ErrRepoNotConfigured) ||
		!strings.Contains(err.Error(), "set GOCODE_UPDATE_REPO=owner/repo") {
		t.Fatalf("unconfigured err = %v", err)
	}
	for _, bad := range []string{"owner", "owner/repo/extra", "/repo", "owner/", "own er/repo", "../x", "a/.."} {
		if _, _, err := ResolveRepo(env(bad), ""); err == nil {
			t.Errorf("invalid repo %q accepted", bad)
		}
	}
}

func TestUnconfiguredRepoMakesNoHTTPCalls(t *testing.T) {
	gh := newFakeGitHub(t)
	c := gh.client("")
	c.Repo = ""
	if _, err := c.LatestRelease(context.Background(), ""); !errors.Is(err, ErrRepoNotConfigured) {
		t.Fatalf("err = %v", err)
	}
	if len(gh.apiReqs) != 0 {
		t.Fatalf("made %d HTTP calls", len(gh.apiReqs))
	}
}

// -----------------------------------------------------------------------------
// Assets, checksums, archives
// -----------------------------------------------------------------------------

func TestSelectAsset(t *testing.T) {
	assets := []Asset{
		{Name: "checksums.txt"},
		{Name: "gocode_linux_amd64"},
		{Name: "gocode_darwin_arm64.tar.gz"},
		{Name: "gocode_windows_amd64.exe"},
		{Name: "gocode_windows_arm64.zip"},
		{Name: "gocode_linux_arm64.zip"},
		{Name: "gocode_linux_arm64.tar.gz"},
	}
	cases := []struct {
		goos, goarch, name string
		kind               ArchiveKind
	}{
		{"linux", "amd64", "gocode_linux_amd64", ArchiveNone},
		{"darwin", "arm64", "gocode_darwin_arm64.tar.gz", ArchiveTarGz},
		{"windows", "amd64", "gocode_windows_amd64.exe", ArchiveNone},
		{"windows", "arm64", "gocode_windows_arm64.zip", ArchiveZip},
		{"linux", "arm64", "gocode_linux_arm64.tar.gz", ArchiveTarGz},
	}
	for _, c := range cases {
		a, k, err := SelectAsset(assets, c.goos, c.goarch)
		if err != nil || a.Name != c.name || k != c.kind {
			t.Errorf("%s/%s → %q %v %v, want %q %v", c.goos, c.goarch, a.Name, k, err, c.name, c.kind)
		}
	}
	if _, _, err := SelectAsset(assets, "freebsd", "amd64"); err == nil || !strings.Contains(err.Error(), "freebsd/amd64") {
		t.Fatalf("missing asset err = %v", err)
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	m, err := ParseChecksums(strings.NewReader(h + "  gocode_linux_amd64\n\n" + strings.ToUpper(h) + " *gocode.zip\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["gocode_linux_amd64"] != h || m["gocode.zip"] != h {
		t.Fatalf("parsed %v", m)
	}
	if _, err := ParseChecksums(strings.NewReader("nothex  file\n")); err == nil {
		t.Fatal("malformed line accepted")
	}
}

func TestDownloadVerified(t *testing.T) {
	gh := newFakeGitHub(t)
	c := gh.client("")
	payload := []byte("binary payload")
	a := gh.asset("gocode_linux_amd64", payload)
	dir := t.TempDir()

	// pass
	dest := filepath.Join(dir, "ok")
	if got, err := c.DownloadVerified(context.Background(), a, map[string]string{a.Name: sha(payload)}, dest); err != nil || got != sha(payload) {
		t.Fatalf("verified download failed: %v", err)
	}

	// mismatch → refused, partial deleted
	dest = filepath.Join(dir, "bad")
	_, err := c.DownloadVerified(context.Background(), a, map[string]string{a.Name: strings.Repeat("0", 64)}, dest)
	var mism *ChecksumMismatchError
	if !errors.As(err, &mism) {
		t.Fatalf("want ChecksumMismatchError, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("partial download not removed")
	}

	// missing entry → refused before download
	n := len(gh.downloads())
	dest = filepath.Join(dir, "missing")
	_, err = c.DownloadVerified(context.Background(), a, map[string]string{"other": sha(payload)}, dest)
	var miss *MissingChecksumError
	if !errors.As(err, &miss) {
		t.Fatalf("want MissingChecksumError, got %v", err)
	}
	if len(gh.downloads()) != n {
		t.Errorf("downloaded despite missing checksum entry")
	}

	// size cap: release lists fewer bytes than served
	short := a
	short.Size = 3
	dest = filepath.Join(dir, "short")
	if _, err := c.DownloadVerified(context.Background(), short, map[string]string{a.Name: sha(payload)}, dest); err == nil {
		t.Fatal("oversized download accepted")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("oversized partial not removed")
	}
}

type archEntry struct {
	name string
	body []byte
	link bool
}

func makeTarGz(t *testing.T, entries []archEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link {
			hdr = &tar.Header{Name: e.name, Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !e.link {
			_, _ = tw.Write(e.body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func makeZip(t *testing.T, entries []archEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(e.body)
	}
	zw.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bin := []byte("the binary")

	// tar.gz, nested directory
	tgz := write("a.tar.gz", makeTarGz(t, []archEntry{{name: "README.md", body: []byte("hi")}, {name: "gocode_1.2.3/gocode", body: bin}}))
	out := filepath.Join(dir, "out1")
	if err := ExtractBinary(tgz, ArchiveTarGz, "gocode", out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bin) {
		t.Fatalf("extracted %q", got)
	}

	// zip
	z := write("a.zip", makeZip(t, []archEntry{{name: "gocode.exe", body: bin}}))
	out = filepath.Join(dir, "out2")
	if err := ExtractBinary(z, ArchiveZip, "gocode.exe", out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bin) {
		t.Fatalf("extracted %q", got)
	}

	// zip slip
	z = write("slip.zip", makeZip(t, []archEntry{{name: "../../evil", body: []byte("x")}, {name: "gocode", body: bin}}))
	if err := ExtractBinary(z, ArchiveZip, "gocode", filepath.Join(dir, "out3")); !errors.Is(err, ErrUnsafeArchivePath) {
		t.Fatalf("zip slip err = %v", err)
	}
	tgz = write("slip.tar.gz", makeTarGz(t, []archEntry{{name: "/abs/gocode", body: bin}}))
	if err := ExtractBinary(tgz, ArchiveTarGz, "gocode", filepath.Join(dir, "out4")); !errors.Is(err, ErrUnsafeArchivePath) {
		t.Fatalf("absolute path err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out3")); !os.IsNotExist(err) {
		t.Error("output written for rejected archive")
	}

	// symlink named like the binary → rejected
	tgz = write("link.tar.gz", makeTarGz(t, []archEntry{{name: "gocode", link: true}}))
	if err := ExtractBinary(tgz, ArchiveTarGz, "gocode", filepath.Join(dir, "out5")); err == nil {
		t.Fatal("symlink binary accepted")
	}

	// missing binary
	z = write("none.zip", makeZip(t, []archEntry{{name: "other", body: bin}}))
	if err := ExtractBinary(z, ArchiveZip, "gocode", filepath.Join(dir, "out6")); err == nil {
		t.Fatal("archive without binary accepted")
	}
}

// -----------------------------------------------------------------------------
// End-to-end Updater
// -----------------------------------------------------------------------------

// publish registers a release with the platform asset and checksums.txt.
func (f *fakeGitHub) publish(tag, assetName string, content []byte, sumsOverride []byte) *Release {
	a := f.asset(assetName, content)
	sums := sumsOverride
	if sums == nil {
		sums = checksumsFor(map[string][]byte{assetName: content})
	}
	c := f.asset("checksums.txt", sums)
	sig, err := releasesig.Sign(f.sk, sums, "timestamp:0\tfile:checksums.txt\thashed")
	if err != nil {
		panic(err)
	}
	s := f.asset("checksums.txt.sig", sig)
	rel := &Release{TagName: tag, Assets: []Asset{c, s, a}}
	f.latest = rel
	f.tags[tag] = rel
	return rel
}

func newTestUpdater(t *testing.T, gh *fakeGitHub, current string) (*Updater, *string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "bin", "gocode")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarted := new(string)
	return &Updater{
		TrustedKeys: []releasesig.PublicKey{gh.sk.Public()},
		Client:      gh.client(""),
		Current:     current,
		Executable:  exe,
		StagingDir:  dir,
		GOOS:        "linux",
		GOARCH:      "amd64",
		Restart: &Restarter{
			Exec: func(argv0 string, _ []string, _ []string) error { *restarted = argv0; return nil },
			Args: []string{"gocode"},
			Env:  []string{},
		},
	}, restarted
}

func TestUpdaterEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name, asset string
		pack        func([]byte) []byte
	}{
		{"raw", "gocode_linux_amd64", func(b []byte) []byte { return b }},
		{"targz", "gocode_linux_amd64.tar.gz", func(b []byte) []byte { return makeTarGz(t, []archEntry{{name: "gocode", body: b}}) }},
		{"zip", "gocode_linux_amd64.zip", func(b []byte) []byte { return makeZip(t, []archEntry{{name: "dist/gocode", body: b}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			newBin := []byte("NEW BINARY " + tc.name)
			gh.publish("v1.3.0", tc.asset, tc.pack(newBin), nil)
			u, restarted := newTestUpdater(t, gh, "1.2.0")

			res, err := u.Check(context.Background(), CheckOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Available || res.Current != "1.2.0" || res.Latest != "1.3.0" {
				t.Fatalf("check = %+v", res)
			}
			if len(gh.downloads()) != 0 {
				t.Fatal("Check downloaded assets")
			}
			installed, err := u.Apply(context.Background(), res.Release)
			if err != nil {
				t.Fatal(err)
			}
			if installed != u.Executable || *restarted != u.Executable {
				t.Fatalf("installed=%q restarted=%q want %q", installed, *restarted, u.Executable)
			}
			got, _ := os.ReadFile(u.Executable)
			if !bytes.Equal(got, newBin) {
				t.Fatalf("executable = %q", got)
			}
			if fi, _ := os.Stat(u.Executable); fi.Mode().Perm()&0o111 == 0 && runtime.GOOS != "windows" {
				t.Errorf("installed binary not executable: %v", fi.Mode())
			}
			// staging cleaned up
			entries, _ := os.ReadDir(u.StagingDir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "gocode-update-") {
					t.Errorf("staging dir left behind: %s", e.Name())
				}
			}
		})
	}
}

func TestUpdaterRefusesBadChecksum(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.publish("v1.3.0", "gocode_linux_amd64", []byte("NEW"), []byte(strings.Repeat("0", 64)+"  gocode_linux_amd64\n"))
	u, restarted := newTestUpdater(t, gh, "1.2.0")
	res, err := u.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Apply(context.Background(), res.Release)
	var mism *ChecksumMismatchError
	if !errors.As(err, &mism) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(u.Executable); string(got) != "OLD" || *restarted != "" {
		t.Fatal("executable replaced / restarted despite mismatch")
	}

	// no checksums entry
	gh.publish("v1.3.0", "gocode_linux_amd64", []byte("NEW"), []byte(strings.Repeat("0", 64)+"  other\n"))
	res, _ = u.Check(context.Background(), CheckOptions{})
	var miss *MissingChecksumError
	if _, err := u.Apply(context.Background(), res.Release); !errors.As(err, &miss) {
		t.Fatalf("err = %v", err)
	}

	// no checksums.txt at all
	rel := &Release{TagName: "v1.3.0", Assets: []Asset{gh.asset("gocode_linux_amd64", []byte("NEW"))}}
	if _, err := u.Apply(context.Background(), rel); err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(u.Executable); string(got) != "OLD" {
		t.Fatal("executable replaced")
	}
}

func TestUpdaterDecisions(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.publish("v1.3.0", "gocode_linux_amd64", []byte("NEW"), nil)

	// dev build without force: reported, nothing downloaded
	u, _ := newTestUpdater(t, gh, "dev")
	res, err := u.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Available || !res.DevBuild || res.Reason != DevBuildReason {
		t.Fatalf("dev check = %+v", res)
	}
	if len(gh.downloads()) != 0 {
		t.Fatal("dev build downloaded")
	}
	// dev build with force
	if res, _ = u.Check(context.Background(), CheckOptions{Force: true}); !res.Available {
		t.Fatalf("dev forced = %+v", res)
	}

	// up to date
	u.Current = "v1.3.0"
	if res, _ = u.Check(context.Background(), CheckOptions{}); res.Available {
		t.Fatalf("up-to-date = %+v", res)
	}
	if res, _ = u.Check(context.Background(), CheckOptions{Force: true}); !res.Available {
		t.Fatalf("forced reinstall = %+v", res)
	}
	// newer local build is not downgraded by the channel…
	u.Current = "2.0.0"
	if res, _ = u.Check(context.Background(), CheckOptions{}); res.Available {
		t.Fatalf("downgrade via channel = %+v", res)
	}
	// …but an explicit tag may downgrade
	if res, err = u.Check(context.Background(), CheckOptions{Tag: "1.3.0"}); err != nil || !res.Available {
		t.Fatalf("explicit tag = %+v %v", res, err)
	}

	// pin: latest above MaxVersion → pinned release
	gh.tags["v1.2.5"] = &Release{TagName: "v1.2.5"}
	u.Current, u.MaxVersion = "1.2.0", "1.2.5"
	if res, err = u.Check(context.Background(), CheckOptions{}); err != nil || res.Latest != "1.2.5" || !res.Available {
		t.Fatalf("pinned = %+v %v", res, err)
	}
}

func TestReplaceExecutableWindowsMovesAside(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "gocode.exe")
	_ = os.WriteFile(src, []byte("NEW"), 0o755)
	_ = os.WriteFile(dst, []byte("OLD"), 0o755)
	if err := replaceExecutable("windows", src, dst, sha([]byte("NEW"))); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW" {
		t.Fatalf("dst = %q", got)
	}
	if got, _ := os.ReadFile(dst + ".old"); string(got) != "OLD" {
		t.Fatalf("old = %q", got)
	}
	// failure restores the original
	_ = os.WriteFile(src, []byte("NEWER"), 0o755)
	if err := replaceExecutable("windows", src, dst, strings.Repeat("0", 64)); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW" {
		t.Fatalf("dst not restored: %q", got)
	}
}
