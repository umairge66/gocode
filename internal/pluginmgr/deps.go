package pluginmgr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/updater"
)

// Plugin dependency install.
//
// A plugin that ships a package.json together with a lockfile gets its
// node_modules installed when the plugin is installed or updated — never at
// session start-up. The lockfile is checked first and rewritten into a scratch
// directory beside the plugin, so only registry packages at exact versions with
// integrity hashes can be fetched; the package manager then runs there with
// install scripts disabled, and the resulting node_modules is moved into the
// plugin.

// DependencyResult is the outcome of a plugin dependency install.
type DependencyResult struct {
	// Ran is true when a package manager was started (or would have been, had
	// starting it not failed).
	Ran bool
	// Error is the warning text ("" on success or when nothing was needed).
	Error string
}

// lockfileInstaller is one row of the lockfile → command table.
type lockfileInstaller struct {
	lockfile string
	command  string
	args     []string
}

// npmCIArgs are the npm ci flags.
var npmCIArgs = []string{"ci", "--ignore-scripts", "--workspaces=false", "--no-audit", "--no-fund"}

// lockfileInstallers is the lockfile → command table, in priority order.
var lockfileInstallers = []lockfileInstaller{
	{lockfile: "bun.lock", command: "bun", args: []string{"install", "--frozen-lockfile", "--ignore-scripts"}},
	{lockfile: "npm-shrinkwrap.json", command: "npm", args: npmCIArgs},
	{lockfile: "package-lock.json", command: "npm", args: npmCIArgs},
	{lockfile: "bun.lockb", command: "bun", args: []string{"install", "--frozen-lockfile", "--ignore-scripts"}},
}

const (
	// depsInstallTimeout is the package-manager timeout.
	depsInstallTimeout = 60 * time.Second
	// npmConfigTimeout is the `npm config get registry` timeout.
	npmConfigTimeout = 30 * time.Second
	// maxManifestBytes caps package.json / lockfile reads.
	maxManifestBytes = 32 << 20
	// depsWorkspaceName / depsWorkspaceVersion name the scratch project.
	depsWorkspaceName    = "plugin-dependencies"
	depsWorkspaceVersion = "0.0.0"
	bunlockbRefusal      = "its bun.lockb is a binary lockfile that cannot be checked; ship a text bun.lock instead"
)

// ignoreScriptsEnv: every package-manager run also gets
// npm_config_ignore_scripts=true on top of the --ignore-scripts flag.
var ignoreScriptsEnv = []string{"npm_config_ignore_scripts=true"}

// CommandRunner runs one package-manager command. It is the seam tests use; the
// default runs the real binary through os/exec.
type CommandRunner func(ctx context.Context, spec CommandSpec) CommandResult

// CommandSpec is one package-manager invocation.
type CommandSpec struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string // full environment
	Timeout time.Duration
}

// CommandResult is what a run produced.
type CommandResult struct {
	Code   int
	Stdout string
	Stderr string
	// Err is set when the command could not be started or was killed.
	Err error
}

// lockfileRefusal is a lockfile or package.json the install will not use.
type lockfileRefusal struct{ msg string }

func (e *lockfileRefusal) Error() string { return e.msg }

func refuse(format string, a ...any) error { return &lockfileRefusal{msg: fmt.Sprintf(format, a...)} }

// InstallDependencies installs the dependencies of the plugin directory dir. It does
// nothing (Ran=false) when the plugin has no package.json, or no supported
// lockfile; a yarn/pnpm lockfile is skipped with a note.
func InstallDependencies(ctx context.Context, dir string, run CommandRunner) DependencyResult {
	if run == nil {
		run = execCommandRunner
	}
	names, err := readDirNames(dir)
	if err != nil {
		return DependencyResult{}
	}
	if !names["package.json"] {
		return DependencyResult{}
	}
	inst, yarnOrPnpm := pickInstaller(names)
	if yarnOrPnpm {
		return DependencyResult{Error: "Skipped: yarn/pnpm lockfiles are not supported (resolution-time hooks bypass --ignore-scripts). Use bun or npm."}
	}
	if inst == nil {
		return DependencyResult{}
	}
	return runDependencyInstall(ctx, dir, *inst, run)
}

