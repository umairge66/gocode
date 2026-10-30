package settingsload

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// newCtx returns a Context wired to a MemFS-friendly fixed environment: a home
// directory, a working directory and a managed base dir override, with the Env
// hook returning nothing so UserConfigDir falls through to HOME/.config/gocode.
func newCtx() *Context {
	return &Context{
		HomeDir:                "/home/user",
		Cwd:                    "/home/user/project",
		ManagedBaseDirOverride: "/etc/gocode",
		Env:                    func(string) string { return "" },
		// Hermetic: no OS-managed policy unless a test injects one.
		OSPolicyReader: func() OSPolicyReadout { return OSPolicyReadout{} },
	}
}

func TestPathForSource(t *testing.T) {
	ctx := newCtx()
	cases := []struct {
		src  settingspolicy.Source
		want string
		ok   bool
	}{
		{settingspolicy.SourceUser, "/home/user/.config/gocode/settings.json", true},
		{settingspolicy.SourceProject, "/home/user/project/.gocode/settings.json", true},
		{settingspolicy.SourceLocal, "/home/user/project/.gocode/settings.local.json", true},
		{settingspolicy.SourcePolicy, "/etc/gocode/managed-settings.json", true},
		{settingspolicy.SourceFlag, "", false},
	}
	for _, c := range cases {
		got, ok := ctx.PathForSource(c.src)
		if ok != c.ok || got != filepath.FromSlash(c.want) {
			t.Errorf("PathForSource(%s) = %q,%v; want %q,%v", c.src, got, ok, c.want, c.ok)
		}
	}
}

func TestUserConfigDirPrecedence(t *testing.T) {
	// GOCODE_CONFIG_DIR wins outright.
	ctx := &Context{HomeDir: "/home/user", Env: func(k string) string {
		if k == "GOCODE_CONFIG_DIR" {
			return "/custom/cfg"
		}
		return ""
	}}
	if dir, ok := ctx.UserConfigDir(); !ok || dir != "/custom/cfg" {
		t.Fatalf("GOCODE_CONFIG_DIR: got %q,%v", dir, ok)
	}

	// XDG_CONFIG_HOME -> <xdg>/gocode.
	ctx = &Context{HomeDir: "/home/user", Env: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}}
	if dir, ok := ctx.UserConfigDir(); !ok || dir != filepath.FromSlash("/xdg/gocode") {
		t.Fatalf("XDG_CONFIG_HOME: got %q,%v", dir, ok)
	}

	// HOME fallback.
	ctx = &Context{HomeDir: "/home/user", Env: func(string) string { return "" }}
	if dir, ok := ctx.UserConfigDir(); !ok || dir != filepath.FromSlash("/home/user/.config/gocode") {
		t.Fatalf("HOME fallback: got %q,%v", dir, ok)
	}

	// Nothing available.
	ctx = &Context{Env: func(string) string { return "" }}
	if _, ok := ctx.UserConfigDir(); ok {
		t.Fatalf("no dir: expected ok=false")
	}
}

func TestManagedBaseDirPerOS(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  "/Library/Application Support/gocode",
		"windows": "C:\\Program Files\\gocode",
		"linux":   "/etc/gocode",
	} {
		ctx := &Context{GOOS: goos}
		if got := ctx.ManagedBaseDir(); got != want {
			t.Errorf("ManagedBaseDir(%s) = %q; want %q", goos, got, want)
		}
	}
}

// TestLoadAllTiers writes a user + project + local settings file and asserts
// each loads with LoadLoaded and feeds the right tier/source.
func TestLoadAllTiers(t *testing.T) {
	ctx := newCtx()
	fsys := fsfacade.NewMemFS()
	userPath, _ := ctx.PathForSource(settingspolicy.SourceUser)
	projPath, _ := ctx.PathForSource(settingspolicy.SourceProject)
	localPath, _ := ctx.PathForSource(settingspolicy.SourceLocal)

	fsys.WriteFile(userPath, []byte(`{"model":"user-model"}`))
	fsys.WriteFile(projPath, []byte(`{"model":"project-model"}`))
	fsys.WriteFile(localPath, []byte("// local override\n{\"model\":\"local-model\",}"))

	results := LoadAll(fsys, ctx)

	check := func(src settingspolicy.Source, wantModel string) {
		r := results[src]
		if r.LoadState != settingspolicy.LoadLoaded {
			t.Fatalf("%s: loadState = %q; want loaded (errors=%v)", src, r.LoadState, r.Errors)
		}
		if got, _ := r.Settings["model"]; got != wantModel {
			t.Errorf("%s: model = %v; want %q", src, got, wantModel)
		}
	}
	check(settingspolicy.SourceUser, "user-model")
	check(settingspolicy.SourceProject, "project-model")
	check(settingspolicy.SourceLocal, "local-model") // JSONC comment + trailing comma tolerated

	// The policy/managed source is absent (no managed-settings.json written).
	if st := results[settingspolicy.SourcePolicy].LoadState; st != settingspolicy.LoadAbsent {
		t.Errorf("policy source loadState = %q; want absent", st)
	}
}

