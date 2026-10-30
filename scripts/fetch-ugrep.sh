#!/bin/sh
# fetch-ugrep.sh — download, verify and vendor the ugrep binaries that gocode
# embeds (internal/ugrep).
#
# Usage:
#   scripts/fetch-ugrep.sh <version>            prebuilt binaries only
#   scripts/fetch-ugrep.sh <version> --build    also build the other platforms
#                                               from source (needs zig, make)
#
# Every step is verified before anything in the tree changes; on any failure
# the script aborts and the vendored files stay as they were. See
# third_party/ugrep/UPDATING.md for the full procedure and the pinned inputs.
#
# Outputs (written only after every download and check succeeded):
#   internal/ugrep/bin/VERSION                       the version (read by Go)
#   internal/ugrep/bin/<goos>_<goarch>/ugrep[.exe]   one binary per platform
#   internal/ugrep/bin/<goos>_<goarch>/ugrep.sha256  its sha256
#   third_party/ugrep/VERSION, checksums.txt, LICENSE.txt
#
# This script is a build-time tool only: gocode never downloads ugrep at
# runtime.
set -eu

REPO=Genivia/ugrep
REPO_URL="https://github.com/$REPO"

# Pinned build inputs for --build (see UPDATING.md for how to bump them).
ZIG_VERSION=0.16.0
ZIG_HOST=x86_64-linux
ZIG_SHA256=70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00
ZIG_PUBKEY=RWSGOq2NVecA2UPNdBUZykf1CCb147pkmdtYxgb3Ti+JO/wCYvhbAb/U
PCRE2_VERSION=10.49
PCRE2_SHA256=53c156e1ba416a20da8e65395daa132da0d80e76910424caca3fcdae7831d384
PCRE2_SIGNER_FPR=A95536204A3BB489715231282A98E77EB6F24CA8

# goos_goarch:prebuilt-asset (or "-": built from source):zig-target:configure-host
PLATFORMS="
windows_amd64:ugrep-windows-x64.zip:x86_64-windows-gnu:x86_64-w64-mingw32
linux_amd64:-:x86_64-linux-musl:x86_64-linux-musl
linux_arm64:-:aarch64-linux-musl:aarch64-linux-musl
darwin_amd64:-:x86_64-macos:x86_64-apple-darwin
darwin_arm64:-:aarch64-macos:aarch64-apple-darwin
windows_arm64:-:aarch64-windows-gnu:aarch64-w64-mingw32
"

die() { echo "fetch-ugrep: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "usage: $0 <version> [--build]"
VERSION=${1#v}
BUILD=0
[ "${2:-}" = "--build" ] && BUILD=1
case "$VERSION" in
'' | *[!0-9.]*) die "invalid version: '$VERSION'" ;;
esac

for tool in curl tar unzip git python3; do
	command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "missing required tool: sha256sum or shasum"
fi
filesize() { wc -c <"$1" | tr -d ' '; }

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN_DIR="$ROOT/internal/ugrep/bin"
TP_DIR="$ROOT/third_party/ugrep"

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/ugrep-stage.XXXXXX")
cleanup() { rm -rf "$STAGE"; }
trap cleanup EXIT INT TERM
mkdir -p "$STAGE/dl" "$STAGE/src" "$STAGE/out"

# --- 1. Source: release tag tarball, checked against the git tag tree. -------
TAG="v$VERSION"
COMMIT=$(git ls-remote "$REPO_URL" "refs/tags/$TAG^{}" | awk '{print $1}')
[ -n "$COMMIT" ] || COMMIT=$(git ls-remote "$REPO_URL" "refs/tags/$TAG" | awk '{print $1}')
[ -n "$COMMIT" ] || die "tag $TAG not found in $REPO_URL"
SRC_TGZ="$STAGE/dl/ugrep-$VERSION.tar.gz"
curl -fsSL --retry 3 -o "$SRC_TGZ" "$REPO_URL/archive/refs/tags/$TAG.tar.gz" || die "source download failed"
tar -xzf "$SRC_TGZ" -C "$STAGE/src"
SRC="$STAGE/src/ugrep-$VERSION"
[ -d "$SRC" ] || die "unexpected source layout"
git -c advice.detachedHead=false clone -q --depth 1 --branch "$TAG" "$REPO_URL" "$STAGE/git"
[ "$(git -C "$STAGE/git" rev-parse HEAD)" = "$COMMIT" ] || die "tag $TAG moved while fetching"
diff -r -q -x .git "$STAGE/git" "$SRC" >/dev/null || die "source tarball differs from tag $TAG ($COMMIT)"
SRC_SHA=$(sha256 "$SRC_TGZ")
echo "fetch-ugrep: source $TAG ($COMMIT) tarball sha256 $SRC_SHA matches the tag tree"
[ -f "$SRC/LICENSE.txt" ] || die "source has no LICENSE.txt"

# --- 2. Release assets: sha256 published by GitHub for each asset. ----------
curl -fsSL --retry 3 -o "$STAGE/dl/release.json" "https://api.github.com/repos/$REPO/releases/tags/$TAG" ||
	die "could not read release $TAG"
asset_digest() {
	python3 -I -c 'import json,sys
d=json.load(open(sys.argv[1]))
for a in d.get("assets",[]):
    if a["name"]==sys.argv[2]:
        print((a.get("digest") or "").removeprefix("sha256:")); break' "$STAGE/dl/release.json" "$1"
}

{
	echo "# ugrep $VERSION ($REPO tag $TAG, commit $COMMIT)"
	echo "# source tarball sha256 $SRC_SHA"
	echo "# platform origin input_sha256 binary_sha256 binary_size input"
} >"$STAGE/checksums.txt"

record() { # plat origin input_sha binary input
	bsum=$(sha256 "$4")
	mkdir -p "$STAGE/out/$1"
	name=ugrep
	case "$1" in windows_*) name=ugrep.exe ;; esac
	cp "$4" "$STAGE/out/$1/$name"
	chmod 0755 "$STAGE/out/$1/$name"
	printf '%s\n' "$bsum" >"$STAGE/out/$1/ugrep.sha256"
	printf '%s %s %s %s %s %s\n' "$1" "$2" "$3" "$bsum" "$(filesize "$4")" "$5" >>"$STAGE/checksums.txt"
	echo "fetch-ugrep: $1 <- $2 ($bsum)"
}

