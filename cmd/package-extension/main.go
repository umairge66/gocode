// Command package-extension builds the Chrome Web Store upload zip of the
// gocode extension: <out>/gocode-chrome-extension-<version>.zip plus a
// matching .sha256 file. The zip holds the extension's runtime files with the
// "key" field removed from manifest.json, and is byte-for-byte reproducible.
// It reads only the files built into the extension package and makes no
// network calls. scripts/package-extension.sh is the usual way to run it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/umairge66/gocode/extension"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("package-extension", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "dist", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path, sum, err := writeZip(*out)
	if err != nil {
		fmt.Fprintf(stderr, "package-extension: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\nsha256 %s\n", path, sum)
	return 0
}

// zipName is the upload zip's file name for an extension version.
func zipName(version string) string {
	return "gocode-chrome-extension-" + version + ".zip"
}

// writeZip writes the zip and its .sha256 file into dir and returns the zip
// path and its hex sha256.
func writeZip(dir string) (string, string, error) {
	var buf bytes.Buffer
	if err := extension.WriteStoreZip(&buf); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	name := zipName(extension.Version())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", "", err
	}
	h := sha256.Sum256(buf.Bytes())
	sum := hex.EncodeToString(h[:])
	if err := os.WriteFile(path+".sha256", []byte(sum+"  "+name+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return path, sum, nil
}
