package filetools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListPathsBreadthFirstWithDirs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	for p, body := range map[string]string{
		".git/HEAD":         "x",
		".gitignore":        "out/\n",
		"b.txt":             "x",
		"a/x/deep.go":       "x",
		"a/y.go":            "x",
		"out/gen.go":        "x",
		"skipme/inside.go":  "x",
		"pkg/.ignore":       "*.tmp\n",
		"pkg/keep.go":       "x",
		"pkg/scratch.tmp":   "x",
		".config/tool.json": "x",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type entry struct {
		rel   string
		isDir bool
	}
	list := func(opt ListOptions) []entry {
		var got []entry
		complete, err := ListPaths(context.Background(), root, opt, func(rel string, isDir bool) bool {
			got = append(got, entry{rel, isDir})
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if opt.Limit == 0 && !complete {
			t.Fatal("unlimited walk reported incomplete")
		}
		return got
	}
	got := list(ListOptions{IncludeHidden: true, RespectIgnore: true, SkipDirs: map[string]struct{}{"skipme": {}}})
	want := []entry{
		{".config", true}, {".gitignore", false}, {"a", true}, {"b.txt", false}, {"pkg", true},
		{".config/tool.json", false}, {"a/x", true}, {"a/y.go", false}, {"pkg/.ignore", false}, {"pkg/keep.go", false},
		{"a/x/deep.go", false},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("walk order:\n got %v\nwant %v", got, want)
	}

	got = list(ListOptions{IncludeHidden: true, RespectIgnore: true, Limit: 3})
	if len(got) != 3 {
		t.Fatalf("limit 3 visited %d", len(got))
	}

	var n int
	complete, err := ListPaths(context.Background(), root, ListOptions{}, func(string, bool) bool { n++; return n < 2 })
	if complete || err != nil || n != 2 {
		t.Fatalf("visit stop: complete=%v err=%v n=%d", complete, err, n)
	}

	if _, err := ListPaths(context.Background(), filepath.Join(root, "b.txt"), ListOptions{}, func(string, bool) bool { return true }); err == nil {
		t.Fatal("a file root must be rejected")
	}
}
