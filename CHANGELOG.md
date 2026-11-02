# Changelog

What changed in each gocode release, newest first. The release build copies
the section for the version it is building onto the GitHub release page, and
it refuses to publish a version that has no section here.

## [Unreleased]

### Added

- **`gocode auth login [provider]` and `gocode auth logout [provider]`.**
  Save or remove a provider API key without starting a session, for
  example on a new machine or in a setup script. Without a provider name
  they use the active provider.
  - `login` asks for the key with hidden input, or reads it from stdin when
    piped (`pass show openrouter | gocode auth login openrouter`). The key is
    never taken as a command-line argument, where it would end up in your
    shell history.
  - Both say when a provider environment variable is set, since it wins over
    the saved key.

## [0.1.4] - 2026-11-02

### Added

- **Session scratchpad.** Every session gets a private temporary directory,
  `<temp>/gocode-<uid>/<project>/<session>/scratchpad`, and the model is told
  to use it for throwaway scripts and intermediate output instead of `/tmp` or
  your project. Reading and writing there needs no permission prompt, and
  sandboxed commands can write there too.
  - Each level is created private to you (mode 0700). If the per-user
    directory is a symlink or belongs to someone else, gocode does not use it
    and the session runs without a scratchpad.
  - A link inside the scratchpad that points outside it still asks before it
    is read.
  - Set `GOCODE_DISABLE_SCRATCHPAD=1` to turn it off.
  - Old scratchpads are left for your system's temp cleanup.

### Documentation

- Release pages now carry written release notes (from this changelog) and
  install and update instructions.

## [0.1.3] - 2026-11-02

### Changed

- **`/login` now handles a key that is already set.** Picking a provider that
  already has a key shows where that key comes from (never the key itself) and
  what you can do:
  - saved by `/login` in `~/.config/gocode/auth.json`: replace it, remove it
    or cancel;
  - from a settings file or an API key helper: save a different key, which
    takes priority;
  - from an environment variable such as `OPENROUTER_API_KEY`: the variable
    always wins, so change it where it is set (for example your shell profile)
    and restart gocode. You can still save a key for when the variable is gone.
- Saving a key while a provider environment variable is set now warns that the
  variable takes priority, instead of silently switching back at the next
  start.
- Removing the key of the provider in use says the session keeps using it
  until gocode restarts.

### Documentation

- README: install with Homebrew (`brew install umairge66/gocode/gocode`), and
  colour-coded download buttons.

## [0.1.2] - 2026-11-01

### Added

- **One-line installers.** `install.sh` for macOS, Linux and the BSDs, and
  `install.ps1` for Windows. Both check the download against the release's
  `checksums.txt`, and accept `GOCODE_VERSION` and `GOCODE_INSTALL_DIR`.
- **Homebrew tap**: `brew install umairge66/gocode/gocode`.

### Changed

- `gocode update` no longer replaces a binary that a package manager owns
  (Homebrew, winget, Scoop, Nix, .deb or .rpm packages, AppImage). It tells you
  the command to update it with instead.

### Fixed

- A language server that dies right after starting is now reported as a
  crash, and crash reports include the server's last error output.

## [0.1.1] - 2026-11-01

### Changed

- **Signed releases.** `checksums.txt` is signed (`checksums.txt.sig`,
  minisign format), and `gocode update` verifies the signature before it
  installs anything. 0.1.0 has no signature, so updating from 0.1.0 has to be
  done by hand once.

### Fixed

- Windows: paths given in short 8.3 form (`C:\PROGRA~1\...`) and paths with
  backslashes are handled correctly by the file tools, the agent definitions
  loader and the language server diagnostics.
- Windows: saving settings retries when another process briefly holds the
  file.
- An edit made within the same clock tick as the previous read, without
  changing the file's size, is now noticed as a change.
- `mcp add-from-gocode-desktop` no longer counts servers it does not import as
  already present.

### Removed

- The benchmark adapters and the Chrome probe tool.

## [0.1.0] - 2026-10-31

First public release.

[Unreleased]: https://github.com/umairge66/gocode/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/umairge66/gocode/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/umairge66/gocode/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/umairge66/gocode/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/umairge66/gocode/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/umairge66/gocode/releases/tag/v0.1.0
