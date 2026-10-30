package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/umairge66/gocode/extension"
)

// checkZip verifies the store zip: exactly the runtime files, and no "key" in
// manifest.json.
func checkZip(t *testing.T, path string) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name != "manifest.json" {
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("manifest.json in zip: %v", err)
		}
		if _, ok := m["key"]; ok {
			t.Error(`manifest.json in the zip has a "key"`)
		}
		if m["version"] != extension.Version() {
			t.Errorf("zip manifest version = %v", m["version"])
		}
	}
	if !slices.Equal(names, extension.RuntimeFiles) {
		t.Errorf("zip entries = %v, want %v", names, extension.RuntimeFiles)
	}
	for _, icon := range []string{"icons/icon-16.png", "icons/icon-32.png", "icons/icon-48.png", "icons/icon-128.png"} {
		if !slices.Contains(names, icon) {
			t.Errorf("zip has no %s", icon)
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "icons/src/") || strings.HasSuffix(name, ".svg") {
			t.Errorf("zip contains icon source %s", name)
		}
	}
}

func TestRun_WritesReproducibleZip(t *testing.T) {
	var sums []string
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		var out, errb bytes.Buffer
		if code := run([]string{"-o", dir}, &out, &errb); code != 0 {
			t.Fatalf("run = %d: %s", code, errb.String())
		}
		path := filepath.Join(dir, zipName(extension.Version()))
		checkZip(t, path)
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || lines[0] != path || !strings.HasPrefix(lines[1], "sha256 ") {
			t.Fatalf("output = %q", out.String())
		}
		sum := strings.TrimPrefix(lines[1], "sha256 ")
		side, err := os.ReadFile(path + ".sha256")
		if err != nil || !strings.HasPrefix(string(side), sum+"  ") {
			t.Errorf(".sha256 file = %q, %v", side, err)
		}
		sums = append(sums, sum)
	}
	if sums[0] != sums[1] {
		t.Errorf("two runs gave different sha256: %v", sums)
	}
}

// The shell wrapper builds the same zip.
func TestPackageScript(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "scripts", "package-extension.sh"))
	var sums []string
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		cmd := exec.Command(sh, script, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		path := filepath.Join(dir, zipName(extension.Version()))
		checkZip(t, path)
		i := strings.Index(string(out), "sha256 ")
		if i < 0 {
			t.Fatalf("script printed no sha256:\n%s", out)
		}
		sums = append(sums, strings.TrimSpace(string(out[i+len("sha256 "):])))
	}
	if sums[0] != sums[1] {
		t.Errorf("two script runs gave different sha256: %v", sums)
	}
}
