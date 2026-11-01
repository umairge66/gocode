package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/updater"
)

// This file wires `gocode update` (alias `upgrade`, also reached via the
// `--update` / `--upgrade` rewrite) and `gocode install [target]` to the
// GitHub-Releases updater in internal/updater.
//
//	gocode update [--check] [--channel stable|latest] [--force]
//	gocode install [stable|latest|<version>] [--force]
//
// update checks the configured repository; when a newer release exists it
// prints "current → latest", downloads the platform asset, verifies the
// release's checksums.txt.sig (Ed25519, minisign format) against the
// compiled-in updater.ReleasePublicKeys, verifies the asset against the now
// trusted checksums.txt, atomically replaces the running executable and
// restarts into it. --check only reports. install resolves a channel or an
// exact version tag (downgrades allowed) and installs it the same way. Output
// is plain lines; errors go to stderr with a non-zero exit. A build without a
// compiled-in signing key refuses to install (see scripts/RELEASE-SIGNING.md);
// there is deliberately no flag to skip signature verification.

// updateAPIURLEnvVar overrides the GitHub API base (GitHub Enterprise Server).
const updateAPIURLEnvVar = "GOCODE_UPDATE_API_URL"

// updateRestartedEnvVar marks the process re-executed after a successful
// update, so it reports the new version instead of checking again.
const updateRestartedEnvVar = "GOCODE_UPDATE_RESTARTED"

// Injectable seams for tests.
var (
	updateHTTPClient         updater.Doer = http.DefaultClient
	updateExecutable                      = updater.ExecutablePath
	updateCurrentVersion                  = func() string { return updater.CurrentVersion }
	updateSettingsRepo                    = loadUpdateRepo
	updateGOOS, updateGOARCH              = "", "" // empty → runtime values
	updateFileExists                      = func(p string) bool { _, err := os.Stat(p); return err == nil }
	updateRestart                         = func(exe string) error {
		r := &updater.Restarter{Env: append(os.Environ(), updateRestartedEnvVar+"=1")}
		return r.Restart(exe)
	}
)

type updateFlags struct {
	check   bool
	force   bool
	channel string
	target  string
}

func parseUpdateFlags(verb string, rest []string) (updateFlags, error) {
	var f updateFlags
	seenVerb := false
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		key, val, hasVal := strings.Cut(x, "=")
		switch {
		case !seenVerb && (x == verb || (verb == "update" && x == "upgrade")):
			seenVerb = true
		case key == "--check" && !hasVal && verb == "update":
			f.check = true
		case key == "--force" && !hasVal:
			f.force = true
		case key == "--channel" && verb == "update":
			if !hasVal {
				if i+1 >= len(rest) {
					return f, errors.New("--channel requires a value (stable or latest)")
				}
				i++
				val = rest[i]
			}
			f.channel = val
		case x == "-p" || x == "--print" || x == "--verbose":
			// output is always plain lines
		case strings.HasPrefix(x, "-"):
			return f, fmt.Errorf("unknown option %s", x)
		case verb == "install" && f.target == "":
			f.target = x
		default:
			return f, fmt.Errorf("unexpected argument %q", x)
		}
	}
	switch f.channel {
	case "", updater.ChannelStable, updater.ChannelLatest:
	default:
		return f, fmt.Errorf("invalid channel %q; use %q or %q", f.channel, updater.ChannelStable, updater.ChannelLatest)
	}
	return f, nil
}

