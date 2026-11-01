#!/bin/sh
# Install gocode from its GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/umairge66/gocode/main/install.sh | sh
#
# Environment:
#   GOCODE_VERSION       release to install, e.g. 0.1.1 (default: the latest)
#   GOCODE_INSTALL_DIR   where to put the binary (default: ~/.local/bin)
#
# The download is checked against the release's checksums.txt before it is
# installed. Nothing else is downloaded or changed.
set -eu

repo="umairge66/gocode"
dir="${GOCODE_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
fail() { printf 'gocode install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
FreeBSD) os=freebsd ;;
OpenBSD) os=openbsd ;;
NetBSD) os=netbsd ;;
MINGW* | MSYS* | CYGWIN*) fail "on Windows, run in PowerShell: irm https://raw.githubusercontent.com/$repo/main/install.ps1 | iex" ;;
*) fail "unsupported system: $(uname -s)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
armv7* | armv8l) arch=arm ;;
i386 | i486 | i586 | i686) arch=386 ;;
riscv64) arch=riscv64 ;;
ppc64le) arch=ppc64le ;;
s390x) arch=s390x ;;
*) fail "unsupported processor: $(uname -m)" ;;
esac

# An x86_64 shell under Rosetta on Apple silicon: install the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
	arch=arm64
fi

if [ -n "${GOCODE_VERSION:-}" ]; then
	base="https://github.com/$repo/releases/download/v${GOCODE_VERSION#v}"
else
	base="https://github.com/$repo/releases/latest/download"
fi
archive="gocode_${os}_${arch}.tar.gz"

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail "curl or wget is required"
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
elif command -v sha256 >/dev/null 2>&1; then
	sha256() { sha256 -q "$1"; }
else
	fail "sha256sum, shasum or sha256 is required to check the download"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t gocode)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading gocode for $os/$arch..."
fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (expected $want, got $got)"

tar -xzf "$tmp/$archive" -C "$tmp"
bin="$(find "$tmp" -type f -name gocode | head -n 1)"
[ -n "$bin" ] || fail "the archive holds no gocode binary"

mkdir -p "$dir"
# Replace by rename, so a running gocode keeps its old file.
cp "$bin" "$dir/.gocode.new"
chmod 0755 "$dir/.gocode.new"
mv -f "$dir/.gocode.new" "$dir/gocode"
if [ "$os" = darwin ]; then
	xattr -d com.apple.quarantine "$dir/gocode" 2>/dev/null || true
fi

say "Installed $("$dir/gocode" --version 2>/dev/null || echo gocode) to $dir/gocode"
case ":$PATH:" in
*":$dir:"*) say "Run: gocode" ;;
*)
	say ""
	say "$dir is not on your PATH. Add it, then open a new terminal:"
	case "${SHELL:-}" in
	*/zsh) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
	*/fish) say "  fish_add_path $dir" ;;
	*) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc" ;;
	esac
	;;
esac
