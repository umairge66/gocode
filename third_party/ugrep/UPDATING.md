# ugrep in gocode

gocode embeds a ugrep binary per release platform and uses it for two things:

- **The Grep tool's content matching.** gocode's walker picks the candidate
  files (ignore files, hidden entries, globs, file types — the same rules as
  before), ugrep lists which of them match (`ugrep --no-config -l -0 -s -Y -P
  --color=never [-i] -e PATTERN --from=-`), and the Go engine confirms and
  renders the matches. Multiline searches, and any search where ugrep is
  missing, fails or rejects the pattern, run on the Go engine alone. Glob and
  the `@` file menu list files with the walker (listing needs no file
  contents).
- **`grep` in the model's shell.** Before each Bash command (and `!` shell
  mode and command `!`-blocks) gocode defines a `grep` shell function that
  runs `ugrep -G --ignore-files --hidden -I --exclude-dir=.git
  --exclude-dir=.svn --exclude-dir=.hg --exclude-dir=.bzr --exclude-dir=.jj
  --exclude-dir=.sl "$@"`. Arguments matching `-*-filter*`, `-*-pager*`,
  `-*-view*`, `-*-format-open*`, `-*-config*`, `---*`, `-@*`,
  `-*-save-config*`, `-[Zz]*`, `-[!-]*[Zz]*`, `--null` or `--null-data` send
  the command to the system grep instead, as does a missing ugrep binary.
  `egrep`, `fgrep`, `zgrep` and `find` are not changed. While this is active
  the Grep tool is not offered and the prompts point at `grep` in Bash. It is
  off for PowerShell (Windows without Git Bash), for
  `GOCODE_CODE_ENTRYPOINT=local-agent`, and with `search.useUgrep: false`.

## Layout

| Path | Contents |
| --- | --- |
| `internal/ugrep/bin/VERSION` | pinned version (single source for Go) |
| `internal/ugrep/bin/<goos>_<goarch>/ugrep[.exe]` | the binary, when one is vendored |
| `internal/ugrep/bin/<goos>_<goarch>/ugrep.sha256` | its sha256, checked at extraction |
| `internal/ugrep/bundle_<goos>_<goarch>.go` | embeds `bin/<goos>_<goarch>` (no binary there = no bundle) |
| `internal/ugrep/bundle_none.go` | other platforms, or `-tags gocode_nobundledugrep` |
| `third_party/ugrep/VERSION`, `checksums.txt` | version, origin and sha256 of every input and binary |
| `third_party/ugrep/LICENSE.txt` | ugrep licence (BSD 3-Clause) |
| `third_party/ugrep/licenses/` | licences of the libraries in the Windows release binary |

A platform without a vendored binary still builds; it uses a `ugrep` on PATH
if there is one, else the built-in Go engine (and the system grep in Bash).

## Current state (7.8.5)

| Platform | Binary | Origin |
| --- | --- | --- |
| windows/amd64 | vendored | official release asset `ugrep-windows-x64.zip`, sha256 verified against the digest GitHub publishes for the asset |
| linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/arm64 | not yet vendored | upstream publishes no prebuilt binary; build with `--build` (below) |

