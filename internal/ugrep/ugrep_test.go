package ugrep

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func fakeBundle(version string, data []byte) *Bundle {
	sum := sha256.Sum256(data)
	return &Bundle{Version: version, SHA256: hex.EncodeToString(sum[:]), Name: ExeName(), Data: data}
}

func TestExtractWritesVerifiedExecutable(t *testing.T) {
	root := t.TempDir()
	b := fakeBundle("9.9.9", []byte("#!/bin/sh\necho ugrep 9.9.9\n"))
	p, err := Extract(b, root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "gocode", "ugrep", "9.9.9", ExeName())
	if p != want {
		t.Fatalf("path = %s, want %s", p, want)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(b.Data) {
		t.Fatal("content mismatch")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("expected only the binary in the cache dir, got %v", entries)
	}
}

func TestExtractRemovesStaleSearchCache(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "gocode", "ripgrep", "15.2.0")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stale, "rg"), []byte("old"), 0o755)
	if _, err := Extract(fakeBundle("1.0.0", []byte("x")), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "gocode", "ripgrep")); !os.IsNotExist(err) {
		t.Fatalf("stale search binary cache not removed: %v", err)
	}
}

func TestExtractReusesGoodCache(t *testing.T) {
	root := t.TempDir()
	b := fakeBundle("1.0.0", []byte("binary-v1"))
	p, err := Extract(b, root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	p2, err := Extract(b, root)
	if err != nil || p2 != p {
		t.Fatalf("second extract: %v %s", err, p2)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a verified cached binary must be reused, not rewritten")
	}
}

func TestExtractReplacesCorruptCache(t *testing.T) {
	root := t.TempDir()
	b := fakeBundle("1.0.0", []byte("the real binary"))
	p, err := Extract(b, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(b, root); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "the real binary" {
		t.Fatalf("corrupt cache not re-extracted: %q", got)
	}
	os.WriteFile(p, nil, 0o755)
	if _, err := Extract(b, root); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "the real binary" {
		t.Fatal("truncated cache not re-extracted")
	}
}

func TestExtractRejectsCorruptEmbeddedBytes(t *testing.T) {
	b := fakeBundle("1.0.0", []byte("good"))
	b.Data = []byte("evil")
	root := t.TempDir()
	if _, err := Extract(b, root); err == nil {
		t.Fatal("expected an error for embedded bytes not matching the embedded sha256")
	}
	if _, err := os.Stat(filepath.Join(root, "gocode", "ugrep", "1.0.0", ExeName())); err == nil {
		t.Fatal("an unverified binary must never be written")
	}
}

func TestExtractRejectsBadVersion(t *testing.T) {
	if _, err := Extract(fakeBundle("../../etc", []byte("x")), t.TempDir()); err == nil {
		t.Fatal("expected a path-traversal version to be rejected")
	}
}

func TestExtractCleansUpOtherVersions(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "gocode", "ugrep")
	for _, v := range []string{"7.0.0", "7.8.4"} {
		os.MkdirAll(filepath.Join(base, v), 0o755)
		os.WriteFile(filepath.Join(base, v, ExeName()), []byte("old"), 0o755)
	}
	os.WriteFile(filepath.Join(base, "stray-file"), []byte("x"), 0o644)
	if _, err := Extract(fakeBundle("7.8.5", []byte("new")), root); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 || entries[0].Name() != "7.8.5" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("other versions not cleaned up: %v", names)
	}
}

func TestCleanupOtherVersionsNeverFails(t *testing.T) {
	CleanupOtherVersions(filepath.Join(t.TempDir(), "missing"), "1.0.0")
}

func TestExtractConcurrent(t *testing.T) {
	root := t.TempDir()
	b := fakeBundle("2.0.0", []byte(strings.Repeat("x", 1<<16)))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Extract(b, root); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

type env map[string]string

func (e env) get(k string) string { return e[k] }

func boolp(b bool) *bool { return &b }

func makeExe(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveOrder(t *testing.T) {
	tmp := t.TempDir()
	configured := makeExe(t, tmp, "my-ugrep")
	envPath := makeExe(t, tmp, "env-ugrep")
	notExec := filepath.Join(tmp, "plain")
	os.WriteFile(notExec, []byte("x"), 0o644)
	cache := t.TempDir()
	bundle := fakeBundle("7.8.5", []byte("bundled"))
	bundledPath := filepath.Join(cache, "gocode", "ugrep", "7.8.5", ExeName())

	systemFound := func(string) (string, error) { return "/usr/bin/ugrep", nil }
	systemMissing := func(string) (string, error) { return "", errors.New("not found") }
	withBundle := func() *Bundle { return bundle }
	noBundle := func() *Bundle { return nil }

	type tcase struct {
		name       string
		cfg        Config
		env        env
		wantPath   string
		wantSource Source
		wantErr    bool
	}
	cases := []tcase{
		{"ugrep off", Config{UseUgrep: boolp(false), UgrepPath: configured}, nil, "", SourceBuiltin, false},
		{"configured path wins", Config{UgrepPath: configured, LookPath: systemFound, Bundle: withBundle}, nil, configured, SourceConfigured, false},
		{"env path wins over setting", Config{UgrepPath: configured, Bundle: withBundle}, env{EnvUgrepPath: envPath}, envPath, SourceConfigured, false},
		{"bad configured path falls through to bundled", Config{UgrepPath: filepath.Join(tmp, "nope"), LookPath: systemFound, Bundle: withBundle}, nil, bundledPath, SourceBundled, false},
		{"bundled preferred by default", Config{LookPath: systemFound, Bundle: withBundle}, nil, bundledPath, SourceBundled, false},
		{"preferSystem setting", Config{PreferSystem: true, LookPath: systemFound, Bundle: withBundle}, nil, "/usr/bin/ugrep", SourceSystem, false},
		{"preferSystem but none on PATH", Config{PreferSystem: true, LookPath: systemMissing, Bundle: withBundle}, nil, bundledPath, SourceBundled, false},
		{"no bundle -> system", Config{LookPath: systemFound, Bundle: noBundle}, nil, "/usr/bin/ugrep", SourceSystem, false},
		{"nothing -> builtin", Config{LookPath: systemMissing, Bundle: noBundle}, nil, "", SourceBuiltin, true},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, tcase{"non-executable configured path skipped", Config{UgrepPath: notExec, LookPath: systemMissing, Bundle: noBundle}, nil, "", SourceBuiltin, true})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Getenv = tc.env.get
			cfg.CacheDir = func() (string, error) { return cache, nil }
			if cfg.LookPath == nil {
				cfg.LookPath = systemMissing
			}
			if cfg.Bundle == nil {
				cfg.Bundle = noBundle
			}
			r := ResolveDetailed(cfg)
			p, src, err := r.Path, r.Source, r.Err
			if p != tc.wantPath || src != tc.wantSource || (err != nil) != tc.wantErr {
				t.Fatalf("ResolveDetailed = (%q, %q, %v), want (%q, %q, err=%v)", p, src, err, tc.wantPath, tc.wantSource, tc.wantErr)
			}
		})
	}
}

