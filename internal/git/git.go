// Package git implements gocode git subsystem: how the CLI shells out
// to the `git` binary for status, branch/HEAD detection and worktree
// isolation, and the hardened environment every git child process is launched
// with.
//
// The package contains the git env/argv primitives (the base config args, the
// porcelain status argv and the sanitized-env builder SanitizedEnv), the
// `worktree list --porcelain -z` parser and path helpers (worktree.go), the
// worktree-isolation argv builders, and the `status --porcelain=v2 -z`
// snapshot parser (status.go).
//
// The sanitized env overlaps the internal/envtrust
// (envtrust) package, which owns GitTraceSilencingEnv / GitConfigPassthroughEnv
// and the sensitive-name lists; this package references those names rather than
// re-deriving them.
package git

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	envtrust "github.com/umairge66/gocode/internal/envtrust"
)

// BaseConfigArgs are the `-c` overrides prepended to (almost) every git
// invocation: hooks are routed to /dev/null so a checked-out hook cannot run,
// and the fsmonitor is disabled so no long-lived helper is spawned.
var BaseConfigArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor="}

// IgnoreSubmodulesDirty is the shared `--ignore-submodules` value: a dirty
// submodule working tree does not, on its own, make the superproject dirty.
const IgnoreSubmodulesDirty = "--ignore-submodules=dirty"

// StatusPorcelainArgs is the v1 status probe: a stable, script-facing
// listing that the isolation code treats as "clean" iff stdout is empty.
var StatusPorcelainArgs = []string{"status", "--porcelain", "--untracked-files=normal", IgnoreSubmodulesDirty}

// AllowProtocolDefault is the GIT_ALLOW_PROTOCOL sentinel meaning "no transport
// allowed" — a single slash, which never matches a real transport name.
const AllowProtocolDefault = "/"

// --- argv builders ---------------------------------------------------------
//
// Each builder returns a fresh slice of the subcommand args *without* the
// binary or BaseConfigArgs prefix, so callers compose them as
// append(BaseConfigArgs, builder()...). They are pure and exercised directly by
// the tests.

// CurrentBranchArgs resolves the checked-out branch's short name, or fails
// quietly (exit 1, empty stdout) in detached-HEAD state.
func CurrentBranchArgs() []string {
	return []string{"symbolic-ref", "--short", "-q", "HEAD"}
}

// Result is a git child-process result: the exit code and the captured
// streams.
type Result struct {
	Code   int
	Stdout []byte
	Stderr []byte
}

// Runner shells out to git with the hardened environment. GitPath is the git
// binary; Platform selects case/drive-letter rules for the parsers; BaseEnv is
// the launch environment the sanitized env is layered over.
type Runner struct {
	GitPath  string
	Platform envtrust.Platform
	BaseEnv  envtrust.Env
}

// NewRunner builds a Runner for the current platform. gitPath defaults to "git"
// (PATH lookup) when empty.
func NewRunner(gitPath string, baseEnv envtrust.Env) *Runner {
	if gitPath == "" {
		gitPath = "git"
	}
	return &Runner{GitPath: gitPath, Platform: envtrust.CurrentPlatform(), BaseEnv: baseEnv}
}

// Run executes git with BaseConfigArgs prepended to subArgs, in dir, with the
// sanitized environment. It never returns a non-nil error for a non-zero exit
// (that is reported in Result.Code); err is reserved for a failure to spawn.
//
// The child launches with the fully HardenedEnv (git-var sanitize + the
// envtrust loader/trace/config hardening), as the worktree flows require.
func (r *Runner) Run(ctx context.Context, dir string, subArgs []string, overrides envtrust.Env) (*Result, error) {
	args := append(append([]string{}, BaseConfigArgs...), subArgs...)
	cmd := exec.CommandContext(ctx, r.GitPath, args...)
	cmd.Dir = dir
	cmd.Env = EnvSlice(HardenedEnv(r.BaseEnv, overrides, r.Platform))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := &Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.Code = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	res.Code = 0
	return res, nil
}

