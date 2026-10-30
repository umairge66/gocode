package confighome

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolvePrecedence(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	cases := []struct {
		name string
		env  map[string]string
		home string
		want string
		ok   bool
	}{
		{"override", map[string]string{EnvOverride: "/o", "XDG_CONFIG_HOME": "/x"}, home, "/o", true},
		{"override trimmed", map[string]string{EnvOverride: "  /o  "}, home, "/o", true},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": "/x"}, home, filepath.Join("/x", "gocode"), true},
		{"home", nil, home, filepath.Join(home, ".config", "gocode"), true},
		{"none", nil, "", "", false},
	}
	for _, c := range cases {
		got, ok := Resolve(envOf(c.env), c.home)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: Resolve = %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
	if got := ResolveDefault(envOf(nil), func() (string, error) { return home, nil }); got != filepath.Join(home, ".config", "gocode") {
		t.Errorf("ResolveDefault from homeDir = %q", got)
	}
	if got := ResolveDefault(nil, func() (string, error) { return "", errors.New("no home") }); got != filepath.Join(".config", "gocode") {
		t.Errorf("ResolveDefault without home = %q", got)
	}
	if got := StatePath("/r"); got != filepath.Join("/r", ".gocode.json") {
		t.Errorf("StatePath = %q", got)
	}
}

// layout is a fake home with the former locations filled in.
type layout struct {
	home, root, legacy string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	l := layout{home: home, root: filepath.Join(home, ".config", "gocode"), legacy: filepath.Join(home, ".gocode")}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

func writeFile(t *testing.T, p, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// fullLegacy fills ~/.gocode and ~/.gocode.json and returns the files
// (relative to ~/.gocode) with their contents.
func fullLegacy(t *testing.T, l layout) map[string]string {
	t.Helper()
	files := map[string]string{
		filepath.Join("projects", "-work-a", "s1.jsonl"):                     `{"type":"user"}` + "\n",
		filepath.Join("projects", "-work-a", "s1", "tool-results", "t1.txt"): "big output",
		filepath.Join("projects", "-work-b", "s2.jsonl"):                     `{"type":"assistant"}` + "\n",
		filepath.Join("file-history", "s1", "0123456789abcdef@v1"):           "old content",
		filepath.Join("file-history", "s1", "snapshots.json"):                `{"snapshots":[]}`,
		filepath.Join("plans", "brave-plan.md"):                              "# Plan\n",
		filepath.Join("agents", "reviewer.md"):                               "---\nname: reviewer\n---\n",
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(l.legacy, rel), body, 0o600)
	}
	// A past modification time that a copy must keep (session lists sort by it).
	old := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(l.legacy, "projects", "-work-a", "s1.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, LegacyStatePath(l.home), `{"theme":"dark","projects":{"/trusted/one":{"hasTrustDialogAccepted":true}}}`, 0o600)
	return files
}

func checkMigrated(t *testing.T, l layout, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(l.root, rel)
		if got := readFile(t, p); got != body {
			t.Errorf("%s = %q, want %q", rel, got, body)
		}
		if runtime.GOOS != "windows" {
			if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", rel, fi.Mode().Perm())
			}
		}
	}
	fi, err := os.Stat(filepath.Join(l.root, "projects", "-work-a", "s1.jsonl"))
	if err != nil || time.Since(fi.ModTime()) < 71*time.Hour {
		t.Errorf("modification time not kept: %v %v", fi, err)
	}
	state := readFile(t, StatePath(l.root))
	if !strings.Contains(state, "/trusted/one") {
		t.Errorf("state file lost the trusted folder: %s", state)
	}
	if _, err := os.Lstat(l.legacy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("~/.gocode still present: %v", err)
	}
	if _, err := os.Lstat(LegacyStatePath(l.home)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("~/.gocode.json still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(l.root, migrateLock)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file left behind: %v", err)
	}
}

func TestMigrateFreshInstallDoesNothing(t *testing.T) {
	l := newLayout(t)
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if res.Notice != "" || len(res.Moved) != 0 || len(res.Kept) != 0 {
		t.Fatalf("fresh install result = %+v", res)
	}
	if _, err := os.Lstat(l.root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("root created without anything to move: %v", err)
	}
}

func TestMigrateMovesEverythingOnce(t *testing.T) {
	l := newLayout(t)
	files := fullLegacy(t, l)
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	want := "gocode: moved data from " + filepath.Join("~", ".gocode") + " to " + filepath.Join("~", ".config", "gocode")
	if res.Notice != want {
		t.Fatalf("notice = %q, want %q", res.Notice, want)
	}
	if len(res.Kept) != 0 {
		t.Fatalf("kept = %v", res.Kept)
	}
	checkMigrated(t, l, files)
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(StatePath(l.root)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("state file mode = %v %v", fi, err)
		}
	}

	// Second start: a no-op, even when an old binary recreated the directory.
	writeFile(t, filepath.Join(l.legacy, "projects", "-work-c", "s3.jsonl"), "x", 0o600)
	if res := Migrate(MigrateOptions{Root: l.root, Home: l.home}); res.Notice != "" || len(res.Moved) != 0 {
		t.Fatalf("second run = %+v", res)
	}
}