func pickInstaller(names map[string]bool) (*lockfileInstaller, bool) {
	for i := range lockfileInstallers {
		if names[lockfileInstallers[i].lockfile] {
			return &lockfileInstallers[i], false
		}
	}
	return nil, names["yarn.lock"] || names["pnpm-lock.yaml"]
}

func readDirNames(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}

func runDependencyInstall(ctx context.Context, dir string, inst lockfileInstaller, run CommandRunner) DependencyResult {
	work, err := scratchDirFor(dir)
	if err != nil {
		return failedInstall(inst.command, err)
	}
	defer os.RemoveAll(work)

	staged, err := stageLockfile(ctx, dir, work, inst, run)
	if err != nil {
		var r *lockfileRefusal
		if errors.As(err, &r) {
			return DependencyResult{Error: "Skipped installing this plugin's dependencies: " + r.msg + "."}
		}
		return failedInstall(inst.command, err)
	}
	if staged.packageCount == 0 {
		return DependencyResult{}
	}
	if err := os.WriteFile(filepath.Join(work, "package.json"), staged.packageJSON, 0o644); err != nil {
		return failedInstall(inst.command, err)
	}
	lockName := "bun.lock"
	if inst.command == "npm" {
		lockName = "package-lock.json"
	}
	if err := writeExclusive(filepath.Join(work, lockName), staged.lockfile); err != nil {
		return failedInstall(inst.command, err)
	}

	env := append(os.Environ(), ignoreScriptsEnv...)
	spec := CommandSpec{Name: inst.command, Args: inst.args, Dir: work, Env: env, Timeout: depsInstallTimeout}
	if inst.command == "npm" {
		spec.Args = npmArgs(work, inst.args)
	} else {
		spec.Env = append(spec.Env, "BUN_INSTALL_CACHE_DIR="+filepath.Join(work, "bun-cache"))
	}
	res := run(ctx, spec)
	if res.Code == 0 && res.Err == nil {
		if err := swapNodeModules(work, dir); err != nil {
			return failedInstall(inst.command, err)
		}
		return DependencyResult{Ran: true}
	}
	out := res.Stderr
	if out == "" {
		out = res.Stdout
	}
	if out == "" && res.Err != nil {
		out = res.Err.Error()
	}
	if out == "" {
		out = "no output"
	}
	return DependencyResult{Ran: true, Error: truncate500(fmt.Sprintf("Plugin dependency install failed (%s): %s", inst.command, oneLine(out)))}
}

// npmArgs builds the npm argv: git is disabled for the run by pointing npm's `git`
// at a path that does not exist.
func npmArgs(work string, args []string) []string {
	return append([]string{"--git=" + filepath.Join(work, "git-is-disabled")}, args...)
}

func failedInstall(command string, err error) DependencyResult {
	return DependencyResult{Ran: true, Error: truncate500(fmt.Sprintf("Plugin dependency install failed (%s): %s", command, oneLine(err.Error())))}
}

