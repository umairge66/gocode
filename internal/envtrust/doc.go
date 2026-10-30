// Package envtrust decides which parts of the host environment and filesystem
// the CLI is willing to trust before it (a) resolves its own settings/config,
// (b) spawns git and other subprocesses, and (c) touches files relative to a
// work tree. It provides:
//
//   - Classification data: the fixed lists of environment variable names the
//     CLI treats specially — loader/compiler/OpenSSL variables that can hijack
//     a child process, git-config overrides, temp-dir and config-dir
//     locators, toolchain roots, the combined "settings sensitive" set and the
//     uppercased lookup set.
//   - Env predicates: case-insensitive (Windows-aware) lookup and the
//     injectable-value detector that flags values able to smuggle in
//     variable expansion.
//   - Env builders: the git-trace-silencing overrides and the git-config
//     passthrough.
//   - Path trust: single-line sanitisation, ".git" pointer parsing,
//     absolute-aware joining, the within-directory containment test
//     and Windows drive-letter resolution.
//   - Filesystem errno classification: the errno codes a path probe may
//     tolerate as "expected" rather than fatal.
//
// Everything above is pure and depends only on inputs the caller supplies.
// Settings-tier resolution, repository/work-tree vouching, hooks dispatch and
// memory stores live elsewhere.
package envtrust