func TestMigrateKeepsHomeProjectSettings(t *testing.T) {
	l := newLayout(t)
	writeFile(t, filepath.Join(l.legacy, "settings.json"), `{"model":"x"}`, 0o644)
	writeFile(t, filepath.Join(l.legacy, "plans", "p.md"), "plan", 0o600)
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if res.Notice == "" || len(res.Kept) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, filepath.Join(l.legacy, "settings.json")) != `{"model":"x"}` {
		t.Fatal("home project settings moved")
	}
	if _, err := os.Stat(filepath.Join(l.root, "settings.json")); err == nil {
		t.Fatal("home project settings copied into the root")
	}
	if readFile(t, filepath.Join(l.root, "plans", "p.md")) != "plan" {
		t.Fatal("plans not moved")
	}
	if got := LegacyLeftovers(l.home); len(got) != 0 {
		t.Fatalf("leftovers = %v", got)
	}
}

func TestMigrateClashKeepsTargetAndLeavesSource(t *testing.T) {
	l := newLayout(t)
	writeFile(t, filepath.Join(l.legacy, "projects", "-w", "same.jsonl"), "old", 0o600)
	writeFile(t, filepath.Join(l.legacy, "projects", "-w", "only-old.jsonl"), "only old", 0o600)
	writeFile(t, filepath.Join(l.legacy, "plans", "p.md"), "old plan", 0o600)
	writeFile(t, filepath.Join(l.root, "projects", "-w", "same.jsonl"), "new", 0o600)
	writeFile(t, filepath.Join(l.root, "projects", "-w", "only-new.jsonl"), "only new", 0o600)

	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if got := readFile(t, filepath.Join(l.root, "projects", "-w", "same.jsonl")); got != "new" {
		t.Fatalf("target overwritten: %q", got)
	}
	if got := readFile(t, filepath.Join(l.legacy, "projects", "-w", "same.jsonl")); got != "old" {
		t.Fatalf("clashing source not left in place: %q", got)
	}
	for p, want := range map[string]string{
		filepath.Join(l.root, "projects", "-w", "only-old.jsonl"): "only old",
		filepath.Join(l.root, "projects", "-w", "only-new.jsonl"): "only new",
		filepath.Join(l.root, "plans", "p.md"):                    "old plan",
	} {
		if got := readFile(t, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if len(res.Kept) != 1 || res.Kept[0] != filepath.Join(l.legacy, "projects", "-w", "same.jsonl") {
		t.Fatalf("kept = %v", res.Kept)
	}
	if !strings.Contains(res.Notice, "moved data from") || !strings.Contains(res.Notice, "same.jsonl") {
		t.Fatalf("notice = %q", res.Notice)
	}
	if _, err := os.Lstat(filepath.Join(l.legacy, "plans")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("emptied source directory not removed")
	}
	if got := LegacyLeftovers(l.home); len(got) != 1 || got[0] != "projects" {
		t.Fatalf("leftovers = %v", got)
	}
	// The clash is reported once.
	if res := Migrate(MigrateOptions{Root: l.root, Home: l.home}); res.Notice != "" {
		t.Fatalf("clash reported again: %q", res.Notice)
	}
}

func TestMigrateMergesStateFiles(t *testing.T) {
	l := newLayout(t)
	writeFile(t, LegacyStatePath(l.home), `{
  "theme": "light",
  "numStartups": 4,
  "projects": {
    "/a": {"hasTrustDialogAccepted": true, "lastCost": 1},
    "/both": {"hasTrustDialogAccepted": true, "oldOnly": "x"},
    "/c": {"hasTrustDialogAccepted": false}
  }
}`, 0o600)
	writeFile(t, StatePath(l.root), `{
  "theme": "dark",
  "projects": {
    "/b": {"hasTrustDialogAccepted": true},
    "/both": {"hasTrustDialogAccepted": false, "lastCost": 9},
    "/c": {"hasTrustDialogAccepted": true}
  }
}`, 0o600)
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if res.Notice == "" || len(res.Kept) != 0 {
		t.Fatalf("result = %+v", res)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readFile(t, StatePath(l.root))), &got); err != nil {
		t.Fatal(err)
	}
	if got["theme"] != "dark" || got["numStartups"] != float64(4) {
		t.Errorf("top-level merge = %v", got)
	}
	projects := got["projects"].(map[string]any)
	var trusted []string
	for k, v := range projects {
		if v.(map[string]any)["hasTrustDialogAccepted"] == true {
			trusted = append(trusted, k)
		}
	}
	sort.Strings(trusted)
	if strings.Join(trusted, ",") != "/a,/b,/both,/c" {
		t.Errorf("trusted = %v (projects %v)", trusted, projects)
	}
	both := projects["/both"].(map[string]any)
	if both["lastCost"] != float64(9) || both["oldOnly"] != "x" {
		t.Errorf("/both = %v", both)
	}
	if _, err := os.Lstat(LegacyStatePath(l.home)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("merged ~/.gocode.json not removed: %v", err)
	}
}

func TestMigrateLeavesUnparsableStateFile(t *testing.T) {
	l := newLayout(t)
	writeFile(t, LegacyStatePath(l.home), `not json`, 0o600)
	writeFile(t, StatePath(l.root), `{"projects":{}}`, 0o600)
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if readFile(t, LegacyStatePath(l.home)) != "not json" {
		t.Fatal("unparsable ~/.gocode.json changed")
	}
	if len(res.Kept) != 1 || !strings.Contains(res.Notice, "was not moved") {
		t.Fatalf("result = %+v", res)
	}
}

func TestMigrateCrossDeviceCopies(t *testing.T) {
	l := newLayout(t)
	files := fullLegacy(t, l)
	if err := os.Chmod(filepath.Join(l.legacy, "file-history"), 0o700); err != nil {
		t.Fatal(err)
	}
	renames := 0
	failRename := func(oldpath, newpath string) error {
		renames++
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("invalid cross-device link")}
	}
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home, Rename: failRename})
	if renames == 0 {
		t.Fatal("rename never attempted")
	}
	if res.Notice == "" || len(res.Kept) != 0 {
		t.Fatalf("result = %+v", res)
	}
	checkMigrated(t, l, files)
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(l.root, "file-history")); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("directory mode = %v %v", fi, err)
		}
	}
}

