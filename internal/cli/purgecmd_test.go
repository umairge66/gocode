package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/sessionstore"
)

const purgeSID = "11111111-2222-4333-8444-555555555555"

// purgeTree lays out session state for f.cwd and for another project.
func purgeTree(t *testing.T, f *subcmdFixture) (data, other string) {
	t.Helper()
	data = filepath.Join(f.home, ".config", "gocode")
	proj := filepath.Join(data, "projects", sessionstore.EscapeCwd(f.cwd))
	subcmdWriteFile(t, filepath.Join(proj, purgeSID+".jsonl"), `{"type":"user","cwd":"`+f.cwd+`"}`+"\n")
	subcmdWriteFile(t, filepath.Join(proj, "memory", "notes.md"), "m")
	subcmdWriteFile(t, filepath.Join(data, "tasks", purgeSID, "1.json"), "{}")
	subcmdWriteFile(t, filepath.Join(data, "debug", purgeSID+".txt"), "log")
	subcmdWriteFile(t, filepath.Join(data, "file-history", purgeSID, "a"), "x")
	otherCwd := filepath.Join(filepath.Dir(f.cwd), "elsewhere")
	other = filepath.Join(data, "projects", sessionstore.EscapeCwd(otherCwd))
	subcmdWriteFile(t, filepath.Join(other, "99999999-2222-4333-8444-555555555555.jsonl"), "{}\n")
	subcmdWriteFile(t, filepath.Join(data, "history.jsonl"),
		`{"display":"a","project":"`+f.cwd+`"}`+"\n"+
			`{"display":"b","project":"`+otherCwd+`"}`+"\n"+
			`{"display":"c","project":"`+f.cwd+`/sub"}`+"\n")
	subcmdWriteFile(t, filepath.Join(data, ".gocode.json"), `{"theme":"x","projects":{"`+f.cwd+`":{"trusted":true},"`+otherCwd+`":{}}}`)
	return data, other
}

func TestPurgeDryRunListsPlanOnly(t *testing.T) {
	f := newSubcmdFixture(t)
	data, _ := purgeTree(t, f)
	code, out, _ := f.run(t, "purge", f.cwd, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Purge plan for " + f.cwd, "tasks for session " + purgeSID, "debug log for session", "file edit history for session", "project transcripts (.jsonl) and memory/", `config: projects["` + f.cwd + `"]`, "2 prompt(s) typed in this project", "Dry run: 6 item(s) would be deleted."} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "tasks", purgeSID)); err != nil {
		t.Fatal("dry run deleted something")
	}
}

func TestPurgeDeletesOnlyThisProject(t *testing.T) {
	f := newSubcmdFixture(t)
	data, other := purgeTree(t, f)
	code, out, errOut := f.run(t, "purge", f.cwd, "-y")
	if code != 0 || !strings.Contains(out, "Purged 6 item(s) for "+f.cwd+".") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	for _, gone := range []string{filepath.Join(data, "tasks", purgeSID), filepath.Join(data, "debug", purgeSID+".txt"), filepath.Join(data, "file-history", purgeSID), filepath.Join(data, "projects", sessionstore.EscapeCwd(f.cwd))} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists", gone)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other project deleted: %v", err)
	}
	hist := subcmdReadFile(t, filepath.Join(data, "history.jsonl"))
	if strings.Contains(hist, `"a"`) || strings.Contains(hist, `"c"`) || !strings.Contains(hist, `"b"`) {
		t.Errorf("history = %q", hist)
	}
	cfg := subcmdReadFile(t, filepath.Join(f.home, ".config", "gocode", ".gocode.json"))
	if strings.Contains(cfg, `"`+f.cwd+`"`) || !strings.Contains(cfg, "elsewhere") || !strings.Contains(cfg, `"theme"`) {
		t.Errorf("config = %s", cfg)
	}
	if _, err := os.Stat(f.cwd); err != nil {
		t.Fatal("the project directory itself was touched")
	}
}

