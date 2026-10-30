package pluginmgr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testPackageJSON = `{"name":"my-plugin","version":"1.0.0","dependencies":{"left-pad":"^1.3.0"}}`

const testPackageLock = `{
  "name": "my-plugin", "version": "1.0.0", "lockfileVersion": 3, "requires": true,
  "packages": {
    "": {"name": "my-plugin", "version": "1.0.0", "dependencies": {"left-pad": "^1.3.0"}},
    "node_modules/left-pad": {
      "version": "1.3.0",
      "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
      "integrity": "sha512-XI5MPzVNApjAyhQzphX8BkmKsKUxD4LdyK24iZeQ9MqQ6KXyw2xjp4H8Xb/FxRyIAeU5fWX5nQhs3mIvsOJM0w==",
      "license": "WTFPL",
      "bin": {"lp": "bin/lp.js"}
    }
  }
}`

type fakeRunner struct {
	mu    sync.Mutex
	calls []CommandSpec
	fn    func(spec CommandSpec) CommandResult
}

func (f *fakeRunner) run(_ context.Context, spec CommandSpec) CommandResult {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(spec)
	}
	return CommandResult{}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallDependenciesNoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	if res := InstallDependencies(context.Background(), dir, r.run); res.Ran || res.Error != "" || len(r.calls) != 0 {
		t.Fatalf("res=%+v calls=%v", res, r.calls)
	}
	// package.json without a lockfile: nothing to do either.
	writeFiles(t, dir, map[string]string{"package.json": testPackageJSON})
	if res := InstallDependencies(context.Background(), dir, r.run); res.Ran || len(r.calls) != 0 {
		t.Fatalf("res=%+v calls=%v", res, r.calls)
	}
}

func TestInstallDependenciesYarnSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"package.json": testPackageJSON, "yarn.lock": "x"})
	r := &fakeRunner{}
	res := InstallDependencies(context.Background(), dir, r.run)
	if res.Ran || !strings.Contains(res.Error, "yarn/pnpm lockfiles are not supported") || len(r.calls) != 0 {
		t.Fatalf("res=%+v", res)
	}
}

