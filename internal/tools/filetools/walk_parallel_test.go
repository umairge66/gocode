package filetools

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// sequentialWalk is the plain depth-first walk the parallel one must match:
// it probes every directory for its ignore files by name.
func sequentialWalk(root string, f *walkFilter, chain []*dirIgnores) []string {
	var out []string
	var walk func(dir string, chain []*dirIgnores)
	walk = func(dir string, chain []*dirIgnores) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			abs := filepath.Join(dir, e.Name())
			isDir := e.IsDir()
			if !e.Type().IsRegular() && !isDir {
				continue
			}
			if !f.decide(abs, e.Name(), isDir, chain) {
				continue
			}
			if isDir {
				next := chain
				if f.useIgnores {
					next = append(chain[:len(chain):len(chain)], loadDirIgnores(abs))
				}
				walk(abs, next)
				continue
			}
			out = append(out, abs)
		}
	}
	walk(root, chain)
	return out
}

// buildIgnoreTree makes a random tree with ignore files, hidden entries and
// a nested repository at several depths.
func buildIgnoreTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	r := rand.New(rand.NewPCG(1, 2))
	write := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, ".gitignore"), "*.log\nbuild/\n")
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	dirs = append(dirs, root)
	for i := 0; i < 400; i++ {
		parent := dirs[r.IntN(len(dirs))]
		d := filepath.Join(parent, fmt.Sprintf("d%03d", i))
		if r.IntN(15) == 0 {
			d = filepath.Join(parent, "build")
		}
		if r.IntN(20) == 0 {
			d = filepath.Join(parent, fmt.Sprintf(".hidden%d", i))
		}
		dirs = append(dirs, d)
		for j := 0; j < 1+r.IntN(4); j++ {
			ext := []string{".go", ".log", ".txt", ".tmp"}[r.IntN(4)]
			write(filepath.Join(d, fmt.Sprintf("f%d%s", j, ext)), "x\n")
		}
		switch r.IntN(12) {
		case 0:
			write(filepath.Join(d, ".gitignore"), "*.tmp\n!keep.tmp\n")
			write(filepath.Join(d, "keep.tmp"), "x\n")
		case 1:
			write(filepath.Join(d, ".ignore"), "*.txt\n")
		case 2:
			write(filepath.Join(d, ".rgignore"), "f0.go\n")
		case 3:
			if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(d, ".gitignore"), "f1.*\n")
		}
	}
	return root
}

func TestParallelWalkMatchesSequential(t *testing.T) {
	root := buildIgnoreTree(t)
	for _, c := range []struct{ hidden, ignores bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		f, chain := newWalkFilter(root, c.hidden, c.ignores, newOverrideSet(root, vcsExcludeGlobs()), nil)
		want := sequentialWalk(root, f, chain)
		for i := 0; i < 5; i++ {
			got := walkFiles(root, f, chain)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("hidden=%v ignores=%v: parallel walk differs (%d vs %d files)", c.hidden, c.ignores, len(got), len(want))
			}
		}
		if len(want) == 0 {
			t.Fatalf("hidden=%v ignores=%v: empty walk", c.hidden, c.ignores)
		}
	}
}

func TestIgnoreFileName(t *testing.T) {
	for name, want := range map[string]string{
		".gitignore": ".gitignore", ".ignore": ".ignore", ".rgignore": ".rgignore", ".git": ".git",
		"gitignore": "", ".gitignores": "", ".gi": "", "main.go": "",
	} {
		if got := ignoreFileName(name); got != want {
			t.Errorf("%q: got %q, want %q", name, got, want)
		}
	}
	upper := ignoreFileName(".GITIGNORE")
	if caseInsensitiveFS && upper != ".gitignore" || !caseInsensitiveFS && upper != "" {
		t.Errorf(".GITIGNORE: got %q (case-insensitive FS: %v)", upper, caseInsensitiveFS)
	}
}