# --- 3. Toolchain for source builds (zig, minisign-verified; PCRE2, GPG). ---
setup_toolchain() {
	command -v make >/dev/null 2>&1 || die "missing required tool: make"
	command -v gpg >/dev/null 2>&1 || die "missing required tool: gpg"
	ztar="zig-$ZIG_HOST-$ZIG_VERSION.tar.xz"
	curl -fsSL --retry 3 -o "$STAGE/dl/$ztar" "https://ziglang.org/download/$ZIG_VERSION/$ztar"
	curl -fsSL --retry 3 -o "$STAGE/dl/$ztar.minisig" "https://ziglang.org/download/$ZIG_VERSION/$ztar.minisig"
	[ "$(sha256 "$STAGE/dl/$ztar")" = "$ZIG_SHA256" ] || die "zig tarball sha256 mismatch"
	(cd "$ROOT" && go run ./cmd/gocode-release verify -pubkey "$ZIG_PUBKEY" "$STAGE/dl/$ztar" "$STAGE/dl/$ztar.minisig") ||
		die "zig signature does not verify"
	mkdir -p "$STAGE/zig"
	tar -xJf "$STAGE/dl/$ztar" -C "$STAGE/zig" --strip-components=1
	ZIG="$STAGE/zig/zig"

	ptar="pcre2-$PCRE2_VERSION.tar.bz2"
	purl="https://github.com/PCRE2Project/pcre2/releases/download/pcre2-$PCRE2_VERSION/$ptar"
	curl -fsSL --retry 3 -o "$STAGE/dl/$ptar" "$purl"
	curl -fsSL --retry 3 -o "$STAGE/dl/$ptar.sig" "$purl.sig"
	[ "$(sha256 "$STAGE/dl/$ptar")" = "$PCRE2_SHA256" ] || die "PCRE2 tarball sha256 mismatch"
	export GNUPGHOME="$STAGE/gnupg"
	mkdir -m 700 "$GNUPGHOME"
	curl -fsSL "https://keys.openpgp.org/vks/v1/by-fingerprint/$PCRE2_SIGNER_FPR" | gpg -q --import
	gpg -q --status-fd 1 --verify "$STAGE/dl/$ptar.sig" "$STAGE/dl/$ptar" 2>/dev/null |
		grep -q "VALIDSIG.* $PCRE2_SIGNER_FPR\$" || die "PCRE2 signature does not verify"
}

