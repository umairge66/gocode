# Releasing gocode

Releases are built and published by GitHub Actions
(`.github/workflows/release.yml`) when a version tag is pushed. Nothing is
uploaded anywhere except the repository's own GitHub Releases page.

## Cutting a release

1. Make sure CI is green on the commit you want to release and that
   `THIRD_PARTY_NOTICES` is current (`python3 scripts/gen-notices.py`).
2. Write the release notes in `CHANGELOG.md`: rename `## [Unreleased]` to
   `## [1.2.3] - YYYY-MM-DD`, start a new empty `## [Unreleased]` above it,
   and update the compare links at the bottom. Write for people who use
   gocode: what they can now do, what behaves differently, what was fixed.
   Check the result with `scripts/release-notes.sh 1.2.3`, and commit.
   The release workflow refuses a tag whose version has no section, before
   it builds anything.
3. Tag that commit with a semantic version (signed) and push the tag:

   ```sh
   git tag -s v1.2.3 -m "gocode 1.2.3"
   git push origin v1.2.3
   ```

   A suffix makes it a prerelease (`v1.3.0-rc.1`, `v1.3.0-beta.2`): the
   GitHub release is marked as a prerelease, and `gocode update` only offers
   it on the `latest` channel.
4. Watch the **Release** workflow. When it finishes, the release exists with
   the `CHANGELOG.md` section as its notes, install and update instructions,
   a link to the full list of changes since the previous tag, and every asset
   listed below.
5. Update the Homebrew formula in `umairge66/homebrew-gocode` to the new
   version and its checksums from `checksums.txt`.

The tag name without the `v` becomes the version compiled into the binaries
(`gocode --version` prints `1.2.3 (gocode)`) and is bound into the signature.
A release cannot be re-created by re-running the workflow; delete the
release (not the tag) first if you need to rebuild it.

## Dry run

Actions → **Release** → **Run workflow**, optionally with a version (default
`0.0.0-dryrun`). It runs every build, packaging and signing step and uploads
the result as the `release-assets` workflow artifact, but creates no release
and no attestation.

Locally, without AppImage and .deb/.rpm:

```sh
scripts/build-release.sh 1.2.3                # all targets -> dist/release/assets
scripts/build-release.sh -t "linux/amd64" 1.2.3 /tmp/out
```

The script uses the same flags as CI (`CGO_ENABLED=0 -trimpath
-buildvcs=false -ldflags "-s -w -buildid= -X …CurrentVersion=<version>"`), sets
archive timestamps from `SOURCE_DATE_EPOCH` (default: the last commit time),
and needs no network once the Go modules are downloaded. Set
`RELEASE_PUBLIC_KEYS` and `RELEASE_REPO` to embed the update key and default
update repository the way CI does.

## Secrets and variables

Set these under Settings → Secrets and variables → Actions.

| Name | Kind | Purpose |
|---|---|---|
| `GOCODE_RELEASE_SIGNING_KEY` | secret | Contents of the private key file from `gocode-release keygen`. Signs `checksums.txt`. |
| `GOCODE_RELEASE_PUBLIC_KEY` | variable | The matching public key (`RW…`), compiled into the binaries so `gocode update` can verify releases. Comma-separate two keys while rotating. |

Create the key pair once, on a trusted machine:

```sh
go run ./cmd/gocode-release keygen -out ~/secure/gocode-release.key
```

It prints the public key. Paste the whole private key file (both lines) into
the secret and the public key into the variable, then keep the private key
offline. `scripts/RELEASE-SIGNING.md` covers the format, manual signing and key
rotation.

Without the secret the workflow still publishes a release, but:

- there is no `checksums.txt.sig`, the release title ends in "(unsigned)" and
  the notes start with a warning;
- `gocode update` refuses to install it (the updater only accepts a signed
  `checksums.txt`; there is no override).

If the secret is set but the variable is missing, the workflow fails rather
than publish signed releases that no binary can verify.

The repository name (`${{ github.repository }}`) is compiled in as the default
update source, so a fork or a renamed repository updates from itself.
No other secrets are used; the release is created with the workflow's own
`GITHUB_TOKEN`, and only the publish job can write to the repository.

