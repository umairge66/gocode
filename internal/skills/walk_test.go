package skills

import (
	"reflect"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
)

func writeSkill(fs *fsfacade.MemFS, dir, body string) {
	fs.MkdirAll(dir)
	fs.WriteFile(dir+"/SKILL.md", []byte(body))
}

// TestProjectSkillsFromParents: project skills are read from cwd up to the
// git root, nearest first; nothing above the root (or in home) is read.
func TestProjectSkillsFromParents(t *testing.T) {
	fs := fsfacade.NewMemFS()
	writeSkill(fs, "/home/u/repo/.gocode/skills/shared", "---\ndescription: root shared\n---\nb")
	writeSkill(fs, "/home/u/repo/.gocode/skills/rootonly", "---\ndescription: root only\n---\nb")
	writeSkill(fs, "/home/u/repo/.claude/skills/cl", "---\ndescription: claude root\n---\nb")
	writeSkill(fs, "/home/u/repo/apps/web/.gocode/skills/shared", "---\ndescription: web shared\n---\nb")
	writeSkill(fs, "/home/u/.gocode/skills/above", "---\ndescription: above root\n---\nb")

	reg := Load(fs, LoadConfig{Cwd: "/home/u/repo/apps/web", ProjectRoot: "/home/u/repo"})
	if d, ok := reg.Lookup("shared"); !ok || d.Description != "web shared" {
		t.Errorf("shared = %+v, want the nearest (web) copy", d)
	}
	for _, n := range []string{"rootonly", "cl"} {
		if _, ok := reg.Lookup(n); !ok {
			t.Errorf("%s from the git root missing", n)
		}
	}
	if _, ok := reg.Lookup("above"); ok {
		t.Error("a skill above the git root was read")
	}

	// No repo: the walk stops below home.
	reg = Load(fs, LoadConfig{Cwd: "/home/u/repo/apps/web", Home: "/home/u"})
	if _, ok := reg.Lookup("rootonly"); !ok {
		t.Error("parent skill missing without a git root")
	}
	if _, ok := reg.Lookup("above"); ok {
		t.Error("home's own .gocode/skills must not be a parent project")
	}

	got := projectSkillDirs("/a/b/c", "/a/b", "")
	if want := []string{"/a/b/c", "/a/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dirs = %v, want %v", got, want)
	}
	// A root cwd is not inside is ignored; the walk then reaches /.
	if got := projectSkillDirs("/x/y", "/a", ""); !reflect.DeepEqual(got, []string{"/x/y", "/x", "/"}) {
		t.Errorf("dirs outside root = %v", got)
	}
	if got := projectSkillDirs("/home/u", "", "/home/u"); !reflect.DeepEqual(got, []string{"/home/u"}) {
		t.Errorf("cwd = home dirs = %v", got)
	}
}

// TestListingWhenToUseConditionalDenied: when_to_use joins the listing line;
// a `paths:` skill is listed once a matching file was touched; a denied skill
// is never listed.
func TestListingWhenToUseConditionalDenied(t *testing.T) {
	fs := fsfacade.NewMemFS()
	writeSkill(fs, "/proj/.gocode/skills/review", "---\ndescription: Review the diff\nwhen_to_use: before a commit\n---\nb")
	writeSkill(fs, "/proj/.gocode/skills/gostyle", "---\ndescription: Go style\npaths: \"**/*.{go,mod}\"\n---\nb")
	writeSkill(fs, "/proj/.gocode/skills/nope", "---\ndescription: denied\n---\nb")

	reg := Load(fs, LoadConfig{Cwd: "/proj", ProjectRoot: "/proj"})
	reg.Denied = func(name string) bool { return name == "nope" }
	if got, want := reg.Listing(), "- review: Review the diff - before a commit (project)"; got != want {
		t.Fatalf("listing = %q, want %q", got, want)
	}
	if d, _ := reg.Lookup("gostyle"); !reflect.DeepEqual(d.Paths, []string{"**/*.go", "**/*.mod"}) {
		t.Errorf("paths = %v", d.Paths)
	}

	if got := reg.ActivateForPath("/proj/README.md", "/proj"); len(got) != 0 {
		t.Errorf("README activated %v", got)
	}
	if got := reg.ActivateForPath("/elsewhere/x.go", "/proj"); len(got) != 0 {
		t.Errorf("a file outside the project activated %v", got)
	}
	if got := reg.ActivateForPath("/proj/pkg/x.go", "/proj"); !reflect.DeepEqual(got, []string{"gostyle"}) {
		t.Fatalf("activated = %v", got)
	}
	if got := reg.ActivateForPath("/proj/pkg/y.go", "/proj"); len(got) != 0 {
		t.Errorf("re-activation reported %v", got)
	}
	if l := reg.Listing(); !strings.Contains(l, "- gostyle: Go style (project)") || strings.Contains(l, "nope") {
		t.Errorf("listing after activation = %q", l)
	}
}
