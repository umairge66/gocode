#!/bin/sh
# package-appimage.sh — wrap a Linux gocode binary in an AppImage.
#
# Usage:
#   APPIMAGETOOL=/path/to/appimagetool APPIMAGE_RUNTIME=/path/to/runtime-<arch> \
#     scripts/package-appimage.sh <version> <x86_64|aarch64> <gocode-binary> <outdir>
#
# Writes <outdir>/gocode-<version>-<arch>.AppImage. The AppDir holds AppRun,
# gocode.desktop (Terminal=true), a 256px icon and usr/bin/gocode; the files
# come from packaging/linux. APPIMAGE_RUNTIME is the AppImage type2 runtime for
# the target architecture, so an x86_64 host can build the aarch64 image too.
# appimagetool runs with --appimage-extract-and-run, so FUSE is not needed.
# Nothing is downloaded by this script.
set -eu

die() {
	echo "package-appimage: $*" >&2
	exit 1
}

[ $# -eq 4 ] || die "usage: $0 <version> <x86_64|aarch64> <gocode-binary> <outdir>"
version=${1#v}
arch=$2
binary=$3
out=$4
case $arch in
x86_64 | aarch64) ;;
*) die "unsupported architecture $arch" ;;
esac
[ -f "$binary" ] || die "no binary at $binary"
tool=${APPIMAGETOOL:?set APPIMAGETOOL to the appimagetool executable}
runtime=${APPIMAGE_RUNTIME:?set APPIMAGE_RUNTIME to the type2 runtime for $arch}
[ -f "$runtime" ] || die "no runtime at $runtime"

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$out"
out=$(CDPATH= cd -- "$out" && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
appdir=$work/gocode.AppDir
mkdir -p "$appdir/usr/bin" "$appdir/usr/share/applications" \
	"$appdir/usr/share/icons/hicolor/256x256/apps" "$appdir/usr/share/doc/gocode"

install -m 0755 "$binary" "$appdir/usr/bin/gocode"
install -m 0755 "$root/packaging/linux/AppRun" "$appdir/AppRun"
install -m 0644 "$root/packaging/linux/gocode.desktop" "$appdir/gocode.desktop"
install -m 0644 "$root/packaging/linux/gocode.desktop" "$appdir/usr/share/applications/gocode.desktop"
install -m 0644 "$root/packaging/linux/gocode.png" "$appdir/gocode.png"
install -m 0644 "$root/packaging/linux/gocode.png" "$appdir/usr/share/icons/hicolor/256x256/apps/gocode.png"
ln -s gocode.png "$appdir/.DirIcon"
for f in LICENSE README.md THIRD_PARTY_NOTICES; do
	install -m 0644 "$root/$f" "$appdir/usr/share/doc/gocode/$f"
done

dest=$out/gocode-$version-$arch.AppImage
rm -f "$dest"
ARCH=$arch "$tool" --appimage-extract-and-run --no-appstream \
	--runtime-file "$runtime" "$appdir" "$dest"
chmod 0755 "$dest"
echo "wrote $dest"