func TestMigrateCopyFailureLeavesSource(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	l := newLayout(t)
	writeFile(t, filepath.Join(l.legacy, "plans", "secret.md"), "x", 0o000)
	failRename := func(oldpath, newpath string) error { return errors.New("cross-device") }
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home, Rename: failRename})
	if _, err := os.Lstat(filepath.Join(l.legacy, "plans", "secret.md")); err != nil {
		t.Fatalf("unreadable source removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(l.root, "plans", "secret.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial copy left in the root: %v", err)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("kept = %v", res.Kept)
	}
}

func TestMigrateConcurrentStartsMoveOnce(t *testing.T) {
	l := newLayout(t)
	files := fullLegacy(t, l)
	const n = 8
	var wg sync.WaitGroup
	notices := make([]string, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			notices[i] = Migrate(MigrateOptions{Root: l.root, Home: l.home}).Notice
		}()
	}
	close(start)
	wg.Wait()
	count := 0
	for _, s := range notices {
		if s != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d processes reported a move, want 1: %q", count, notices)
	}
	checkMigrated(t, l, files)
}

func TestMigrateStaleLockIsTakenOver(t *testing.T) {
	l := newLayout(t)
	files := fullLegacy(t, l)
	writeFile(t, filepath.Join(l.root, migrateLock), "99999\n", 0o600)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(l.root, migrateLock), old, old); err != nil {
		t.Fatal(err)
	}
	if res := Migrate(MigrateOptions{Root: l.root, Home: l.home, LockWait: 100 * time.Millisecond}); res.Notice == "" {
		t.Fatal("stale lock blocked the move")
	}
	checkMigrated(t, l, files)
}