// --- sanitized environment -------------------------------------------------

// gitConfigParametersAliases are the GIT_CONFIG_PARAMETERS entries that neuter
// git's network/autocorrect behavior: unknown remotes are aliased to the
// no-op `version` command, bundle-URI fetching is blanked, and autocorrect is
// disabled. The "/" placeholder transport is always present.
var gitConfigParametersAliases = []string{
	"'alias.remote-none=version'",
	"'alias.remote-none.command=version'",
	"'alias.remote-/.command=version'",
	"'fetch.bundleURI='",
	"'help.autocorrect=0'",
}

// baseSanitizedEnv is the fixed hardening layer applied to every git child.
var baseSanitizedEnv = envtrust.Env{
	"GIT_ALLOW_PROTOCOL":     AllowProtocolDefault,
	"GIT_CONFIG_PARAMETERS":  strings.Join(gitConfigParametersAliases, " "),
	"GIT_NO_LAZY_FETCH":      "1",
	"GIT_NO_REPLACE_OBJECTS": "1",
	"GIT_TERMINAL_PROMPT":    "0",
	"GIT_LFS_SKIP_SMUDGE":    "1",
}

// sshDisablingEnv forces any SSH transport attempt to fail fast; it is applied
// unless GIT_ALLOW_PROTOCOL explicitly lists "ssh".
var sshDisablingEnv = envtrust.Env{"GIT_SSH_COMMAND": "false"}

// positionallyClearedEnv are the per-invocation git variables that must never
// leak from the launch environment into a sanitized child (they would redirect
// GIT_DIR, the work tree, the index, config inclusion, etc.). Each is force-set
// to "unset".
var positionallyClearedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_SHALLOW_FILE", "GIT_NAMESPACE", "GIT_IMPLICIT_WORK_TREE",
	"GIT_REFERENCE_BACKEND",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM", "GIT_CONFIG",
}

// gitConfigKeyValueRe matches the indexed GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n
// pairs (case-insensitively), which are stripped from the launch environment so
// a caller's config injection cannot reach the sanitized child.
var gitConfigKeyValueRe = regexp.MustCompile(`(?i)^GIT_CONFIG_(KEY|VALUE)_\d+$`)

// clearSentinel marks a variable that must be removed from the child
// environment. It is distinct from an empty value (which git treats as "set to
// empty" for some variables).
const clearSentinel = "\x00__git_unset__"

