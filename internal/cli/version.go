// Package cli implements the top-level gocode entrypoint and command
// dispatch: the top-level argument parsing, the
// fast-path dispatch order, the subcommand/flag surface, the version reporting
// and the subcommand handlers.
package cli

import "github.com/umairge66/gocode/internal/updater"

// BuildInfo is the build-metadata record used wherever version/build data is
// needed.
type BuildInfo struct {
	IssuesExplainer  string
	PackageURL       string
	ReadmeURL        string
	Version          string
	FeedbackChannel  string
	BuildTime        string
	GitSHA           string
	HooksWorkerURL   string
	DDSourcemapGroup string
}

// Build is the single build-metadata record.
var Build = BuildInfo{
	IssuesExplainer:  "report the issue at https://github.com/umairge66/gocode/issues",
	PackageURL:       "@gocode-ai/gocode",
	ReadmeURL:        "",
	Version:          updater.CurrentVersion, // the build version (ldflags -X updater.CurrentVersion); "dev" in source
	FeedbackChannel:  "https://github.com/umairge66/gocode/issues",
	BuildTime:        "2026-10-21T00:58:51Z",
	GitSHA:           "01ae8327ca9931db91a0802d6e8e377419dd44e6",
	HooksWorkerURL:   "/$bunfs/root/src/plugins/functionHooks/hooks-worker/hooks-worker.js",
	DDSourcemapGroup: "default",
}

// Version is the source-marker version ("1.0.0"). It is distinct from
// Build.Version, the runtime build version the `--version`
// command reports. Build.Version is updater.CurrentVersion ("dev" in source,
// stamped with -ldflags "-X github.com/umairge66/gocode/internal/updater.CurrentVersion=…"),
// so `gocode --version` and `gocode update` always report the same version.
const Version = "1.0.0"

// versionSuffix returns the optional build-ref-name suffix appended
// after "(gocode)" in `--version` output. In this build it resolves to the
// empty string because BUILD_REF_NAME is absent from the metadata object.
func versionSuffix() string {
	// There is no BUILD_REF_NAME in the build metadata -> "".
	return ""
}

// versionLine is the version line `gocode --version` prints and the /version
// command shows: "<version> (gocode)" plus the optional build-ref suffix.
func versionLine() string {
	return updateCurrentVersion() + " (gocode)" + versionSuffix()
}