func TestLoadMalformedAndUnparsable(t *testing.T) {
	ctx := newCtx()
	fsys := fsfacade.NewMemFS()
	projPath, _ := ctx.PathForSource(settingspolicy.SourceProject)
	localPath, _ := ctx.PathForSource(settingspolicy.SourceLocal)

	// Invalid JSON syntax -> unparsable.
	fsys.WriteFile(projPath, []byte(`{"model": `))
	if r := LoadSource(fsys, ctx, settingspolicy.SourceProject); r.LoadState != settingspolicy.LoadUnparsable {
		t.Errorf("bad JSON: loadState = %q; want unparsable", r.LoadState)
	} else if len(r.Errors) == 0 || r.Errors[0].Severity != "fatal" {
		t.Errorf("bad JSON: expected a fatal error, got %v", r.Errors)
	}

	// Valid JSON but not an object -> malformed.
	fsys.WriteFile(localPath, []byte(`[1,2,3]`))
	if r := LoadSource(fsys, ctx, settingspolicy.SourceLocal); r.LoadState != settingspolicy.LoadMalformed {
		t.Errorf("non-object JSON: loadState = %q; want malformed", r.LoadState)
	}
}

func TestLoadAbsentAndEmpty(t *testing.T) {
	ctx := newCtx()
	fsys := fsfacade.NewMemFS()

	// Nothing on disk -> absent.
	if r := LoadSource(fsys, ctx, settingspolicy.SourceUser); r.LoadState != settingspolicy.LoadAbsent {
		t.Errorf("missing file: loadState = %q; want absent", r.LoadState)
	}

	// Empty file -> loaded, empty settings.
	userPath, _ := ctx.PathForSource(settingspolicy.SourceUser)
	fsys.WriteFile(userPath, []byte("   \n  "))
	r := LoadSource(fsys, ctx, settingspolicy.SourceUser)
	if r.LoadState != settingspolicy.LoadLoaded {
		t.Errorf("empty file: loadState = %q; want loaded", r.LoadState)
	}
	if len(r.Settings) != 0 {
		t.Errorf("empty file: settings = %v; want empty", r.Settings)
	}
}

func TestDisklessDisablesSources(t *testing.T) {
	ctx := newCtx()
	ctx.Diskless = true
	for _, src := range []settingspolicy.Source{
		settingspolicy.SourceUser, settingspolicy.SourceProject,
		settingspolicy.SourceLocal, settingspolicy.SourcePolicy,
	} {
		if _, ok := ctx.PathForSource(src); ok {
			t.Errorf("diskless: PathForSource(%s) should be ok=false", src)
		}
	}
}

func TestProjectSourcesNeedProjectDir(t *testing.T) {
	ctx := &Context{HomeDir: "/home/user", Env: func(string) string { return "" }} // no Cwd, no ProjectConfigDir
	if _, ok := ctx.PathForSource(settingspolicy.SourceProject); ok {
		t.Errorf("no project dir: project source should be ok=false")
	}
	// ProjectConfigDir re-enables them even without a Cwd.
	ctx.ProjectConfigDir = "/custom/proj"
	if got, ok := ctx.PathForSource(settingspolicy.SourceLocal); !ok || got != filepath.FromSlash("/custom/proj/.gocode/settings.local.json") {
		t.Errorf("projectConfigDir local: got %q,%v", got, ok)
	}
}

func TestLocalAnchoredAtGitRoot(t *testing.T) {
	ctx := newCtx()
	ctx.Cwd = "/home/user/project/sub/dir"
	ctx.GitRoot = "/home/user/project"
	got, ok := ctx.PathForSource(settingspolicy.SourceLocal)
	if !ok || got != filepath.FromSlash("/home/user/project/.gocode/settings.local.json") {
		t.Errorf("local at git root: got %q,%v", got, ok)
	}
	// Project source stays anchored at the cwd.
	got, _ = ctx.PathForSource(settingspolicy.SourceProject)
	if got != filepath.FromSlash("/home/user/project/sub/dir/.gocode/settings.json") {
		t.Errorf("project at cwd: got %q", got)
	}
}