# build_one <plat> <zig-target> <configure-host>: static PCRE2 + ugrep with
# PCRE2 (-P) and no compression libraries (the shell's grep refuses -z).
build_one() {
	plat=$1 target=$2 host=$3
	w="$STAGE/build/$plat"
	mkdir -p "$w"
	tar -xjf "$STAGE/dl/pcre2-$PCRE2_VERSION.tar.bz2" -C "$w"
	# Keep the source timestamps so make treats the shipped configure and
	# Makefile.in as current instead of trying to regenerate them.
	cp -Rp "$SRC" "$w/ugrep"
	export SOURCE_DATE_EPOCH=$(git -C "$STAGE/git" log -1 --format=%ct)
	export LC_ALL=C TZ=UTC
	cc="$ZIG cc -target $target"
	cxx="$ZIG c++ -target $target"
	flags="-O2 -ffile-prefix-map=$w=."
	ldflags=""
	case "$plat" in linux_*) ldflags="-static -s" ;; esac
	(cd "$w/pcre2-$PCRE2_VERSION" &&
		./configure --host="$host" --prefix="$w/pcre2" --disable-shared --enable-static --enable-jit \
			CC="$cc" AR="$ZIG ar" RANLIB="$ZIG ranlib" CFLAGS="$flags" >/dev/null &&
		make -j4 >/dev/null && make install >/dev/null) || die "$plat: PCRE2 build failed"
	(cd "$w/ugrep" &&
		./configure --host="$host" --with-pcre2="$w/pcre2" --without-boost-regex \
			--without-zlib --without-bzlib --without-lzma --without-lz4 --without-zstd \
			--without-brotli --without-bzip3 --disable-7zip --disable-auto-color \
			CC="$cc" CXX="$cxx" AR="$ZIG ar" RANLIB="$ZIG ranlib" \
			CFLAGS="$flags" CXXFLAGS="$flags" LDFLAGS="$ldflags" >/dev/null &&
		make -j4 ACLOCAL=true AUTOCONF=true AUTOMAKE=true AUTOHEADER=true >/dev/null) || {
		# A platform whose build fails ships without ugrep and uses the
		# built-in search engine; the other platforms are still written.
		echo "fetch-ugrep: WARNING: $plat: ugrep build failed; this platform will use the built-in engine" >&2
		return 0
	}
	exe="$w/ugrep/src/ugrep"
	[ -f "$exe.exe" ] && exe="$exe.exe"
	[ -f "$exe" ] || die "$plat: no ugrep binary produced"
	record "$plat" "source+zig$ZIG_VERSION+pcre2-$PCRE2_VERSION" "$SRC_SHA" "$exe" "ugrep-$VERSION.tar.gz"
}

[ "$BUILD" = 1 ] && setup_toolchain

for entry in $PLATFORMS; do
	plat=${entry%%:*}
	rest=${entry#*:}
	asset=${rest%%:*}
	rest=${rest#*:}
	target=${rest%%:*}
	host=${rest#*:}
	if [ "$asset" != "-" ]; then
		want=$(asset_digest "$asset")
		[ -n "$want" ] || die "release $TAG publishes no sha256 for $asset"
		curl -fsSL --retry 3 -o "$STAGE/dl/$asset" "$REPO_URL/releases/download/$TAG/$asset" || die "download failed: $asset"
		got=$(sha256 "$STAGE/dl/$asset")
		[ "$want" = "$got" ] || die "sha256 MISMATCH for $asset: published $want, downloaded $got"
		mkdir -p "$STAGE/ex/$plat"
		unzip -q -o "$STAGE/dl/$asset" ugrep.exe -d "$STAGE/ex/$plat" || die "extract failed: $asset"
		record "$plat" "release-asset" "$got" "$STAGE/ex/$plat/ugrep.exe" "$asset"
	elif [ "$BUILD" = 1 ]; then
		build_one "$plat" "$target" "$host"
	elif kept=$(ls "$BIN_DIR/$plat/ugrep" "$BIN_DIR/$plat/ugrep.exe" 2>/dev/null | head -n 1) && [ -n "$kept" ] &&
		[ "$(tr -d ' \r\n' <"$BIN_DIR/VERSION" 2>/dev/null)" = "$VERSION" ]; then
		# Keep a binary of this version built earlier with --build.
		record "$plat" "kept" "-" "$kept" "-"
	else
		echo "fetch-ugrep: $plat has no prebuilt release binary; run with --build to build it (until then it uses the built-in engine)"
	fi
done

# --- 4. Write the verified results into the tree. -------------------------------
for plat_dir in "$BIN_DIR"/*_*; do
	plat=$(basename "$plat_dir")
	rm -f "$plat_dir/ugrep" "$plat_dir/ugrep.exe" "$plat_dir/ugrep.sha256"
	if [ -d "$STAGE/out/$plat" ]; then
		cp "$STAGE/out/$plat/"* "$plat_dir/"
	fi
done
printf '%s\n' "$VERSION" >"$BIN_DIR/VERSION"
mkdir -p "$TP_DIR"
printf '%s\n' "$VERSION" >"$TP_DIR/VERSION"
cp "$STAGE/checksums.txt" "$TP_DIR/checksums.txt"
tr -d '\r' <"$SRC/LICENSE.txt" >"$TP_DIR/LICENSE.txt"
echo "fetch-ugrep: vendored ugrep $VERSION; next: go test ./internal/ugrep/ ./internal/tools/..."