func TestMigrateHeldLockSkips(t *testing.T) {
	l := newLayout(t)
	fullLegacy(t, l)
	writeFile(t, filepath.Join(l.root, migrateLock), "1\n", 0o600)
	if res := Migrate(MigrateOptions{Root: l.root, Home: l.home, LockWait: 50 * time.Millisecond}); res.Notice != "" {
		t.Fatalf("moved while another process held the lock: %+v", res)
	}
	if _, err := os.Stat(l.legacy); err != nil {
		t.Fatal("source touched while locked")
	}
}

func TestMigrateOverrideMovesNothing(t *testing.T) {
	l := newLayout(t)
	fullLegacy(t, l)
	override := filepath.Join(t.TempDir(), "custom")
	root, _ := Resolve(envOf(map[string]string{EnvOverride: override}), l.home)
	if root != override {
		t.Fatalf("override not honoured: %q", root)
	}
	res := Migrate(MigrateOptions{Root: root, Home: l.home, Overridden: true})
	if res.Notice != "" {
		t.Fatalf("moved into the override: %+v", res)
	}
	if _, err := os.Lstat(override); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("override directory written")
	}
	if _, err := os.Stat(filepath.Join(l.legacy, "projects")); err != nil {
		t.Fatal("legacy data touched")
	}
}

func TestMigrateRootInsideLegacyDirMovesNothing(t *testing.T) {
	l := newLayout(t)
	fullLegacy(t, l)
	for _, root := range []string{l.legacy, filepath.Join(l.legacy, "sub"), l.home} {
		if res := Migrate(MigrateOptions{Root: root, Home: l.home}); res.Notice != "" {
			t.Fatalf("root %s: %+v", root, res)
		}
	}
}

func TestMigrateSymlinkedLegacyDirIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	l := newLayout(t)
	real := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, filepath.Join(real, "projects", "x.jsonl"), "x", 0o600)
	if err := os.Symlink(real, l.legacy); err != nil {
		t.Fatal(err)
	}
	res := Migrate(MigrateOptions{Root: l.root, Home: l.home})
	if len(res.Kept) != 1 || !strings.Contains(res.Notice, "was not moved") {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, filepath.Join(real, "projects", "x.jsonl")) != "x" {
		t.Fatal("symlink target changed")
	}
}

func TestMergeStateWithoutProjects(t *testing.T) {
	cur := map[string]any{"a": 1}
	old := map[string]any{"a": 2, "projects": map[string]any{"/x": map[string]any{"hasTrustDialogAccepted": true}}}
	got := MergeState(cur, old)
	b, _ := json.Marshal(got)
	if !bytes.Contains(b, []byte(`"/x":{"hasTrustDialogAccepted":true}`)) || got["a"] != 1 {
		t.Fatalf("merged = %s", b)
	}
}