## Assets

For a version `V`:

| Asset | What |
|---|---|
| `gocode-V-<os>-<arch>.tar.gz` | Linux, macOS, FreeBSD, OpenBSD, NetBSD: a `gocode-V-<os>-<arch>/` directory with `gocode`, `LICENSE`, `README.md`, `THIRD_PARTY_NOTICES` |
| `gocode-V-windows-<arch>.zip` | the same for Windows, with `gocode.exe` |
| `gocode_<goos>_<goarch>.tar.gz` / `.zip` | the same archives under the names `gocode update` looks for (Go's `GOOS`/`GOARCH`; 32-bit ARM is `linux_arm`) |
| `gocode-V-x86_64.AppImage`, `gocode-V-aarch64.AppImage` | Linux AppImages |
| `gocode_V_amd64.deb`, `gocode_V_arm64.deb` | Debian/Ubuntu packages |
| `gocode-V-1.x86_64.rpm`, `gocode-V-1.aarch64.rpm` | RPM packages |
| `gocode-chrome-extension-<ext-version>.zip` (+ `.sha256`) | the Chrome Web Store upload zip (`scripts/package-extension.sh`) |
| `checksums.txt` | SHA-256 of every other asset (sha256sum format) |
| `checksums.txt.sig` | minisign signature of `checksums.txt`, bound to version `V` (signed releases only) |

Targets: linux amd64, arm64, 386, armv7, riscv64, ppc64le, s390x; darwin
amd64, arm64; windows amd64, arm64, 386; freebsd amd64, arm64; openbsd amd64;
netbsd amd64. All builds are static (`CGO_ENABLED=0`). The bundled ugrep is
embedded for linux, darwin (amd64/arm64) and windows/amd64; the other targets
use the built-in search engine.

On public repositories the workflow also records a signed SLSA build attestation for
every asset in `checksums.txt` (`gh attestation verify <file> --repo
<owner>/<repo>`). Attestations are not available for private repositories on
most plans, so the step is skipped there.

### AppImage

`scripts/package-appimage.sh` builds an AppDir from `packaging/linux`
(`AppRun`, `gocode.desktop` with `Terminal=true` and
`Categories=Development;`, a 256 px icon rendered from
`extension/icons/src/prompty-128.svg`) plus `usr/bin/gocode`, and runs
`appimagetool`. Both images are built on an x86_64 runner: the aarch64 one
uses the aarch64 type2 runtime via `--runtime-file`. The workflow pins
appimagetool and the runtimes to exact releases and checks their SHA-256
before use; bump `APPIMAGETOOL_*` / `APPIMAGE_RUNTIME_*` in `release.yml`
together with the new checksums from the release page.

### .deb and .rpm

Built with [nfpm](https://nfpm.goreleaser.com) from `packaging/linux/nfpm.yaml`
(pinned version and SHA-256 in `release.yml`, `NFPM_*`). They install
`/usr/bin/gocode` and the license files under `/usr/share/doc/gocode/`, and
have no dependencies. The packages are not GPG-signed; the signed
`checksums.txt` covers them.

## Not set up yet

- **macOS signing and notarization.** Needs an Apple Developer ID Application
  certificate and an App Store Connect API key; sign with `codesign
  --options runtime --timestamp`, submit with `xcrun notarytool submit
  --wait` (a bare binary can't be stapled, so ship it in a notarized zip or
  pkg). This has to run on a macOS runner.
- **Windows Authenticode signing.** Needs a code-signing certificate (an EV or
  cloud-HSM certificate, or Azure Trusted Signing) and `signtool sign /fd
  sha256 /tr <timestamp-url> /td sha256` on the `.exe` before it is archived.
- **Package managers.** A Homebrew tap (formula pointing at the darwin/linux
  tarballs and their SHA-256), a Scoop bucket manifest for the Windows zips,
  and a winget manifest (`wingetcreate`) can all be generated from
  `checksums.txt` after a release.

Both signing steps would go between "build" and "archive" in
`scripts/build-release.sh`, so that `checksums.txt` covers the signed binaries.
