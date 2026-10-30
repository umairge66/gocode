package extension

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// notRuntime reports whether a path in this directory (slash-separated) is
// not part of the extension Chrome loads.
func notRuntime(name string) bool {
	switch name {
	case "README.md", "embed.go", ".gitignore", "store", ".keys", "icons/src":
		return true
	}
	return strings.HasSuffix(name, "_test.go")
}

func TestRuntimeFilesAreExactlyTheEmbeddedFiles(t *testing.T) {
	if !slices.IsSorted(RuntimeFiles) {
		t.Errorf("RuntimeFiles is not sorted: %v", RuntimeFiles)
	}
	var embedded []string
	err := fs.WalkDir(FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded = append(embedded, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(embedded)
	if !slices.Equal(embedded, RuntimeFiles) {
		t.Errorf("embedded files %v != RuntimeFiles %v", embedded, RuntimeFiles)
	}
}

// Every file in extension/ that is not documentation or Go code must be
// embedded, so a new runtime file (an icon, a script) cannot be left out of
// the binary and the store zip.
func TestEveryRuntimeFileOnDiskIsEmbedded(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := filepath.ToSlash(path)
		if name == "." {
			return nil
		}
		if notRuntime(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !slices.Contains(RuntimeFiles, name) {
			t.Errorf("%s is in extension/ but not embedded (add it to the go:embed line and RuntimeFiles, or to notRuntime)", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range RuntimeFiles {
		disk, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil {
			t.Fatal(err)
		}
		emb, _ := ReadFile(name)
		if !bytes.Equal(disk, emb) {
			t.Errorf("embedded %s differs from the file on disk", name)
		}
	}
}

func TestInfo(t *testing.T) {
	m := Info()
	if m.Version == "" || m.Key == "" || m.Name == "" {
		t.Fatalf("Info() = %+v, want name, version and key", m)
	}
}

func TestStoreManifestDropsOnlyTheKey(t *testing.T) {
	b, err := StoreManifest()
	if err != nil {
		t.Fatal(err)
	}
	var store, full map[string]any
	if err := json.Unmarshal(b, &store); err != nil {
		t.Fatalf("store manifest is not JSON: %v", err)
	}
	_ = json.Unmarshal(Manifest(), &full)
	if _, ok := store["key"]; ok {
		t.Fatal("store manifest still has a key")
	}
	delete(full, "key")
	ob, _ := json.Marshal(full)
	sb, _ := json.Marshal(store)
	if !bytes.Equal(ob, sb) {
		t.Errorf("store manifest differs beyond the key:\n%s\n%s", ob, sb)
	}
	if bytes.Contains(b, []byte(`\u003c`)) {
		t.Errorf("store manifest escapes < as \\u003c:\n%s", b)
	}
}

func TestStoreZipIsReproducibleAndHasOnlyRuntimeFiles(t *testing.T) {
	var a, b bytes.Buffer
	if err := WriteStoreZip(&a); err != nil {
		t.Fatal(err)
	}
	if err := WriteStoreZip(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two zips of the same files differ")
	}
	CheckStoreZip(t, a.Bytes())
}

// CheckStoreZip verifies a store upload zip: exactly RuntimeFiles, in order,
// no "key" in the manifest, other files identical to the embedded ones.
func CheckStoreZip(t *testing.T, data []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "manifest.json" {
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("zip manifest is not JSON: %v", err)
			}
			if _, ok := m["key"]; ok {
				t.Error(`zip manifest has a "key" field`)
			}
			continue
		}
		want, err := ReadFile(f.Name)
		if err != nil {
			t.Errorf("unexpected file %s in zip", f.Name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s in zip differs from the embedded file", f.Name)
		}
	}
	if !slices.Equal(names, RuntimeFiles) {
		t.Errorf("zip entries %v, want %v", names, RuntimeFiles)
	}
}

// The Chrome Web Store rejects a manifest description over 132 characters.
func TestManifestDescriptionFitsStoreLimit(t *testing.T) {
	var m struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(Manifest(), &m); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(m.Description)); n == 0 || n > 132 {
		t.Errorf("manifest description has %d characters, want 1-132", n)
	}
}

// Every icon the manifest names (top-level and toolbar action) is an embedded
// PNG of the declared size, and the store-required sizes are present.
func TestManifestIconsAreEmbeddedPNGs(t *testing.T) {
	var m struct {
		Icons  map[string]string `json:"icons"`
		Action struct {
			DefaultIcon map[string]string `json:"default_icon"`
		} `json:"action"`
	}
	if err := json.Unmarshal(Manifest(), &m); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"16", "32", "48", "128"} {
		if m.Icons[size] == "" {
			t.Errorf("manifest icons has no %s entry", size)
		}
		if m.Action.DefaultIcon[size] == "" {
			t.Errorf("manifest action.default_icon has no %s entry", size)
		}
	}
	for _, set := range []map[string]string{m.Icons, m.Action.DefaultIcon} {
		for size, path := range set {
			if !slices.Contains(RuntimeFiles, path) {
				t.Errorf("icon %s (%s) is not a runtime file", path, size)
				continue
			}
			data, err := ReadFile(path)
			if err != nil {
				t.Fatalf("icon %s: %v", path, err)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Errorf("icon %s is not a PNG: %v", path, err)
				continue
			}
			if want, _ := strconv.Atoi(size); cfg.Width != want || cfg.Height != want {
				t.Errorf("icon %s is %dx%d, want %sx%s", path, cfg.Width, cfg.Height, size, size)
			}
		}
	}
}