func truncate500(s string) string {
	r := []rune(s)
	if len(r) > 500 {
		return string(r[:500])
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// scratchDirFor returns a sibling "<dir>.tmp~<8 hex>" on the same volume,
// so the finished node_modules can be renamed into place.
func scratchDirFor(dir string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	work := filepath.Clean(dir) + ".tmp~" + hex.EncodeToString(b[:])
	if err := os.Mkdir(work, 0o700); err != nil {
		return "", err
	}
	return work, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// swapNodeModules swaps node_modules in: the plugin's previous node_modules is set aside,
// the new one moved in, and the old one restored if that fails.
func swapNodeModules(work, dir string) error {
	dst := filepath.Join(dir, "node_modules")
	prev := filepath.Join(work, "previous-node_modules")
	hadPrev := false
	if err := os.Rename(dst, prev); err == nil {
		hadPrev = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(filepath.Join(work, "node_modules"), dst); err != nil {
		if hadPrev {
			_ = os.Rename(prev, dst)
		}
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("the package manager finished but wrote no node_modules")
		}
		return err
	}
	return nil
}

// stagedLockfile is the output of stageLockfile.
type stagedLockfile struct {
	packageCount int
	packageJSON  []byte
	lockfile     []byte
}

func stageLockfile(ctx context.Context, dir, work string, inst lockfileInstaller, run CommandRunner) (stagedLockfile, error) {
	if inst.lockfile == "bun.lockb" {
		return stagedLockfile{}, refuse(bunlockbRefusal)
	}
	pkg, err := readManifestFile(dir, "package.json")
	if err != nil {
		return stagedLockfile{}, err
	}
	lock, err := readManifestFile(dir, inst.lockfile)
	if err != nil {
		return stagedLockfile{}, err
	}
	if err := writeExclusive(filepath.Join(work, "package.json"), []byte("{}")); err != nil {
		return stagedLockfile{}, err
	}
	registryOrigin := ""
	if strings.Contains(lock, `"http://`) {
		registryOrigin = httpRegistryOrigin(ctx, work, run)
	}
	if inst.command == "npm" {
		return stageNPMLockfile(pkg, lock, inst.lockfile, registryOrigin)
	}
	return stageBunLockfile(pkg, lock, registryOrigin)
}

// httpRegistryOrigin returns the user's npm registry origin when it is a plain
// http registry (a lockfile may then link to it over http), else "".
func httpRegistryOrigin(ctx context.Context, work string, run CommandRunner) string {
	res := run(ctx, CommandSpec{
		Name:    "npm",
		Args:    npmArgs(work, []string{"config", "get", "registry", "--workspaces=false"}),
		Dir:     work,
		Env:     append(os.Environ(), ignoreScriptsEnv...),
		Timeout: npmConfigTimeout,
	})
	if res.Code != 0 || res.Err != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(res.Stdout))
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// readManifestFile reads a regular file of at most 32 MB, named exactly.
func readManifestFile(dir, name string) (string, error) {
	p := filepath.Join(dir, name)
	st, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", refuse("its %s is missing, or not named in lowercase", name)
		}
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", refuse("its %s is not a regular file", name)
	}
	if st.Size() > maxManifestBytes {
		return "", refuse("its %s is not a regular file of at most 32 MB", name)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxManifestBytes {
		return "", refuse("its %s is not a regular file of at most 32 MB", name)
	}
	return strings.TrimPrefix(string(b), "\ufeff"), nil
}

var (
	dependencyKinds       = []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"}
	lockedDependencyKinds = []string{"dependencies", "optionalDependencies", "peerDependencies"}
	flagKeys              = []string{"dev", "optional", "devOptional", "peer"}
	platformKeys          = []string{"os", "cpu", "libc"}
	integrityRe           = regexp.MustCompile(`^(sha512|sha384|sha256|sha1)-[A-Za-z0-9+/]+={0,2}$`)
)

func parseJSONObject(text, name string) (map[string]any, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, refuse("its %s is not valid JSON", name)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, refuse("its %s is not a JSON object", name)
	}
	return m, nil
}

func stageNPMLockfile(pkgText, lockText, lockName, registryOrigin string) (stagedLockfile, error) {
	pkg, err := parseJSONObject(pkgText, "package.json")
	if err != nil {
		return stagedLockfile{}, err
	}
	lock, err := parseJSONObject(lockText, lockName)
	if err != nil {
		return stagedLockfile{}, err
	}
	if v := jsonNumber(lock["lockfileVersion"]); v != 2 && v != 3 {
		return stagedLockfile{}, refuse("its %s is in an old format; regenerate it with npm 7 or later", lockName)
	}
	if _, has := pkg["overrides"]; has {
		return stagedLockfile{}, refuse("its package.json sets overrides, which this install does not apply")
	}
	packages, ok := lock["packages"].(map[string]any)
	root, rootOK := packages[""].(map[string]any)
	if !ok || !rootOK {
		return stagedLockfile{}, refuse("its %s has no packages section with the plugin in it", lockName)
	}
	deps, err := rootDependencies(root, pkg, lockName)
	if err != nil {
		return stagedLockfile{}, err
	}
	rootEntry := map[string]any{"name": depsWorkspaceName, "version": depsWorkspaceVersion}
	for k, v := range deps {
		rootEntry[k] = v
	}
	out := map[string]any{"": rootEntry}
	for _, key := range sortedMapKeys(packages) {
		if key == "" {
			continue
		}
		entry, err := checkNPMEntry(key, packages[key], lockName, registryOrigin)
		if err != nil {
			return stagedLockfile{}, err
		}
		out[key] = entry
	}
	pj := map[string]any{"name": depsWorkspaceName, "version": depsWorkspaceVersion}
	for k, v := range deps {
		pj[k] = v
	}
	lf := map[string]any{"name": depsWorkspaceName, "version": depsWorkspaceVersion, "lockfileVersion": 3, "requires": true, "packages": out}
	pjb, _ := json.Marshal(pj)
	lfb, _ := json.Marshal(lf)
	return stagedLockfile{packageCount: len(out) - 1, packageJSON: pjb, lockfile: lfb}, nil
}

// rootDependencies checks the root: the lockfile root and package.json must list the
// same dependencies; the result is the lockfile's lists (plus a sanitized
// peerDependenciesMeta from package.json).
func rootDependencies(lockRoot, pkg map[string]any, lockName string) (map[string]any, error) {
	locked, err := dependencyLists(lockRoot, dependencyKinds, "its "+lockName)
	if err != nil {
		return nil, err
	}
	declared, err := dependencyLists(pkg, dependencyKinds, "its package.json")
	if err != nil {
		return nil, err
	}
	var extra []string
	if lockName == "bun.lock" {
		extra = []string{"devDependencies"}
	}
	declared = withoutOptionalDuplicates(declared, extra)
	if canonicalDeps(locked) != canonicalDeps(declared) {
		return nil, refuse("its package.json and %s list different dependencies", lockName)
	}
	if meta, ok := pkg["peerDependenciesMeta"].(map[string]any); ok {
		locked["peerDependenciesMeta"] = sanitizePeerMeta(meta)
	}
	return locked, nil
}

// withoutOptionalDuplicates: a name in optionalDependencies is dropped
// from dependencies (and the extra kinds).
func withoutOptionalDuplicates(lists map[string]any, extra []string) map[string]any {
	opt, ok := lists["optionalDependencies"].(map[string]any)
	if !ok {
		return lists
	}
	out := make(map[string]any, len(lists))
	for k, v := range lists {
		out[k] = v
	}
	for _, kind := range append([]string{"dependencies"}, extra...) {
		src, ok := lists[kind].(map[string]any)
		if !ok {
			continue
		}
		kept := map[string]any{}
		for name, spec := range src {
			if _, dup := opt[name]; !dup {
				kept[name] = spec
			}
		}
		out[kind] = kept
	}
	return out
}

// canonicalDeps returns an order-independent rendering of the lists.
func canonicalDeps(lists map[string]any) string {
	var b strings.Builder
	for _, kind := range dependencyKinds {
		b.WriteString(kind)
		b.WriteByte('{')
		if m, ok := lists[kind].(map[string]any); ok {
			for _, k := range sortedMapKeys(m) {
				fmt.Fprintf(&b, "%q:%q,", k, fmt.Sprint(m[k]))
			}
		}
		b.WriteByte('}')
	}
	return b.String()
}

// dependencyLists validates the lists: each present list must map npm package names to
// registry version ranges (or npm:<name>@<range> aliases).
func dependencyLists(obj map[string]any, kinds []string, who string) (map[string]any, error) {
	out := map[string]any{}
	for _, kind := range kinds {
		v, has := obj[kind]
		if !has {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, refuse("%s has an unreadable %s list", who, kind)
		}
		for name, spec := range m {
			if !validPackageName(name) || !validDependencySpec(spec) {
				return nil, refuse("%s has a dependency that is not a registry package and version range: %s", who, clip(name, 120))
			}
		}
		out[kind] = m
	}
	return out, nil
}

var npmAliasRe = regexp.MustCompile(`^npm:(.+)@([^@]*)$`)

func validDependencySpec(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	if m := npmAliasRe.FindStringSubmatch(s); m != nil {
		return validPackageName(m[1]) && validRangeOrTag(m[2])
	}
	return validRangeOrTag(s)
}

// checkNPMEntry checks one non-root package-lock entry.
func checkNPMEntry(key string, v any, lockName, registryOrigin string) (map[string]any, error) {
	who := fmt.Sprintf("its %s entry %q", lockName, clip(key, 120))
	if !validNodeModulesPath(key) {
		return nil, refuse("%s is not a package in node_modules", who)
	}
	n, ok := v.(map[string]any)
	if !ok {
		return nil, refuse("%s is not an object", who)
	}
	if _, has := n["link"]; has {
		return nil, refuse("%s is a folder link or a bundled copy", who)
	}
	if _, has := n["inBundle"]; has {
		return nil, refuse("%s is a folder link or a bundled copy", who)
	}
	ver, ok := n["version"].(string)
	if !ok || !updater.IsValid(ver) {
		return nil, refuse("%s has no exact version", who)
	}
	out := map[string]any{"version": ver}
	if nm, has := n["name"]; has {
		s, ok := nm.(string)
		if !ok || !validPackageName(s) {
			return nil, refuse("%s has a name that is not an npm package name", who)
		}
		out["name"] = s
	}
	if res, has := n["resolved"]; has {
		s, err := checkDownloadLink(res, who, registryOrigin, "npm")
		if err != nil {
			return nil, err
		}
		out["resolved"] = s
		if _, has := n["integrity"]; !has {
			return nil, refuse("%s has no integrity hash for its download link", who)
		}
	}
	if in, has := n["integrity"]; has {
		s, err := checkIntegrity(in, who, n["resolved"] != nil)
		if err != nil {
			return nil, err
		}
		out["integrity"] = s
	}
	lists, err := dependencyLists(n, lockedDependencyKinds, who)
	if err != nil {
		return nil, err
	}
	for k, v := range lists {
		out[k] = v
	}
	for _, f := range flagKeys {
		if b, _ := n[f].(bool); b {
			out[f] = true
		}
	}
	for _, p := range platformKeys {
		if pv, has := n[p]; has {
			if !isStringList(pv) {
				return nil, refuse("%s has an unreadable %s list", who, p)
			}
			out[p] = pv
		}
	}
	if b, has := n["bin"]; has {
		if !validBin(b) {
			return nil, refuse("%s has a bin link that points outside its package or has an invalid command name", who)
		}
		out["bin"] = b
	}
	if meta, ok := n["peerDependenciesMeta"].(map[string]any); ok {
		out["peerDependenciesMeta"] = sanitizePeerMeta(meta)
	}
	return out, nil
}

func stageBunLockfile(pkgText, lockText, registryOrigin string) (stagedLockfile, error) {
	pkg, err := parseJSONObject(pkgText, "package.json")
	if err != nil {
		return stagedLockfile{}, err
	}
	lock, err := parseJSONObject(stripTrailingCommas(lockText), "bun.lock")
	if err != nil {
		return stagedLockfile{}, refuse("its bun.lock is not valid JSON")
	}
	if v := jsonNumber(lock["lockfileVersion"]); v < 0 || v > 2 {
		return stagedLockfile{}, refuse("its bun.lock is in a format this version of gocode does not read")
	}
	if _, has := lock["patchedDependencies"]; has {
		return stagedLockfile{}, refuse("it patches its dependencies (patchedDependencies), which this install does not apply")
	}
	if _, has := pkg["patchedDependencies"]; has {
		return stagedLockfile{}, refuse("it patches its dependencies (patchedDependencies), which this install does not apply")
	}
	ws, ok := lock["workspaces"].(map[string]any)
	root, rootOK := ws[""].(map[string]any)
	if !ok || !rootOK || len(ws) != 1 {
		return stagedLockfile{}, refuse("its bun.lock does not list the plugin as its only workspace")
	}
	packages, ok := lock["packages"].(map[string]any)
	if !ok {
		return stagedLockfile{}, refuse("its bun.lock lists no packages")
	}
	deps, err := rootDependencies(root, pkg, "bun.lock")
	if err != nil {
		return stagedLockfile{}, err
	}
	wsRoot := map[string]any{"name": depsWorkspaceName}
	for k, v := range deps {
		if k != "peerDependenciesMeta" {
			wsRoot[k] = v
		}
	}
	if op, has := root["optionalPeers"]; has {
		if err := checkOptionalPeers(op, "its bun.lock"); err != nil {
			return stagedLockfile{}, err
		}
		wsRoot["optionalPeers"] = op
	}
	outPkgs := map[string]any{}
	for _, key := range sortedMapKeys(packages) {
		e, err := checkBunEntry(key, packages[key], registryOrigin)
		if err != nil {
			return stagedLockfile{}, err
		}
		outPkgs[key] = e
	}
	lf := map[string]any{"lockfileVersion": lock["lockfileVersion"], "workspaces": map[string]any{"": wsRoot}, "packages": outPkgs}
	if cv, ok := lock["configVersion"].(json.Number); ok {
		lf["configVersion"] = cv
	}
	pj := map[string]any{"name": depsWorkspaceName, "version": depsWorkspaceVersion}
	for k, v := range deps {
		pj[k] = v
	}
	pjb, _ := json.Marshal(pj)
	lfb, _ := json.Marshal(lf)
	return stagedLockfile{packageCount: len(outPkgs), packageJSON: pjb, lockfile: lfb}, nil
}

// checkBunEntry checks one bun.lock entry: [name@version, resolved, metadata, integrity].
func checkBunEntry(key string, v any, registryOrigin string) ([]any, error) {
	who := fmt.Sprintf("its bun.lock entry %q", clip(key, 120))
	if !validBunPackagePath(key) {
		return nil, refuse("%s is not a package path", who)
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 4 {
		return nil, refuse("%s is not a registry package", who)
	}
	id, _ := arr[0].(string)
	at := strings.LastIndex(id, "@")
	if at <= 0 || !validPackageName(id[:at]) || !updater.IsValid(id[at+1:]) {
		return nil, refuse("%s is not a registry package at an exact version", who)
	}
	resolved := arr[1]
	rs, isStr := resolved.(string)
	if !isStr || rs != "" {
		if _, err := checkDownloadLink(resolved, who, registryOrigin, "bun"); err != nil {
			return nil, err
		}
	}
	meta, ok := arr[2].(map[string]any)
	if !ok {
		return nil, refuse("%s has unreadable metadata", who)
	}
	outMeta, err := dependencyLists(meta, lockedDependencyKinds, who)
	if err != nil {
		return nil, err
	}
	for _, p := range platformKeys {
		if pv, has := meta[p]; has {
			if _, s := pv.(string); !s && !isStringList(pv) {
				return nil, refuse("%s has an unreadable %s list", who, p)
			}
			outMeta[p] = pv
		}
	}
	if op, has := meta["optionalPeers"]; has {
		if err := checkOptionalPeers(op, who); err != nil {
			return nil, err
		}
		outMeta["optionalPeers"] = op
	}
	if b, has := meta["bin"]; has {
		if !validBin(b) {
			return nil, refuse("%s has a bin link that points outside its package or has an invalid command name", who)
		}
		outMeta["bin"] = b
	}
	if bd, has := meta["binDir"]; has {
		s, ok := bd.(string)
		if !ok || !insidePackagePath(s) {
			return nil, refuse("%s has a bin folder outside its package", who)
		}
		outMeta["binDir"] = s
	}
	integ, err := checkIntegrity(arr[3], who, !(isStr && rs == ""))
	if err != nil {
		return nil, err
	}
	return []any{id, resolved, outMeta, integ}, nil
}

func checkOptionalPeers(v any, who string) error {
	if !isStringList(v) {
		return refuse("%s has an unreadable optionalPeers list", who)
	}
	for _, p := range v.([]any) {
		if !validPackageName(p.(string)) {
			return refuse("%s has an optional peer that is not a registry package: %s", who, clip(p.(string), 120))
		}
	}
	return nil
}

func checkIntegrity(v any, who string, hasLink bool) (string, error) {
	s, ok := v.(string)
	if ok && s == "" {
		if hasLink {
			return "", refuse("%s has no integrity hash for its download link", who)
		}
		return "", refuse("%s has an empty integrity hash", who)
	}
	if !ok {
		return "", refuse("%s has an unreadable integrity hash", who)
	}
	for _, part := range strings.Split(s, " ") {
		if !integrityRe.MatchString(part) {
			return "", refuse("%s has an unreadable integrity hash", who)
		}
	}
	return s, nil
}

// gitHostedHosts are the hosts a package manager may fetch as a git repository.
var gitHostedHosts = map[string]bool{"github.com": true, "gist.github.com": true, "gitlab.com": true, "bitbucket.org": true, "git.sr.ht": true}

func checkDownloadLink(v any, who, registryOrigin, pm string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", refuse("%s has a download link that is not text", who)
	}
	if reason := downloadLinkProblem(s, registryOrigin, pm); reason != "" {
		return "", refuse("%s has a download link that %s", who, reason)
	}
	return s, nil
}

var httpLinkRe = regexp.MustCompile(`(?i)^https?://`)

func downloadLinkProblem(s, registryOrigin, pm string) string {
	u, err := url.Parse(s)
	if !httpLinkRe.MatchString(s) || err != nil || u.Host == "" {
		return "is not an http or https link"
	}
	if strings.ContainsAny(s, " \t\n\r\f\v#\\") || hasControlChar(s) {
		return `contains a "#", whitespace, a backslash or a control character`
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	gitlabAPI := host == "gitlab.com" && strings.HasPrefix(u.Path, "/api/v4/") && strings.Contains(u.Path, "/-/")
	if gitHostedHosts[host] && !gitlabAPI {
		return "is on GitHub, GitLab, Bitbucket or SourceHut, where " + pm + " may fetch it as a git repository and run its setup script"
	}
	if strings.HasPrefix(u.Path, "//") {
		return `starts its path with "//"`
	}
	if strings.EqualFold(u.Scheme, "http") && (u.Scheme+"://"+u.Host) != registryOrigin {
		return "is an unencrypted http link on a host other than your npm registry, so " + pm + " could send your saved registry token to it in the clear"
	}
	return ""
}

func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] == 127 {
			return true
		}
	}
	return false
}