// SanitizedEnv builds the launch environment for a git child process:
// the hardening layer (plus SSH-disabling unless ssh is allowed), the caller's
// overrides, with every dangerous or config-injecting variable from the base
// environment scrubbed away.
//
// SanitizedEnv produces ONLY the git-var layer. The worktree/isolation
// callers additionally layer the loader/toolchain/home hardening owned by internal/envtrust; that
// composition is HardenedEnv, which is what Runner uses by default.
func SanitizedEnv(base, overrides envtrust.Env, platform envtrust.Platform) envtrust.Env {
	// Whether ssh is explicitly permitted by the caller's GIT_ALLOW_PROTOCOL.
	sshAllowed := false
	if overrides != nil {
		if v, ok := overrides["GIT_ALLOW_PROTOCOL"]; ok {
			for _, p := range strings.Split(v, ":") {
				if p == "ssh" {
					sshAllowed = true
				}
			}
		}
	}

	// hardening := base hardening layer, plus SSH disabling unless ssh is allowed.
	hardening := envtrust.Env{}
	for k, v := range baseSanitizedEnv {
		hardening[k] = v
	}
	if !sshAllowed {
		for k, v := range sshDisablingEnv {
			hardening[k] = v
		}
	}

	// explicit := hardening overlaid by overrides — the layered overlay the child
	// positively sets.
	explicit := envtrust.Env{}
	mergeInto(explicit, hardening)
	mergeInto(explicit, overrides)
	explicit = normalizeAllowProtocol(explicit)

	// Build the resulting env: start from base, scrub, then apply explicit.
	out := envtrust.Env{}
	if base != nil {
		for k, v := range base {
			out[k] = v
		}
	}

	// Clear the git per-invocation variables unconditionally; an explicit
	// re-set below restores the ones the caller positively provides.
	clearedUpper := map[string]struct{}{}
	for _, name := range positionallyClearedEnv {
		clearedUpper[strings.ToUpper(name)] = struct{}{}
		delete(out, name)
	}
	// Scrub from the base only: variables whose uppercased name is a git
	// per-invocation variable, the GIT_CONFIG_KEY/VALUE_n
	// pairs, and any variable whose uppercased name collides with an
	// explicit key but is not itself literally being set (a case-folded
	// duplicate). Note: the loader/toolchain scrubbing in envtrust is NOT done
	// here — this layer is git-specific.
	explicitUpper := upperKeySet(explicit)
	for k := range collectKeys(base) {
		upper := strings.ToUpper(k)
		if _, isGitVar := clearedUpper[upper]; isGitVar {
			delete(out, k)
			continue
		}
		if gitConfigKeyValueRe.MatchString(k) {
			delete(out, k)
			continue
		}
		if _, collides := explicitUpper[upper]; collides {
			if _, set := explicit[k]; !set {
				delete(out, k)
			}
		}
	}

	// Apply the explicit layer.
	for k, v := range explicit {
		out[k] = v
	}

	return sanitizePath(out, platform)
}

// HardenedEnv is the full isolation environment the worktree/isolation flows
// launch git with: the git-var sanitize layer (SanitizedEnv) composed with the
// loader/trace/config hardening owned by the envtrust package. It is what
// Runner uses by default; SanitizedEnv remains the git-var-only layer.
//
// Layering: the git-var sanitizer (SanitizedEnv) is applied to the live base
// env with envtrust.GitConfigPassthroughEnv and envtrust.GitTraceSilencingEnv
// beneath the caller's overrides. So, on top of the git-var layer:
//
//   - the dangerous loader/toolchain/OpenSSL variables (envtrust.DangerousEnvNames)
//     are kept out of the base env — SanitizedEnv treats them as
//     ordinary non-git vars and would otherwise copy them straight through to
//     the child, so they are stripped here before sanitizing;
//   - GitConfigPassthroughEnv and GitTraceSilencingEnv, both
//     computed from the *live* base env, are folded into the overrides beneath
//     the caller's own overrides (so an explicit caller value still wins).
//     Passthrough deliberately re-introduces GIT_CONFIG_GLOBAL/SYSTEM that the
//     git-var layer scrubs from the base; trace-silencing sets the GIT_TRACE2*
//     family that the git-var layer never touches — neither is double-applied.
func HardenedEnv(base, overrides envtrust.Env, platform envtrust.Platform) envtrust.Env {
	// Strip the dangerous loader/toolchain/OpenSSL variables from the base so
	// the git-var layer cannot carry them through to the git child. Matching is
	// case-insensitive on Windows (its environment is case-folded) and exact
	// elsewhere.
	isDangerous := func(name string) bool {
		if platform == envtrust.Windows {
			upper := strings.ToUpper(name)
			for _, n := range envtrust.DangerousEnvNames {
				if strings.ToUpper(n) == upper {
					return true
				}
			}
			return false
		}
		for _, n := range envtrust.DangerousEnvNames {
			if n == name {
				return true
			}
		}
		return false
	}
	strippedBase := envtrust.Env{}
	for k, v := range base {
		if isDangerous(k) {
			continue
		}
		strippedBase[k] = v
	}

	// Fold the config-passthrough and trace-silencing overrides, both derived
	// from the live base env, beneath the caller's overrides.
	hardenedOverrides := envtrust.Env{}
	mergeInto(hardenedOverrides, envtrust.GitConfigPassthroughEnv(base, platform))
	mergeInto(hardenedOverrides, envtrust.GitTraceSilencingEnv(base, platform))
	mergeInto(hardenedOverrides, overrides)

	return SanitizedEnv(strippedBase, hardenedOverrides, platform)
}