func TestInstallDependenciesNPMCi(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	writeFiles(t, dir, map[string]string{
		"package.json":             testPackageJSON,
		"package-lock.json":        testPackageLock,
		"node_modules/old/old.txt": "previous",
	})
	var stagedPkg, stagedLock map[string]any
	r := &fakeRunner{fn: func(spec CommandSpec) CommandResult {
		// The scratch dir is a sibling of the plugin dir.
		if filepath.Dir(spec.Dir) != filepath.Dir(dir) || !strings.HasPrefix(filepath.Base(spec.Dir), "plugin.tmp~") {
			t.Errorf("work dir %s is not a sibling scratch dir", spec.Dir)
		}
		b, _ := os.ReadFile(filepath.Join(spec.Dir, "package.json"))
		_ = json.Unmarshal(b, &stagedPkg)
		b, _ = os.ReadFile(filepath.Join(spec.Dir, "package-lock.json"))
		_ = json.Unmarshal(b, &stagedLock)
		writeFiles(t, spec.Dir, map[string]string{"node_modules/left-pad/index.js": "module.exports=1"})
		return CommandResult{}
	}}
	res := InstallDependencies(context.Background(), dir, r.run)
	if !res.Ran || res.Error != "" {
		t.Fatalf("res=%+v", res)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls=%v", r.calls)
	}
	c := r.calls[0]
	want := []string{"--git=" + filepath.Join(c.Dir, "git-is-disabled"), "ci", "--ignore-scripts", "--workspaces=false", "--no-audit", "--no-fund"}
	if c.Name != "npm" || strings.Join(c.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %s %v", c.Name, c.Args)
	}
	if !containsEnv(c.Env, "npm_config_ignore_scripts=true") {
		t.Fatal("npm_config_ignore_scripts=true not set")
	}
	if stagedPkg["name"] != "plugin-dependencies" || stagedLock["name"] != "plugin-dependencies" {
		t.Fatalf("staged project not renamed: %v / %v", stagedPkg["name"], stagedLock["name"])
	}
	entry := stagedLock["packages"].(map[string]any)["node_modules/left-pad"].(map[string]any)
	if _, has := entry["license"]; has {
		t.Error("unlisted lockfile keys must be dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "left-pad", "index.js")); err != nil {
		t.Fatalf("node_modules not moved in: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "old")); !os.IsNotExist(err) {
		t.Fatal("previous node_modules should be replaced")
	}
	// The scratch dir is gone.
	matches, _ := filepath.Glob(dir + ".tmp~*")
	if len(matches) != 0 {
		t.Fatalf("scratch dir left behind: %v", matches)
	}
}

func TestInstallDependenciesFailureKeepsOldModules(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")
	writeFiles(t, dir, map[string]string{
		"package.json":             testPackageJSON,
		"package-lock.json":        testPackageLock,
		"node_modules/old/old.txt": "previous",
	})
	r := &fakeRunner{fn: func(CommandSpec) CommandResult {
		return CommandResult{Code: 1, Stderr: "npm ERR! network\n  boom"}
	}}
	res := InstallDependencies(context.Background(), dir, r.run)
	if !res.Ran || res.Error != "Plugin dependency install failed (npm): npm ERR! network boom" {
		t.Fatalf("res=%+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "old", "old.txt")); err != nil {
		t.Fatal("old node_modules must survive a failed install")
	}
}

func TestInstallDependenciesRefusesUnsafeLockfiles(t *testing.T) {
	cases := map[string]struct {
		pkg, lock, want string
	}{
		"git host link": {testPackageJSON, strings.Replace(testPackageLock, "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz", "https://github.com/x/left-pad/archive/main.tar.gz", 1),
			"is on GitHub, GitLab, Bitbucket or SourceHut"},
		"no integrity": {testPackageJSON, strings.Replace(testPackageLock, `"integrity": "sha512-XI5MPzVNApjAyhQzphX8BkmKsKUxD4LdyK24iZeQ9MqQ6KXyw2xjp4H8Xb/FxRyIAeU5fWX5nQhs3mIvsOJM0w==",`, "", 1),
			"has no integrity hash for its download link"},
		"folder link": {testPackageJSON, strings.Replace(testPackageLock, `"version": "1.3.0",`, `"version": "1.3.0", "link": true,`, 1),
			"is a folder link or a bundled copy"},
		"range version": {testPackageJSON, strings.Replace(testPackageLock, `"version": "1.3.0",`, `"version": "^1.3.0",`, 1),
			"has no exact version"},
		"old lockfile": {testPackageJSON, strings.Replace(testPackageLock, `"lockfileVersion": 3`, `"lockfileVersion": 1`, 1),
			"is in an old format"},
		"overrides": {`{"name":"p","dependencies":{"left-pad":"^1.3.0"},"overrides":{"x":"1"}}`, testPackageLock,
			"sets overrides"},
		"mismatch": {`{"name":"p","dependencies":{"left-pad":"^2.0.0"}}`, testPackageLock,
			"list different dependencies"},
		"http link": {testPackageJSON, strings.Replace(testPackageLock, "https://registry.npmjs.org", "http://evil.example", 1),
			"is an unencrypted http link"},
		"bin escape": {testPackageJSON, strings.Replace(testPackageLock, `"bin/lp.js"`, `"../../x"`, 1),
			"has a bin link that points outside its package"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"package.json": c.pkg, "package-lock.json": c.lock})
			r := &fakeRunner{fn: func(spec CommandSpec) CommandResult {
				if len(spec.Args) > 1 && spec.Args[1] == "config" {
					return CommandResult{Stdout: "https://registry.npmjs.org/\n"}
				}
				t.Fatalf("the package manager must not run for a refused lockfile: %v", spec.Args)
				return CommandResult{}
			}}
			res := InstallDependencies(context.Background(), dir, r.run)
			if res.Ran || !strings.HasPrefix(res.Error, "Skipped installing this plugin's dependencies: ") || !strings.Contains(res.Error, c.want) {
				t.Fatalf("res=%+v, want %q", res, c.want)
			}
		})
	}
}