// npmNameRe approximates validate-npm-package-name for new packages (the
// name is lowercased, then checked): optional @scope/, URL-safe characters, no leading
// dot or underscore, at most 214 characters.
var npmNameRe = regexp.MustCompile(`^(?:@[a-z0-9-~][a-z0-9-._~]*/)?[a-z0-9-~][a-z0-9-._~]*$`)

func validPackageName(s string) bool {
	l := strings.ToLower(s)
	return len(l) > 0 && len(l) <= 214 && npmNameRe.MatchString(l) && l != "node_modules" && l != "favicon.ico"
}

func validNodeModulesPath(k string) bool {
	if !strings.HasPrefix(k, "node_modules/") {
		return false
	}
	for _, p := range strings.Split(k[len("node_modules/"):], "/node_modules/") {
		if !validPackageName(p) {
			return false
		}
	}
	return true
}

func validBunPackagePath(k string) bool {
	parts := strings.Split(k, "/")
	for i := 0; i < len(parts); i++ {
		name := parts[i]
		if strings.HasPrefix(name, "@") && i+1 < len(parts) {
			i++
			name += "/" + parts[i]
		}
		if !validPackageName(name) {
			return false
		}
	}
	return true
}

func insidePackagePath(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsAny(s, `\:`) {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == ".." {
			return false
		}
	}
	return true
}

