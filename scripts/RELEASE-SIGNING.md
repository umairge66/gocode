# Release signing

`gocode update` / `gocode install` only install a release whose
`checksums.txt` carries a valid Ed25519 signature (`checksums.txt.sig`,
minisign format) from a key compiled into the running binary. Builds without a
key (dev builds) refuse to update; there is no skip flag.

## 1. Generate the key (once)

```sh
go run ./cmd/gocode-release keygen -out ~/secure/gocode-release.key
```

- Writes the private key (mode 0600, **not** password-protected) and
  `gocode-release.key.pub`. Refuses to overwrite existing files.
- Prints the public key (`RW…`, key id embedded) and the `-ldflags` snippet.
- Keep the private key offline or as a CI secret only. Never commit it.
  The public key is not secret.

## 2. Build with the public key

```sh
go build -ldflags "\
  -X github.com/umairge66/gocode/internal/updater.CurrentVersion=1.2.3 \
  -X github.com/umairge66/gocode/internal/updater.ReleasePublicKeys=RW…" ./cmd/gocode
```

## 3. Sign at release time

After producing the assets and `checksums.txt` (sha256sum format):

```sh
GOCODE_RELEASE_KEY_FILE=~/secure/gocode-release.key \
  go run ./cmd/gocode-release sign -version 1.2.3 dist/checksums.txt   # -> dist/checksums.txt.sig
go run ./cmd/gocode-release verify -pubkey RW… dist/checksums.txt      # sanity check
```

Upload both `checksums.txt` and `checksums.txt.sig` along with the
binaries. Use `-version` every time: it binds the signature to the release,
so an old signed manifest can't be re-uploaded under a newer tag.

In CI, store the key file's contents as a secret, write it to a temp file
(mode 0600), point `GOCODE_RELEASE_KEY_FILE` at it, and delete the file
afterwards.
The release workflow does exactly this with the `GOCODE_RELEASE_SIGNING_KEY`
secret and compiles in the `GOCODE_RELEASE_PUBLIC_KEY` repository variable;
see RELEASING.md.

## Rotating keys

1. `keygen` a new key.
2. Ship a release built with both keys:
   `-X …ReleasePublicKeys=RW<old>,RW<new>` (signed with the old key).
3. Sign the releases after that with the new key. Once users are on builds
   that trust the new key, drop the old key from the ldflags.

If a key is compromised, drop it from the ldflags at once. Users still on
builds that trust it need to download a new build manually.

## minisign interop

The files are minisign-compatible: `minisign -V -p gocode-release.key.pub -m
checksums.txt -x checksums.txt.sig` verifies them, and
`minisign -S -s <key> -m checksums.txt -x checksums.txt.sig` signs with the
key (`gocode-release sign` can't read password-encrypted minisign keys).