func TestPurgeConfirmationAndErrors(t *testing.T) {
	f := newSubcmdFixture(t)
	data, _ := purgeTree(t, f)
	f.stdin = "n\n"
	code, _, errOut := f.run(t, "purge", f.cwd)
	if code != 1 || !strings.Contains(errOut, "Aborted.") {
		t.Fatalf("decline exit %d err %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(data, "tasks", purgeSID)); err != nil {
		t.Fatal("declined purge deleted")
	}
	code, _, errOut = f.run(t, "purge", f.cwd, "--all")
	if code != 1 || !strings.Contains(errOut, "Cannot specify both a path and --all.") {
		t.Fatalf("all+path exit %d err %q", code, errOut)
	}
	code, _, errOut = f.run(t, "purge", "--all", "-i")
	if code != 1 || !strings.Contains(errOut, "Cannot use -i/--interactive with --all.") {
		t.Fatalf("all+i exit %d err %q", code, errOut)
	}
	code, _, errOut = f.run(t, "purge", filepath.Join(f.home, "nothing-here"), "-y")
	if code != 1 || !strings.Contains(errOut, "No gocode project state found for") {
		t.Fatalf("nothing exit %d err %q", code, errOut)
	}
}

func TestPurgeInteractiveAndPicker(t *testing.T) {
	f := newSubcmdFixture(t)
	data, _ := purgeTree(t, f)
	// Picker: Enter picks the current directory; then skip the first item
	// and abort on the second.
	f.stdin = "\n2\n4\n"
	code, out, errOut := f.run(t, "purge", "-i")
	if code != 1 || !strings.Contains(out, "Select a project to purge:") || !strings.Contains(errOut, "Aborted. 0 item(s) deleted.") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(data, "tasks", purgeSID)); err != nil {
		t.Fatal("skipped item was deleted")
	}
	// Delete everything that remains with option 3.
	f.stdin = "3\n"
	code, out, _ = f.run(t, "purge", f.cwd, "-i")
	if code != 0 || !strings.Contains(out, "Purged 6/6 item(s)") {
		t.Fatalf("all-remaining exit %d out %q", code, out)
	}
}

func TestPurgeAllAndProjectAlias(t *testing.T) {
	f := newSubcmdFixture(t)
	data, _ := purgeTree(t, f)
	code, out, errOut := f.run(t, "project", "purge", "--all", "--dry-run")
	if code != 0 || !strings.Contains(errOut, "`gocode project purge` is now `gocode purge`") || !strings.Contains(out, "Purge plan for all projects") {
		t.Fatalf("alias exit %d out %q err %q", code, out, errOut)
	}
	code, out, _ = f.run(t, "purge", "--all", "--yes")
	if code != 0 || !strings.Contains(out, "across all projects") {
		t.Fatalf("all exit %d out %q", code, out)
	}
	for _, d := range []string{"projects", "tasks", "debug", "file-history", "history.jsonl"} {
		if _, err := os.Stat(filepath.Join(data, d)); !os.IsNotExist(err) {
			t.Errorf("%s survived --all", d)
		}
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatal("the data directory itself was removed")
	}
}

func TestRemoveUnderRefusesOutsideDataDir(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	outside := filepath.Join(root, "outside")
	subcmdWriteFile(t, filepath.Join(outside, "keep.txt"), "k")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{data, outside, filepath.Join(data, ".."), filepath.Join(outside, "keep.txt")} {
		if err := removeUnder(data, p); err != errOutsideDataDir {
			t.Errorf("removeUnder(%s) = %v", p, err)
		}
	}
	if runtime.GOOS != "windows" {
		// A symlinked component inside the data dir must not steer a delete out.
		if err := os.Symlink(outside, filepath.Join(data, "link")); err != nil {
			t.Fatal(err)
		}
		if err := removeUnder(data, filepath.Join(data, "link", "keep.txt")); err != errOutsideDataDir {
			t.Errorf("symlink escape = %v", err)
		}
		// The link itself is removed as a link.
		if err := removeUnder(data, filepath.Join(data, "link")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("file outside the data dir was deleted")
	}
}
