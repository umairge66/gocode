#!/bin/sh
# build-release.sh — cross-build gocode for every release platform and package
# the release assets. The release workflow (.github/workflows/release.yml)
# runs this same script, so a local run produces the same files as CI.
#
# Usage:
#   scripts/build-release.sh [options] <version> [outdir]    (outdir default: dist/release)
#   scripts/build-release.sh --checksums <assets-dir>
#
# Options:
#   -t, --targets "LIST"   space-separated subset of the targets below
#   --no-extension         skip the Chrome extension zip
#   --no-checksums         skip checksums.txt (when more assets are added later;
#                          run --checksums on the assets directory afterwards)
#
# <version> is a semantic version without the leading "v" (e.g. 1.2.3 or
# 1.3.0-rc.1). It becomes `gocode --version` and the version the updater
# compares against.
#
# Output:
#   <outdir>/assets/gocode-<version>-<os>-<arch>.tar.gz   (.zip on windows)
#                   each holding gocode-<version>-<os>-<arch>/{gocode,LICENSE,README.md,THIRD_PARTY_NOTICES}
#   <outdir>/assets/gocode_<goos>_<goarch>.tar.gz          (.zip on windows) the same archive
#                   under the name `gocode update` looks for
#   <outdir>/assets/gocode-chrome-extension-<v>.zip        (+ .sha256)
#   <outdir>/assets/checksums.txt                          sha256 of every asset
#   <outdir>/bin/<os>-<arch>/gocode[.exe]                  the raw binaries (not uploaded;
#                   used to build the AppImage and .deb/.rpm packages)
#
# Optional environment:
#   RELEASE_PUBLIC_KEYS   release signing public key(s) compiled into the
#                         binaries (comma-separated, from `gocode-release keygen`).
#                         Without it the binaries cannot verify, and therefore
#                         refuse, self-updates.
#   RELEASE_REPO          owner/repo compiled in as the default update source.
#   SOURCE_DATE_EPOCH     timestamp for archive entries (default: the last git
#                         commit time, else 1980-01-01).
#
# No network access is needed once the Go modules are in the module cache.
set -eu

ALL_TARGETS="linux/amd64 linux/arm64 linux/386 linux/armv7 linux/riscv64 linux/ppc64le linux/s390x
darwin/amd64 darwin/arm64
windows/amd64 windows/arm64 windows/386
freebsd/amd64 freebsd/arm64 openbsd/amd64 netbsd/amd64"

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

die() {
	echo "build-release: $*" >&2
	exit 1
}

# write_checksums DIR — sha256sum-format checksums.txt over every regular file
# in DIR except checksums.txt and its signature, sorted by name.
write_checksums() {
	python3 - "$1" <<'EOF'
import hashlib, os, sys
d = sys.argv[1]
skip = {"checksums.txt", "checksums.txt.sig"}
lines = []
for name in sorted(os.listdir(d)):
    p = os.path.join(d, name)
    if name in skip or name.startswith(".") or not os.path.isfile(p):
        continue
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    lines.append(f"{h.hexdigest()}  {name}\n")
if not lines:
    sys.exit("build-release: no assets to checksum in " + d)
with open(os.path.join(d, "checksums.txt"), "w", newline="\n") as f:
    f.writelines(lines)
EOF
	echo "wrote $1/checksums.txt"
}

targets=$ALL_TARGETS
extension=1
checksums=1
while [ $# -gt 0 ]; do
	case $1 in
	-t | --targets)
		[ $# -ge 2 ] || die "$1 needs a value"
		targets=$2
		shift 2
		;;
	--no-extension)
		extension=0
		shift
		;;
	--no-checksums)
		checksums=0
		shift
		;;
	--checksums)
		[ $# -eq 2 ] || die "usage: $0 --checksums <assets-dir>"
		[ -d "$2" ] || die "$2 is not a directory"
		write_checksums "$2"
		exit 0
		;;
	-h | --help)
		sed -n '2,/^set -eu/p' "$0" | sed '$d; s/^# \{0,1\}//'
		exit 0
		;;
	--)
		shift
		break
		;;
	-*) die "unknown option $1" ;;
	*) break ;;
	esac
done