func validBin(v any) bool {
	switch t := v.(type) {
	case string:
		return insidePackagePath(t)
	case map[string]any:
		for name, target := range t {
			ts, ok := target.(string)
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) || !ok || !insidePackagePath(ts) {
				return false
			}
		}
		return true
	}
	return false
}

func sanitizePeerMeta(meta map[string]any) map[string]any {
	out := map[string]any{}
	for name, v := range meta {
		m, ok := v.(map[string]any)
		if !validPackageName(name) || !ok {
			continue
		}
		b, _ := m["optional"].(bool)
		out[name] = map[string]any{"optional": b}
	}
	return out
}

func isStringList(v any) bool {
	arr, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if _, ok := e.(string); !ok {
			return false
		}
	}
	return true
}

func jsonNumber(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return -1
		}
		return int(i)
	case float64:
		return int(n)
	}
	return -1
}

func sortedMapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// stripTrailingCommas removes JSON trailing commas outside strings (bun.lock is
// JSONC with trailing commas but no comments).
func stripTrailingCommas(s string) string {
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

var (
	distTagRe     = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9._-]{0,127}$`)
	tarballSpecRe = regexp.MustCompile(`(?i)[.](?:tgz|tar.gz|tar)$`)
	partialRe     = regexp.MustCompile(`^v?(?:[xX*]|0|[1-9][0-9]*)(?:\.(?:[xX*]|0|[1-9][0-9]*)(?:\.(?:[xX*]|0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)?)?$`)
	comparatorRe  = regexp.MustCompile(`^(<=|>=|<|>|=|~>|~|\^)?\s*(.*)$`)
)

// validRangeOrTag accepts a semver range (node-semver validRange) or a
// dist-tag, but never a tarball file name.
func validRangeOrTag(s string) bool {
	if tarballSpecRe.MatchString(s) {
		return false
	}
	return validSemverRange(s) || distTagRe.MatchString(s)
}

// validSemverRange is a structural check of node-semver's range grammar: sets
// joined by "||", each a hyphen range or whitespace-separated comparators of an
// optional operator and a (partial, x-ranged) version.
func validSemverRange(s string) bool {
	for _, set := range strings.Split(s, "||") {
		set = strings.TrimSpace(set)
		if set == "" {
			continue
		}
		if parts := strings.Split(set, " - "); len(parts) == 2 {
			if !partialRe.MatchString(strings.TrimSpace(parts[0])) || !partialRe.MatchString(strings.TrimSpace(parts[1])) {
				return false
			}
			continue
		}
		// Operators may be separated from their version by spaces (">= 1.2").
		toks := strings.Fields(set)
		for i := 0; i < len(toks); i++ {
			t := toks[i]
			m := comparatorRe.FindStringSubmatch(t)
			ver := m[2]
			if ver == "" && m[1] != "" && i+1 < len(toks) {
				i++
				ver = toks[i]
			}
			if !partialRe.MatchString(ver) {
				return false
			}
		}
	}
	return true
}

// execCommandRunner runs the package manager with the given environment and
// timeout, stdin closed.
func execCommandRunner(ctx context.Context, spec CommandSpec) CommandResult {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = nil
	var out, errb bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, max: 8 << 20}
	cmd.Stderr = &limitedBuffer{buf: &errb, max: 8 << 20}
	err := cmd.Run()
	res := CommandResult{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ctx.Err() == nil:
		res.Code = ee.ExitCode()
	default:
		res.Code = -1
		res.Err = err
		if ctx.Err() != nil {
			res.Err = fmt.Errorf("timed out after %s", spec.Timeout)
		}
	}
	return res
}

// limitedBuffer caps captured output.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