Source: tag `v7.8.5`, commit `f2cc252c011460f6c75b1fb591813b4b6b59c225`,
tarball sha256 `f080ab6cd9d96a5357570cb60a99e09ebe5eef11cf322c35373dbe815378a1f2`
(the script checks the tarball against the tag's git tree).

## Bump procedure

1. Read the ugrep release notes between the current and the new version for
   changed options or output. gocode relies on: `--no-config -l -0 -s -Y -P
   --color=never -i -e --from=-` (Grep tool) and `-G --ignore-files --hidden -I
   --exclude-dir` plus grep-compatible output and exit codes (shell `grep`).
2. Fetch and verify:

   ```sh
   scripts/fetch-ugrep.sh 7.8.5            # prebuilt release binaries only
   scripts/fetch-ugrep.sh 7.8.5 --build    # also build the other platforms
   ```

   The script resolves the release tag to its commit, downloads the source
   tarball and checks it equals the tag's git tree, downloads each prebuilt
   asset and checks it against the sha256 digest GitHub publishes for it, and
   only then writes the binaries, `ugrep.sha256` files, `VERSION`,
   `checksums.txt` and `LICENSE.txt`. Any failed check aborts with nothing
   changed.
3. Run the tests:

   ```sh
   go test ./internal/ugrep/ ./internal/tools/... ./internal/cli/
   GOCODE_TEST_UGREP=/path/to/ugrep go test ./internal/tools/filetools/ ./internal/tools/exec/
   ```

   `internal/ugrep` checks every vendored binary against `ugrep.sha256` and
   `checksums.txt`. `filetools` compares the Grep and Glob results on a
   fixture repository (nested `.gitignore`, `.ignore`, `.git/info/exclude`,
   hidden files, a binary file, symlinks) with `testdata/search_golden.json`,
   with the Go engine and — given a ugrep — with ugrep. `exec` runs a table of
   grep command lines through GNU grep and through the shell function, in sh,
   bash and zsh, and requires the same output and exit status.
4. Cross-build: `GOOS=windows GOARCH=amd64 go build ./...`,
   `GOOS=darwin GOARCH=arm64 go build ./...`, `GOOS=linux GOARCH=arm64 go build ./...`.

## Building from source (`--build`)

For platforms without an official binary the script builds the tagged source
on a Linux x86_64 host with pinned, verified inputs (the `--build` path has
not been run yet; expect to adjust it on its first run):

| Input | Version | Verification |
| --- | --- | --- |
| zig (C/C++ cross toolchain) | 0.16.0 | sha256 `70e49664…3d00` and the minisign signature of the Zig release key `RWSGOq2NVecA2UPNdBUZykf1CCb147pkmdtYxgb3Ti+JO/wCYvhbAb/U` (checked with `go run ./cmd/gocode-release verify`) |
| PCRE2 | 10.49 | sha256 `53c156e1…d384` and the GPG signature of Nicholas Wilson's key `A955 3620 4A3B B489 7152 3128 2A98 E77E B6F2 4CA8` |
| ugrep | the release tag | tarball equal to the tag's git tree |

Targets: `x86_64-linux-musl`, `aarch64-linux-musl` (fully static),
`x86_64-macos`, `aarch64-macos` (libc++ linked in; Apple Silicon binaries get
zig's ad-hoc code signature), `aarch64-windows-gnu`. Each build is PCRE2
(static, JIT) plus ugrep configured with `--with-pcre2 --without-boost-regex
--without-zlib --without-bzlib --without-lzma --without-lz4 --without-zstd
--without-brotli --without-bzip3 --disable-7zip --disable-auto-color`, built
with `SOURCE_DATE_EPOCH` set to the tag's commit time, `LC_ALL=C TZ=UTC` and
`-ffile-prefix-map` so that rebuilding the same inputs aims at the same bytes
(compare a rebuild's sha256 with `checksums.txt`).
Compression support is left out on purpose: the shell `grep` sends `-z` to
the system grep, and the Grep tool never decompresses.

To bump zig or PCRE2, update `ZIG_VERSION`/`ZIG_SHA256` and
`PCRE2_VERSION`/`PCRE2_SHA256` at the top of `scripts/fetch-ugrep.sh` from
their release pages (and the signer fingerprint if the PCRE2 release key
changes), then run `--build` and the tests above.

The macOS binaries cannot be run on the build host; before a release, run
`internal/ugrep`'s tests and the `GOCODE_TEST_UGREP` tests on a Mac (and on
Windows/arm64 hardware for that binary).

## Behaviour differences from before

- File selection is unchanged: the same walker decides which files are
  searched or listed, including `.ignore`/`.rgignore`, `.git/info/exclude`
  and the global git excludes file, so nothing ugrep's own `--ignore-files`
  handles differently (it reads only `.gitignore`, and also outside git
  repositories) affects the tools.
- Regex: a file must match both the Perl-compatible pattern in ugrep and the
  Go (RE2) pattern. `\w`, `\d`, `\s`, `\b` are Unicode-aware in ugrep and
  ASCII in Go, so a pattern such as `na\Wve` no longer matches `naïve` (it
  did with the Go engine alone). A Go pattern that PCRE2 rejects falls back
  to the Go engine.
- Glob sort order is unchanged: oldest modification time first.
- Shell `grep` (vs GNU grep): `.gitignore`d files, binary files and VCS
  directories are skipped and hidden files are searched; with several files,
  output from different files may come in any order (ugrep searches in
  parallel); a path reached through `.` is printed without `./`; error
  messages read `ugrep: warning: …`. Exit codes are the same.
