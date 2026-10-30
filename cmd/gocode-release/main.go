// Command gocode-release manages the Ed25519 key that signs gocode releases
// (checksums.txt → checksums.txt.sig, minisign format). See
// scripts/RELEASE-SIGNING.md.
//
//	go run ./cmd/gocode-release keygen -out ~/secure/gocode-release.key
//	go run ./cmd/gocode-release sign   -key ~/secure/gocode-release.key [-version 1.2.3] dist/checksums.txt
//	go run ./cmd/gocode-release verify -pubkey RWQ… dist/checksums.txt
//
// The private key path for sign may also come from $GOCODE_RELEASE_KEY_FILE.
// Nothing here touches the network.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/updater/releasesig"
)

// keyFileEnvVar names the env var holding the private key path for sign.
const keyFileEnvVar = "GOCODE_RELEASE_KEY_FILE"

const ldflagsVar = "github.com/umairge66/gocode/internal/updater.ReleasePublicKeys"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, time.Now))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  gocode-release keygen -out <private-key-path>
  gocode-release sign [-key <private-key-path>] [-version <v>] [-out <sig>] [-force] <checksums.txt>
  gocode-release verify (-pubkey <key>[,<key>…] | -pubkey-file <file>) <checksums.txt> [<sig>]

sign reads the key path from -key or $`+keyFileEnvVar+`.
`)
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string, now func() time.Time) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = cmdKeygen(args[1:], stdout, stderr)
	case "sign":
		err = cmdSign(args[1:], stdout, stderr, getenv, now)
	case "verify":
		err = cmdVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "gocode-release %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("out", "", "path for the new private key file (required; never overwritten)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *out == "" && fl.NArg() == 1 {
		*out = fl.Arg(0)
	} else if fl.NArg() > 0 {
		return errors.New("unexpected arguments")
	}
	if *out == "" {
		return errors.New("-out <private-key-path> is required")
	}
	pubPath := *out + ".pub"
	for _, p := range []string{*out, pubPath} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite a key", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	sk, err := releasesig.GenerateKey(nil)
	if err != nil {
		return err
	}
	if err := writeExclusive(*out, sk.File(), 0o600); err != nil {
		return err
	}
	pk := sk.Public()
	if err := writeExclusive(pubPath, pk.File(), 0o644); err != nil {
		_ = os.Remove(*out) // never used; don't leave a key without its .pub
		return err
	}
	fmt.Fprintf(stdout, `Wrote private key %s (mode 0600, NOT password-protected — keep it offline or in a CI secret)
Wrote public key  %s

Key id:     %s
Public key: %s

Build gocode trusting this key:
  go build -ldflags "-X %s=%s" ./cmd/gocode

To rotate later, list old and new keys comma-separated:
  -X %s=<old>,<new>
`, *out, pubPath, pk.ID, pk, ldflagsVar, pk, ldflagsVar)
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer, getenv func(string) string, now func() time.Time) error {
	fl := flag.NewFlagSet("sign", flag.ContinueOnError)
	fl.SetOutput(stderr)
	keyPath := fl.String("key", "", "private key file (default $"+keyFileEnvVar+")")
	version := fl.String("version", "", "release version to bind into the signature (recommended, e.g. 1.2.3)")
	out := fl.String("out", "", "signature output path (default <file>.sig)")
	force := fl.Bool("force", false, "overwrite an existing signature file")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return errors.New("expected exactly one file to sign (checksums.txt)")
	}
	file := fl.Arg(0)
	if *keyPath == "" {
		*keyPath = strings.TrimSpace(getenv(keyFileEnvVar))
	}
	if *keyPath == "" {
		return fmt.Errorf("no private key: pass -key <path> or set $%s", keyFileEnvVar)
	}
	if *out == "" {
		*out = file + ".sig"
	}
	if strings.ContainsAny(*version, "\t\r\n") {
		return errors.New("-version must not contain tabs or newlines")
	}
	if base := filepath.Base(file); base != "checksums.txt" {
		fmt.Fprintf(stderr, "warning: signing %q; gocode only accepts a signature whose file is checksums.txt\n", base)
	}

	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	sk, err := releasesig.ParseSecretKey(keyData)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	tc := fmt.Sprintf("timestamp:%d\tfile:%s\thashed", now().Unix(), filepath.Base(file))
	if *version != "" {
		tc += "\tversion:" + strings.TrimPrefix(*version, "v")
	}
	sig, err := releasesig.Sign(sk, msg, tc)
	if err != nil {
		return err
	}
	// Self-check before writing anything.
	if _, _, err := releasesig.Verify([]releasesig.PublicKey{sk.Public()}, msg, sig); err != nil {
		return fmt.Errorf("internal error: fresh signature does not verify: %w", err)
	}
	if *force {
		err = os.WriteFile(*out, sig, 0o644)
	} else {
		err = writeExclusive(*out, sig, 0o644)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed %s with key %s -> %s\n", file, sk.ID, *out)
	return nil
}

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("verify", flag.ContinueOnError)
	fl.SetOutput(stderr)
	pubs := fl.String("pubkey", "", "trusted public key(s), comma-separated base64")
	pubFile := fl.String("pubkey-file", "", "trusted public key file (.pub)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() < 1 || fl.NArg() > 2 {
		return errors.New("expected <file> [<sig>]")
	}
	file := fl.Arg(0)
	sigPath := file + ".sig"
	if fl.NArg() == 2 {
		sigPath = fl.Arg(1)
	}
	keys, err := releasesig.ParsePublicKeys(*pubs)
	if err != nil {
		return err
	}
	if *pubFile != "" {
		b, err := os.ReadFile(*pubFile)
		if err != nil {
			return err
		}
		k, err := releasesig.ParsePublicKey(string(b))
		if err != nil {
			return err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return errors.New("no trusted key: pass -pubkey or -pubkey-file")
	}
	msg, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return err
	}
	s, key, err := releasesig.Verify(keys, msg, sig)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signature OK: %s signed by key %s\nTrusted comment: %s\n", file, key.ID, s.TrustedComment)
	return nil
}

// writeExclusive creates path with perm, failing if it already exists.
func writeExclusive(path string, data []byte, perm os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