// runUpdateCmd implements `gocode update` and `gocode install`.
func runUpdateCmd(env *Env, verb string, rest []string) int {
	errf := func(format string, a ...any) int {
		fmt.Fprintf(env.Stderr, "gocode %s: %s\n", verb, fmt.Sprintf(format, a...))
		return 1
	}
	if env.Getenv(updateRestartedEnvVar) == "1" {
		fmt.Fprintf(env.Stdout, "gocode is now running version %s.\n", updateCurrentVersion())
		return 0
	}

	f, err := parseUpdateFlags(verb, rest)
	if err != nil {
		return errf("%v", err)
	}

	repo, _, err := updater.ResolveRepo(env.Getenv, updateSettingsRepo(env))
	if err != nil {
		return errf("%v", strings.TrimPrefix(err.Error(), "updater: "))
	}

	// Installing requires a compiled-in release signing key: refuse up front,
	// before any network access, rather than after the version check.
	// (--check only reports and stays available.)
	if !f.check {
		if _, err := updater.CompiledKeys(); err != nil {
			return errf("%s", strings.TrimPrefix(err.Error(), "updater: "))
		}
	}

	client := &updater.Client{
		HTTP:       updateHTTPClient,
		Repo:       repo,
		APIBaseURL: strings.TrimSpace(env.Getenv(updateAPIURLEnvVar)),
		Token:      updater.TokenFromEnv(env.Getenv),
	}
	u := &updater.Updater{
		Client:  client,
		Current: updateCurrentVersion(),
		GOOS:    updateGOOS,
		GOARCH:  updateGOARCH,
	}

	opts := updater.CheckOptions{Channel: f.channel, Force: f.force}
	if verb == "install" {
		switch f.target {
		case "", updater.ChannelStable:
			opts.Channel = updater.ChannelStable
		case updater.ChannelLatest:
			opts.Channel = updater.ChannelLatest
		default:
			if !updater.IsValid(f.target) {
				return errf("invalid target %q; use stable, latest, or a version like 1.2.3", f.target)
			}
			opts.Tag = f.target
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, err := u.Check(ctx, opts)
	if err != nil {
		return errf("%s", strings.TrimPrefix(err.Error(), "updater: "))
	}

	fmt.Fprintf(env.Stdout, "Current version: %s\n", res.Current)
	fmt.Fprintf(env.Stdout, "Latest version:  %s (%s, %s)\n", res.Latest, repo, describeTarget(opts))

	if res.DevBuild && !res.Available {
		fmt.Fprintf(env.Stdout, "%s; use --force to install %s anyway.\n", capitalize(updater.DevBuildReason), res.Latest)
		if f.check {
			return 0
		}
		return 1
	}
	if !res.Available {
		fmt.Fprintf(env.Stdout, "gocode is up to date (%s).\n", res.Reason)
		return 0
	}
	// An install a package manager owns is updated through it, never
	// overwritten in place.
	exe, exeErr := updateExecutable()
	if exeErr == nil {
		goos := updateGOOS
		if goos == "" {
			goos = runtime.GOOS
		}
		if m, ok := updater.ManagedInstall(exe, goos, repo, env.Getenv, updateFileExists); ok {
			fmt.Fprintf(env.Stdout, "Update available: %s → %s.\n", res.Current, res.Latest)
			fmt.Fprintf(env.Stdout, "gocode was installed by %s, so it is updated there: %s\n", m.By, m.Upgrade)
			if f.check {
				return 0
			}
			return 1
		}
	}
	if f.check {
		fmt.Fprintf(env.Stdout, "Update available: %s → %s. Run 'gocode update' to install.\n", res.Current, res.Latest)
		return 0
	}

	fmt.Fprintf(env.Stdout, "Updating gocode: %s → %s\n", res.Current, res.Latest)
	if err := exeErr; err != nil {
		return errf("%s", strings.TrimPrefix(err.Error(), "updater: "))
	}
	u.Executable = exe
	installed, err := u.Apply(ctx, res.Release)
	if err != nil {
		return errf("%s", strings.TrimPrefix(err.Error(), "updater: "))
	}
	fmt.Fprintf(env.Stdout, "Verified release signature (key %s) and SHA-256 against checksums.txt; installed %s to %s.\n", u.VerifiedKey.ID, res.Latest, installed)
	fmt.Fprintln(env.Stdout, "Restarting into the new version...")
	if err := updateRestart(installed); err != nil {
		fmt.Fprintf(env.Stderr, "gocode %s: installed %s, but could not restart (%v); run gocode again to use it.\n", verb, res.Latest, err)
	}
	return 0
}

func describeTarget(o updater.CheckOptions) string {
	switch {
	case o.Tag != "":
		return "tag " + o.Tag
	case o.Channel == updater.ChannelLatest:
		return "channel latest, prereleases included"
	default:
		return "channel stable"
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// loadUpdateRepo returns the highest-precedence settings value of
// "updateRepo" (later sources override earlier ones), or "". Only the user's
// own settings, --settings and managed policy may name the release
// repository: a cloned repository's .gocode/settings.json (or a committed
// settings.local.json) must not be able to point `gocode update`, run from
// inside it, at the attacker's releases and replace the gocode binary (the
// release's checksum comes from the same attacker-controlled release, so it
// is no defense). Same trust line as API keys and provider base URLs.
func loadUpdateRepo(env *Env) string {
	ctx := settingsContext(env)
	repo := ""
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded || !trustedKeySource(or.Source) {
			continue
		}
		if v, ok := or.Result.Settings[updater.RepoSettingsKey].(string); ok && strings.TrimSpace(v) != "" {
			repo = strings.TrimSpace(v)
		}
	}
	return repo
}
