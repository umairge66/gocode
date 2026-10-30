package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/updater"
	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// fakeReleaseServer serves /repos/acme/tool/releases/latest plus /dl/<asset>.
// checksums.txt is signed with a key generated for this test, and that key is
// installed as the compiled-in updater.ReleasePublicKeys until the test ends.
func fakeReleaseServer(t *testing.T, tag string, bin []byte) (*httptest.Server, *int32) {
	t.Helper()
	sk, err := releasesig.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	savedKeys := updater.ReleasePublicKeys
	t.Cleanup(func() { updater.ReleasePublicKeys = savedKeys })
	updater.ReleasePublicKeys = sk.Public().String()

	sum := sha256.Sum256(bin)
	sums := fmt.Sprintf("%s  gocode_linux_amd64\n", hex.EncodeToString(sum[:]))
	sig, err := releasesig.Sign(sk, []byte(sums), "timestamp:1\tfile:checksums.txt\thashed")
	if err != nil {
		t.Fatal(err)
	}
	var downloads int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/tool/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag,
				"assets": []map[string]any{
					{"name": "checksums.txt", "size": len(sums), "browser_download_url": srv.URL + "/dl/checksums.txt"},
					{"name": "checksums.txt.sig", "size": len(sig), "browser_download_url": srv.URL + "/dl/checksums.txt.sig"},
					{"name": "gocode_linux_amd64", "size": len(bin), "browser_download_url": srv.URL + "/dl/gocode_linux_amd64"},
				},
			})
		case "/dl/checksums.txt":
			atomic.AddInt32(&downloads, 1)
			_, _ = w.Write([]byte(sums))
		case "/dl/checksums.txt.sig":
			atomic.AddInt32(&downloads, 1)
			_, _ = w.Write(sig)
		case "/dl/gocode_linux_amd64":
			atomic.AddInt32(&downloads, 1)
			_, _ = w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

type updateHarness struct {
	exe       string
	restarted string
	env       *Env
	out, errb *bytes.Buffer
}

func setupUpdate(t *testing.T, srv *httptest.Server, current string, envVars map[string]string) *updateHarness {
	t.Helper()
	h := &updateHarness{out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	dir := t.TempDir()
	h.exe = filepath.Join(dir, "gocode")
	if err := os.WriteFile(h.exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	saved := []any{updateHTTPClient, updateExecutable, updateCurrentVersion, updateSettingsRepo, updateGOOS, updateGOARCH, updateRestart}
	t.Cleanup(func() {
		updateHTTPClient = saved[0].(updater.Doer)
		updateExecutable = saved[1].(func() (string, error))
		updateCurrentVersion = saved[2].(func() string)
		updateSettingsRepo = saved[3].(func(*Env) string)
		updateGOOS, updateGOARCH = saved[4].(string), saved[5].(string)
		updateRestart = saved[6].(func(string) error)
	})
	if srv != nil {
		updateHTTPClient = srv.Client()
	}
	updateExecutable = func() (string, error) { return h.exe, nil }
	updateCurrentVersion = func() string { return current }
	updateSettingsRepo = func(*Env) string { return "" }
	updateGOOS, updateGOARCH = "linux", "amd64"
	updateRestart = func(exe string) error { h.restarted = exe; return nil }

	vars := map[string]string{}
	if srv != nil {
		vars["GOCODE_UPDATE_API_URL"] = srv.URL
	}
	for k, v := range envVars {
		vars[k] = v
	}
	h.env = &Env{
		Getenv: func(k string) string { return vars[k] },
		Setenv: func(string, string) {},
		Stdout: h.out,
		Stderr: h.errb,
		Cwd:    func() (string, error) { return dir, nil },
	}
	return h
}

func TestUpdateCheckOutput(t *testing.T) {
	srv, downloads := fakeReleaseServer(t, "v1.3.0", []byte("NEW"))
	h := setupUpdate(t, srv, "1.2.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})

	code := Run(h.env, []string{"gocode", "update", "--check"})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, h.errb)
	}
	out := h.out.String()
	for _, want := range []string{"Current version: 1.2.0", "Latest version:  1.3.0", "Update available: 1.2.0 → 1.3.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if *downloads != 0 {
		t.Error("--check downloaded assets")
	}
	if got, _ := os.ReadFile(h.exe); string(got) != "OLD" {
		t.Error("--check replaced the executable")
	}
}

func TestUpdateFullFlow(t *testing.T) {
	srv, _ := fakeReleaseServer(t, "v1.3.0", []byte("NEW BINARY"))
	h := setupUpdate(t, srv, "1.2.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})

	code := Run(h.env, []string{"gocode", "upgrade"})
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, h.errb)
	}
	if got, _ := os.ReadFile(h.exe); string(got) != "NEW BINARY" {
		t.Fatalf("executable = %q", got)
	}
	if h.restarted != h.exe {
		t.Errorf("restarted %q, want %q", h.restarted, h.exe)
	}
	if !strings.Contains(h.out.String(), "Updating gocode: 1.2.0 → 1.3.0") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestUpdateUpToDateAndDevBuild(t *testing.T) {
	srv, downloads := fakeReleaseServer(t, "v1.3.0", []byte("NEW"))
	h := setupUpdate(t, srv, "1.3.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	if code := Run(h.env, []string{"gocode", "update"}); code != 0 || !strings.Contains(h.out.String(), "up to date") {
		t.Fatalf("exit %d out=%s err=%s", code, h.out, h.errb)
	}

	h = setupUpdate(t, srv, "dev", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	code := Run(h.env, []string{"gocode", "update"})
	if code == 0 || !strings.Contains(h.out.String(), "Development build; cannot compare versions") {
		t.Fatalf("dev: exit %d out=%s", code, h.out)
	}
	if *downloads != 0 || h.restarted != "" {
		t.Fatal("dev build without --force downloaded/restarted")
	}

	h = setupUpdate(t, srv, "dev", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	if code := Run(h.env, []string{"gocode", "update", "--force"}); code != 0 {
		t.Fatalf("dev --force: exit %d err=%s", code, h.errb)
	}
	if got, _ := os.ReadFile(h.exe); string(got) != "NEW" {
		t.Fatalf("dev --force did not install: %q", got)
	}
}

func TestUpdateUnconfiguredAndBadFlags(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) }))
	t.Cleanup(srv.Close)
	h := setupUpdate(t, srv, "1.0.0", nil)
	old := updater.DefaultRepo
	updater.DefaultRepo = ""
	t.Cleanup(func() { updater.DefaultRepo = old })

	code := Run(h.env, []string{"gocode", "update"})
	if code == 0 || !strings.Contains(h.errb.String(), "update source not configured: set GOCODE_UPDATE_REPO=owner/repo") {
		t.Fatalf("exit %d err=%s", code, h.errb)
	}
	if calls != 0 {
		t.Fatalf("made %d HTTP calls while unconfigured", calls)
	}

	// settings value is used when env is unset
	h = setupUpdate(t, srv, "1.0.0", nil)
	updateSettingsRepo = func(*Env) string { return "bad repo" }
	if code := Run(h.env, []string{"gocode", "update", "--check"}); code == 0 || !strings.Contains(h.errb.String(), "invalid repository") {
		t.Fatalf("exit %d err=%s", code, h.errb)
	}

	h = setupUpdate(t, srv, "1.0.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	if code := Run(h.env, []string{"gocode", "update", "--channel", "nightly"}); code == 0 {
		t.Fatal("invalid channel accepted")
	}
	if code := Run(h.env, []string{"gocode", "update", "--bogus"}); code == 0 {
		t.Fatal("unknown flag accepted")
	}
}

