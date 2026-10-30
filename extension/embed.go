// Package extension carries gocode's Chrome extension (the files in this
// directory) inside the gocode binary, so `gocode chrome install` can write it
// to disk for "Load unpacked" without a source checkout, and so the Chrome Web
// Store upload zip is built from exactly the same files.
//
// Only the runtime files listed in RuntimeFiles are embedded; documentation,
// store listing material (store/), the icons' SVG sources (icons/src/) and
// this Go file are not part of the extension Chrome loads.
package extension

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"time"
)

//go:embed manifest.json background.js icons/*.png
var files embed.FS

// RuntimeFiles lists, in sorted order, every file Chrome needs to run the
// extension. It is the complete content of the embedded extension, of the
// directory `gocode chrome install` writes, and of the store upload zip.
// Names use forward slashes; icons live in the icons/ subdirectory.
var RuntimeFiles = []string{
	"background.js",
	"icons/icon-128.png",
	"icons/icon-16.png",
	"icons/icon-32.png",
	"icons/icon-48.png",
	"manifest.json",
}

// FS returns the embedded runtime files.
func FS() fs.FS { return files }

// ReadFile returns one embedded runtime file.
func ReadFile(name string) ([]byte, error) { return files.ReadFile(name) }

// Manifest returns the embedded manifest.json.
func Manifest() []byte {
	b, err := files.ReadFile("manifest.json")
	if err != nil {
		panic("extension: manifest.json is not embedded: " + err.Error())
	}
	return b
}

// ManifestInfo is the part of manifest.json gocode reads.
type ManifestInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Key is the base64 public key that pins the unpacked extension's id.
	Key string `json:"key"`
}

// ParseManifest decodes the fields gocode uses from a manifest.json.
func ParseManifest(b []byte) (ManifestInfo, error) {
	var m ManifestInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return ManifestInfo{}, fmt.Errorf("parse extension manifest: %w", err)
	}
	return m, nil
}

// Info returns the embedded manifest's name, version and key.
func Info() ManifestInfo {
	m, err := ParseManifest(Manifest())
	if err != nil {
		panic("extension: embedded " + err.Error())
	}
	return m
}

// Version is the embedded extension's version.
func Version() string { return Info().Version }

// StoreManifest returns manifest.json as uploaded to the Chrome Web Store: the
// same manifest, fields in the same order, without the "key" field (the store
// assigns the published extension its own id and does not accept a key).
func StoreManifest() ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(Manifest()))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("extension manifest is not a JSON object")
	}
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse extension manifest: %w", err)
		}
		name, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("parse extension manifest: %w", err)
		}
		if name == "key" {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, b.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// zipTime is the fixed modification time stamped on every zip entry so that
// the same sources always produce byte-identical zips.
var zipTime = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// WriteStoreZip writes the Chrome Web Store upload zip: RuntimeFiles in sorted
// order, the manifest without its "key", fixed timestamps and permissions,
// stored uncompressed so no compressor version can change the bytes.
// The output depends only on the embedded files.
func WriteStoreZip(w io.Writer) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range RuntimeFiles {
		var data []byte
		var err error
		if name == "manifest.json" {
			data, err = StoreManifest()
		} else {
			data, err = files.ReadFile(name)
		}
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Store, Modified: zipTime}
		hdr.SetMode(0o644)
		f, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