// mergeInto copies src over dst (later wins).
func mergeInto(dst, src envtrust.Env) {
	for k, v := range src {
		dst[k] = v
	}
}

func collectKeys(e envtrust.Env) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range e {
		out[k] = struct{}{}
	}
	return out
}

func upperKeySet(e envtrust.Env) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range e {
		out[strings.ToUpper(k)] = struct{}{}
	}
	return out
}

// normalizeAllowProtocol rebuilds GIT_CONFIG_PARAMETERS from GIT_ALLOW_PROTOCOL:
// each permitted transport name gets the same no-op `version` aliases as the
// base set, so an unexpected remote-helper invocation is defanged. The "/"
// placeholder is filtered out of the transport list.
func normalizeAllowProtocol(e envtrust.Env) envtrust.Env {
	allow, ok := e["GIT_ALLOW_PROTOCOL"]
	if !ok {
		return e
	}
	var transports []string
	for _, t := range strings.Split(allow, ":") {
		if t != "/" {
			transports = append(transports, t)
		}
	}
	// Validity: transport names are letters/digits/"-" only. To keep
	// SanitizedEnv total it fails closed on any other name: an invalid list is
	// replaced by the hardened default "/" (no transport allowed), so no
	// unaliased transport is ever let through.
	valid := regexp.MustCompile(`^[A-Za-z0-9-]*$`)
	for _, t := range transports {
		if !valid.MatchString(t) {
			transports = nil
			e = mergeCopy(e)
			e["GIT_ALLOW_PROTOCOL"] = AllowProtocolDefault
			break
		}
	}
	aliases := append([]string{}, gitConfigParametersAliases...)
	seen := map[string]struct{}{}
	for _, a := range aliases {
		seen[a] = struct{}{}
	}
	for _, t := range transports {
		for _, a := range []string{
			"'alias.remote-" + t + "=version'",
			"'alias.remote-" + t + ".command=version'",
		} {
			if _, ok := seen[a]; !ok {
				seen[a] = struct{}{}
				aliases = append(aliases, a)
			}
		}
	}
	prev := e["GIT_CONFIG_PARAMETERS"]
	base := strings.Join(gitConfigParametersAliases, " ")
	var parts []string
	if prev != "" && prev != base {
		parts = append(parts, prev)
	}
	parts = append(parts, aliases...)
	out := envtrust.Env{}
	mergeInto(out, e)
	out["GIT_CONFIG_PARAMETERS"] = strings.Join(parts, " ")
	return out
}

// mergeCopy returns a shallow copy of e.
func mergeCopy(e envtrust.Env) envtrust.Env {
	out := envtrust.Env{}
	mergeInto(out, e)
	return out
}

// sanitizePath rewrites every PATH-family variable so only absolute entries
// survive; a PATH that would otherwise be empty collapses to "/dev/non-existent"
// semantics via a single "/dev/null" fallback.
func sanitizePath(e envtrust.Env, _ envtrust.Platform) envtrust.Env {
	for k, v := range e {
		if k != "PATH" {
			continue
		}
		var abs []string
		for _, entry := range strings.Split(v, ":") {
			if entry != "" && strings.HasPrefix(entry, "/") {
				abs = append(abs, entry)
			}
		}
		if len(abs) > 0 {
			e[k] = strings.Join(abs, ":")
		} else {
			e[k] = "/dev/null"
		}
	}
	return e
}

// EnvSlice renders a sanitized env as a sorted KEY=VALUE slice suitable for
// exec.Cmd.Env, dropping any clearSentinel-marked entries.
func EnvSlice(e envtrust.Env) []string {
	out := make([]string, 0, len(e))
	for k, v := range e {
		if v == clearSentinel {
			continue
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