func TestResolveNotesBadConfiguredPath(t *testing.T) {
	r := ResolveDetailed(Config{
		UgrepPath: "/definitely/not/here/ugrep",
		Getenv:    env{}.get,
		LookPath:  func(string) (string, error) { return "/usr/bin/ugrep", nil },
		Bundle:    func() *Bundle { return nil },
	})
	if r.Source != SourceSystem || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "search.ugrepPath") {
		t.Fatalf("unexpected resolution %+v", r)
	}
}

func TestResolveCacheDirErrorFallsBack(t *testing.T) {
	r := ResolveDetailed(Config{
		Getenv:   env{}.get,
		LookPath: func(string) (string, error) { return "/usr/bin/ugrep", nil },
		Bundle:   func() *Bundle { return fakeBundle("1.0.0", []byte("x")) },
		CacheDir: func() (string, error) { return "", errors.New("no home") },
	})
	if r.Source != SourceSystem || len(r.Notes) == 0 {
		t.Fatalf("unexpected resolution %+v", r)
	}
}

// TestBundledMatchesChecksum checks the embedded binary of this platform,
// when one is vendored, against its checksum and the vendored version.
func TestBundledMatchesChecksum(t *testing.T) {
	b := Bundled()
	if b == nil {
		t.Skip("no bundled ugrep for " + runtime.GOOS + "/" + runtime.GOARCH)
	}
	sum := sha256.Sum256(b.Data)
	if hex.EncodeToString(sum[:]) != b.SHA256 {
		t.Fatal("embedded ugrep does not match embedded sha256")
	}
	p, err := Extract(b, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v, err := ProbeVersion(p)
	if err != nil {
		t.Fatal(err)
	}
	if v != b.Version {
		t.Fatalf("ugrep --version = %s, bundle says %s", v, b.Version)
	}
}

// TestVendoredChecksumsAgree checks every vendored binary against its
// .sha256 file and third_party/ugrep/checksums.txt, on any host.
func TestVendoredChecksumsAgree(t *testing.T) {
	sums, err := os.ReadFile(filepath.Join("..", "..", "third_party", "ugrep", "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && !strings.HasPrefix(line, "#") {
			listed[f[0]] = f[3]
		}
	}
	version, _ := os.ReadFile(filepath.Join("bin", "VERSION"))
	tpVersion, _ := os.ReadFile(filepath.Join("..", "..", "third_party", "ugrep", "VERSION"))
	if strings.TrimSpace(string(version)) != strings.TrimSpace(string(tpVersion)) {
		t.Fatalf("bin/VERSION %q != third_party/ugrep/VERSION %q", version, tpVersion)
	}
	dirs, _ := filepath.Glob(filepath.Join("bin", "*_*"))
	found := 0
	for _, d := range dirs {
		plat := filepath.Base(d)
		for _, name := range []string{"ugrep", "ugrep.exe"} {
			data, err := os.ReadFile(filepath.Join(d, name))
			if err != nil {
				continue
			}
			found++
			sum := sha256.Sum256(data)
			got := hex.EncodeToString(sum[:])
			want, _ := os.ReadFile(filepath.Join(d, "ugrep.sha256"))
			if strings.TrimSpace(string(want)) != got {
				t.Errorf("%s: ugrep.sha256 %q, binary %s", plat, strings.TrimSpace(string(want)), got)
			}
			if listed[plat] != got {
				t.Errorf("%s: checksums.txt lists %q, binary %s", plat, listed[plat], got)
			}
		}
	}
	if found != len(listed) {
		t.Errorf("checksums.txt lists %d binaries, %d vendored", len(listed), found)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "third_party", "ugrep", "LICENSE.txt")); err != nil {
		t.Errorf("the ugrep licence must ship with the binaries: %v", err)
	}
}

func TestLoadBundleWithoutBinary(t *testing.T) {
	if b := loadBundle(bundleFS, ""); b != nil {
		t.Fatal("an empty bundle dir must mean no bundle")
	}
}