[ $# -ge 1 ] && [ $# -le 2 ] || die "usage: $0 [options] <version> [outdir]"
version=${1#v}
out=${2:-"$root/dist/release"}
case $out in
/*) ;;
*) out=$(pwd)/$out ;;
esac

echo "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$' ||
	die "version $version is not a semantic version (want e.g. 1.2.3 or 1.3.0-rc.1)"

for t in $targets; do
	case " $(echo $ALL_TARGETS) " in
	*" $t "*) ;;
	*) die "unknown target $t (known: $(echo $ALL_TARGETS))" ;;
	esac
done

pubkeys=${RELEASE_PUBLIC_KEYS:-}
repo=${RELEASE_REPO:-}
echo "$pubkeys" | grep -Eq '^[A-Za-z0-9+/=,]*$' || die "RELEASE_PUBLIC_KEYS contains unexpected characters"
echo "$repo" | grep -Eq '^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?$' || die "RELEASE_REPO must be owner/repo"

cd "$root"
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH=$(git log -1 --format=%ct 2>/dev/null || true)
	[ -n "$SOURCE_DATE_EPOCH" ] || SOURCE_DATE_EPOCH=315532800
fi
export SOURCE_DATE_EPOCH

for f in LICENSE README.md THIRD_PARTY_NOTICES; do
	[ -f "$f" ] || die "missing $f"
done

pkg=github.com/umairge66/gocode/internal/updater
ldflags="-s -w -buildid= -X $pkg.CurrentVersion=$version"
[ -z "$pubkeys" ] || ldflags="$ldflags -X $pkg.ReleasePublicKeys=$pubkeys"
[ -z "$repo" ] || ldflags="$ldflags -X $pkg.DefaultRepo=$repo"

assets=$out/assets
bins=$out/bin
mkdir -p "$assets" "$bins"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT INT TERM

echo "gocode $version -> $out"
[ -n "$pubkeys" ] || echo "note: RELEASE_PUBLIC_KEYS is empty; these binaries will refuse to self-update"

for t in $targets; do
	goos=${t%%/*}
	label=${t#*/}
	goarm=
	case $label in
	armv7)
		goarch=arm
		goarm=7
		;;
	*) goarch=$label ;;
	esac
	exe=gocode
	[ "$goos" = windows ] && exe=gocode.exe
	bindir=$bins/$goos-$label
	mkdir -p "$bindir"
	echo "building $goos/$label"
	env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" GOFLAGS=-mod=readonly \
		go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$bindir/$exe" ./cmd/gocode

	name=gocode-$version-$goos-$label
	if [ "$goos" = windows ]; then
		archive=$name.zip
		updater_name=gocode_${goos}_$goarch.zip
	else
		archive=$name.tar.gz
		updater_name=gocode_${goos}_$goarch.tar.gz
	fi
	python3 - "$assets/$archive" "$name" "$bindir/$exe" LICENSE README.md THIRD_PARTY_NOTICES <<'EOF'
import gzip, io, os, sys, tarfile, time, zipfile
dest, prefix, binary, *docs = sys.argv[1:]
epoch = max(int(os.environ["SOURCE_DATE_EPOCH"]), 315532800)
files = [(binary, os.path.basename(binary), 0o755)] + [(d, d, 0o644) for d in docs]
if dest.endswith(".zip"):
    stamp = time.gmtime(epoch)[:6]
    with zipfile.ZipFile(dest, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for src, arc, mode in files:
            info = zipfile.ZipInfo(f"{prefix}/{arc}", date_time=stamp)
            info.external_attr = (0o100000 | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            with open(src, "rb") as f:
                z.writestr(info, f.read())
else:
    with open(dest, "wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch, compresslevel=9) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.PAX_FORMAT) as tar:
                for src, arc, mode in files:
                    info = tarfile.TarInfo(f"{prefix}/{arc}")
                    info.size = os.path.getsize(src)
                    info.mode = mode
                    info.mtime = epoch
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    with open(src, "rb") as f:
                        tar.addfile(info, f)
EOF
	cp "$assets/$archive" "$assets/$updater_name"
done

if [ "$extension" = 1 ]; then
	sh scripts/package-extension.sh "$assets" >/dev/null
	echo "packaged the Chrome extension"
fi

if [ "$checksums" = 1 ]; then
	write_checksums "$assets"
fi

echo
ls -l "$assets"