func TestInstallDependenciesHTTPRegistryAllowed(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json":      testPackageJSON,
		"package-lock.json": strings.Replace(testPackageLock, "https://registry.npmjs.org", "http://npm.internal:4873", 1),
	})
	r := &fakeRunner{fn: func(spec CommandSpec) CommandResult {
		if spec.Args[1] == "config" {
			return CommandResult{Stdout: "http://npm.internal:4873/\n"}
		}
		writeFiles(t, spec.Dir, map[string]string{"node_modules/left-pad/index.js": "x"})
		return CommandResult{}
	}}
	res := InstallDependencies(context.Background(), dir, r.run)
	if !res.Ran || res.Error != "" || len(r.calls) != 2 {
		t.Fatalf("res=%+v calls=%d", res, len(r.calls))
	}
	if strings.Join(r.calls[0].Args[1:], " ") != "config get registry --workspaces=false" {
		t.Fatalf("registry probe argv = %v", r.calls[0].Args)
	}
}

func TestInstallDependenciesBun(t *testing.T) {
	dir := t.TempDir()
	lock := `{
  "lockfileVersion": 1,
  "workspaces": {"": {"name": "p", "dependencies": {"left-pad": "^1.3.0"},},},
  "packages": {
    "left-pad": ["left-pad@1.3.0", "", {}, "sha512-XI5MPzVNApjAyhQzphX8BkmKsKUxD4LdyK24iZeQ9MqQ6KXyw2xjp4H8Xb/FxRyIAeU5fWX5nQhs3mIvsOJM0w=="],
  },
}`
	writeFiles(t, dir, map[string]string{"package.json": testPackageJSON, "bun.lock": lock})
	r := &fakeRunner{fn: func(spec CommandSpec) CommandResult {
		writeFiles(t, spec.Dir, map[string]string{"node_modules/left-pad/index.js": "x"})
		return CommandResult{}
	}}
	res := InstallDependencies(context.Background(), dir, r.run)
	if !res.Ran || res.Error != "" {
		t.Fatalf("res=%+v", res)
	}
	c := r.calls[0]
	if c.Name != "bun" || strings.Join(c.Args, " ") != "install --frozen-lockfile --ignore-scripts" {
		t.Fatalf("argv = %s %v", c.Name, c.Args)
	}
	if !containsEnv(c.Env, "BUN_INSTALL_CACHE_DIR="+filepath.Join(c.Dir, "bun-cache")) || !containsEnv(c.Env, "npm_config_ignore_scripts=true") {
		t.Fatal("bun env not set")
	}
	// bun.lockb is refused.
	dir2 := t.TempDir()
	writeFiles(t, dir2, map[string]string{"package.json": testPackageJSON, "bun.lockb": "\x00bin"})
	res = InstallDependencies(context.Background(), dir2, (&fakeRunner{}).run)
	if !strings.Contains(res.Error, "bun.lockb is a binary lockfile") {
		t.Fatalf("res=%+v", res)
	}
}

func TestValidSemverRange(t *testing.T) {
	good := []string{"^1.2.3", "~1.2", "1.x", "*", "", ">=1.0.0 <2.0.0", "1.2.3 - 2.3.4", "^1 || ^2", ">= 1.2", "latest", "next"}
	bad := []string{"file:../x", "git+https://x", "pkg.tgz", "https://x/y", "../x"}
	for _, s := range good {
		if !validRangeOrTag(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if validRangeOrTag(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