func TestParseUpdateFlags(t *testing.T) {
	f, err := parseUpdateFlags("update", []string{"update", "--check", "--channel=latest", "--force"})
	if err != nil || !f.check || !f.force || f.channel != "latest" {
		t.Fatalf("%+v %v", f, err)
	}
	f, err = parseUpdateFlags("install", []string{"install", "v1.2.3", "--force"})
	if err != nil || f.target != "v1.2.3" || !f.force {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := parseUpdateFlags("update", []string{"update", "extra"}); err == nil {
		t.Fatal("positional accepted for update")
	}
}

func TestUpdateRefusesWithoutSigningKey(t *testing.T) {
	srv, downloads := fakeReleaseServer(t, "v1.3.0", []byte("NEW"))
	updater.ReleasePublicKeys = "" // a dev build: no key compiled in

	for _, args := range [][]string{{"gocode", "update"}, {"gocode", "install", "1.3.0"}, {"gocode", "update", "--force"}} {
		h := setupUpdate(t, srv, "1.2.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
		code := Run(h.env, args)
		if code == 0 || !strings.Contains(h.errb.String(), "This build has no release signing key, so updates can't be verified. Build with -ldflags") {
			t.Fatalf("%v: exit %d err=%s", args, code, h.errb)
		}
		if got, _ := os.ReadFile(h.exe); string(got) != "OLD" || h.restarted != "" {
			t.Fatalf("%v: installed without a key", args)
		}
	}
	if *downloads != 0 {
		t.Fatalf("downloaded %d assets without a key", *downloads)
	}

	// --check only reports and still works.
	h := setupUpdate(t, srv, "1.2.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	if code := Run(h.env, []string{"gocode", "update", "--check"}); code != 0 || !strings.Contains(h.out.String(), "Update available") {
		t.Fatalf("--check: exit %d out=%s err=%s", code, h.out, h.errb)
	}
}

func TestUpdateRefusesWrongKey(t *testing.T) {
	srv, _ := fakeReleaseServer(t, "v1.3.0", []byte("NEW"))
	other, err := releasesig.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	updater.ReleasePublicKeys = other.Public().String()
	h := setupUpdate(t, srv, "1.2.0", map[string]string{"GOCODE_UPDATE_REPO": "acme/tool"})
	code := Run(h.env, []string{"gocode", "update"})
	if code == 0 || !strings.Contains(h.errb.String(), "checksums.txt.sig") || !strings.Contains(h.errb.String(), "refusing to install") {
		t.Fatalf("exit %d err=%s", code, h.errb)
	}
	if got, _ := os.ReadFile(h.exe); string(got) != "OLD" {
		t.Fatal("installed despite an untrusted signature")
	}
}