// TestManagedFileTierAssembly checks managed-settings.json + drop-ins compose into
// ONE file tier: the base file deep-merged with the drop-ins in sorted name
// order (later documents win per key, arrays union), dot-files and non-.json
// entries ignored.
func TestManagedFileTierAssembly(t *testing.T) {
	ctx := newCtx()
	fsys := fsfacade.NewMemFS()
	fsys.WriteFile("/etc/gocode/managed-settings.json", []byte(`{"permissions":{"defaultMode":"plan","deny":["Bash(rm:*)"]},"base":true,"over":"base"}`))
	fsys.WriteFile("/etc/gocode/managed-settings.d/20-b.json", []byte(`{"b":true,"over":"b"}`))
	fsys.WriteFile("/etc/gocode/managed-settings.d/10-a.json", []byte(`{"a":true,"over":"a","permissions":{"deny":["WebFetch"]}}`))
	fsys.WriteFile("/etc/gocode/managed-settings.d/.ignored.json", []byte(`{"ignored":true}`))
	fsys.WriteFile("/etc/gocode/managed-settings.d/notjson.txt", []byte(`ignored`))

	p := ComposePolicy(fsys, ctx)
	tiers, errs := p.Tiers, p.Errors
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(tiers) != 1 {
		t.Fatalf("got %d tiers; want 1 (the merged file tier)", len(tiers))
	}
	ft := tiers[0]
	if ft["base"] != true || ft["a"] != true || ft["b"] != true || ft["ignored"] != nil {
		t.Errorf("file tier keys: %v", ft)
	}
	if ft["over"] != "b" {
		t.Errorf("later drop-in must win: over=%v", ft["over"])
	}
	deny, _ := settingspolicy.GetPath(ft, []string{"permissions", "deny"})
	if !reflect.DeepEqual(deny, []any{"Bash(rm:*)", "WebFetch"}) {
		t.Errorf("deny arrays must union: %v", deny)
	}
	if mode, _ := settingspolicy.GetPath(ft, []string{"permissions", "defaultMode"}); mode != "plan" {
		t.Errorf("nested keys merge key-wise: %v", ft["permissions"])
	}

	// The policy source is the composition.
	res := LoadSource(fsys, ctx, settingspolicy.SourcePolicy)
	if res.LoadState != settingspolicy.LoadLoaded || res.Settings["a"] != true || res.Settings["base"] != true {
		t.Fatalf("policy source = %+v", res)
	}
}

// TestEffectiveSourcesPrecedence confirms sources come back in canonical
// low->high order with flag/policy always appended.
func TestEffectiveSourcesPrecedence(t *testing.T) {
	ctx := newCtx()
	fsys := fsfacade.NewMemFS()
	ordered := EffectiveSources(fsys, ctx, []settingspolicy.Source{settingspolicy.SourceUser})

	var got []settingspolicy.Source
	for _, o := range ordered {
		got = append(got, o.Source)
	}
	// user is allowed; flag+policy are always added. project/local are not
	// allowed here, so they are excluded. Canonical order is preserved.
	want := []settingspolicy.Source{
		settingspolicy.SourceUser,
		settingspolicy.SourceFlag,
		settingspolicy.SourcePolicy,
	}
	if len(got) != len(want) {
		t.Fatalf("effective order = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("effective order = %v; want %v", got, want)
		}
	}
}

func TestRawKeyPresence(t *testing.T) {
	ctx := newCtx()
	allowed := []settingspolicy.Source{
		settingspolicy.SourceUser, settingspolicy.SourceProject, settingspolicy.SourceLocal,
	}

	// present: key in the project source.
	fsys := fsfacade.NewMemFS()
	projPath, _ := ctx.PathForSource(settingspolicy.SourceProject)
	fsys.WriteFile(projPath, []byte(`{"model":"x"}`))
	store := NewStore(fsys, ctx, allowed)
	if v, _ := store.RawKeyPresence("model"); v != "present" {
		t.Errorf("present: got %q", v)
	}
	if v, _ := store.RawKeyPresence("missing"); v != "absent" {
		t.Errorf("absent: got %q", v)
	}

	// unknowable: a source is present but unparsable.
	fsys2 := fsfacade.NewMemFS()
	userPath, _ := ctx.PathForSource(settingspolicy.SourceUser)
	fsys2.WriteFile(userPath, []byte(`{bad json`))
	store2 := NewStore(fsys2, ctx, allowed)
	if v, _ := store2.RawKeyPresence("model"); v != "unknowable" {
		t.Errorf("unknowable: got %q", v)
	}

	// user-source attribution keys are not counted as present.
	fsys3 := fsfacade.NewMemFS()
	fsys3.WriteFile(userPath, []byte(`{"attribution":true}`))
	store3 := NewStore(fsys3, ctx, allowed)
	if v, _ := store3.RawKeyPresence("attribution"); v != "absent" {
		t.Errorf("attribution exclusion: got %q", v)
	}
}
