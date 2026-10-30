package fileedit

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fake PDF renderers -----------------------------------------------------

type unavailableRenderer struct{}

func (unavailableRenderer) Render(context.Context, string, int, int) ([]pdfPageImage, error) {
	return nil, errPDFToolUnavailable
}

type fakeRenderer struct {
	pages []pdfPageImage
	gotF  int
	gotL  int
}

func (f *fakeRenderer) Render(_ context.Context, _ string, first, last int) ([]pdfPageImage, error) {
	f.gotF, f.gotL = first, last
	return f.pages, nil
}

// --- helpers ----------------------------------------------------------------

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeDims(t *testing.T, b64 string) (image.Image, int, int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode returned image: %v", err)
	}
	return img, img.Bounds().Dx(), img.Bounds().Dy()
}

// --- image tests ------------------------------------------------------------

func TestReadImageSmallPassthrough(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small.png")
	data := makePNG(t, 16, 16)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != ReadTypeImage || len(res.Images) != 1 {
		t.Fatalf("type/images: %+v", res)
	}
	if res.Images[0].MediaType != "image/png" {
		t.Fatalf("media: %q", res.Images[0].MediaType)
	}
	// A small image under the byte budget is passed through unchanged.
	if res.Images[0].Base64 != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("expected passthrough of raw bytes")
	}
	if res.ImageBase64 != res.Images[0].Base64 || res.MediaType != "image/png" {
		t.Fatalf("single-image mirror fields not set: %+v", res)
	}
}

func TestReadImageResizeOverCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	if err := os.WriteFile(p, makePNG(t, 3000, 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != ReadTypeImage || len(res.Images) != 1 {
		t.Fatalf("type/images: %+v", res)
	}
	_, w, h := decodeDims(t, res.Images[0].Base64)
	if w > imageLongEdgeCap || h > imageLongEdgeCap {
		t.Fatalf("not downscaled to cap: %dx%d (cap %d)", w, h, imageLongEdgeCap)
	}
	if w != imageLongEdgeCap {
		t.Fatalf("expected long edge == %d, got %dx%d", imageLongEdgeCap, w, h)
	}
	raw, _ := base64.StdEncoding.DecodeString(res.Images[0].Base64)
	if len(raw) > imageByteBudget {
		t.Fatalf("output over byte budget: %d > %d", len(raw), imageByteBudget)
	}
}

func TestReadImageCorruptOverBudget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "corrupt.png")
	junk := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0x7f, 0x1a}, imageByteBudget/6+1000)
	if err := os.WriteFile(p, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err == nil {
		t.Fatalf("expected a clean error for a corrupt over-budget image")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Fatalf("error not clear: %v", err)
	}
}

func TestReadImageUnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.webp")
	// RIFF/WEBP header + junk; stdlib cannot decode WebP.
	if err := os.WriteFile(p, append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{1, 2, 3}, 40)...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadTool{}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatalf("unsupported format should not hard-fail: %v", err)
	}
	if res.Type != ReadTypeText || !strings.Contains(res.Text, "unsupported image format") {
		t.Fatalf("expected unsupported note, got %+v", res)
	}
}

// --- PDF tests --------------------------------------------------------------

func TestReadPDFFakeRenderer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4\n...body..."), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRenderer{pages: []pdfPageImage{
		{Data: makeJPEG(t, 80, 100), MediaType: "image/jpeg"},
		{Data: makeJPEG(t, 80, 100), MediaType: "image/jpeg"},
	}}
	res, err := ReadTool{PDFRenderer: fr}.Execute(ctx, ReadInput{FilePath: p, Pages: "1-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != ReadTypeImage || res.PageCount != 2 || len(res.Images) != 2 {
		t.Fatalf("pdf result: %+v", res)
	}
	for i, im := range res.Images {
		if im.Base64 == "" || !strings.HasPrefix(im.MediaType, "image/") {
			t.Fatalf("page %d bad image: %+v", i, im)
		}
	}
	if fr.gotF != 1 || fr.gotL != 2 {
		t.Fatalf("renderer got range %d-%d, want 1-2", fr.gotF, fr.gotL)
	}
}

func TestReadPDFDefaultRange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRenderer{pages: []pdfPageImage{{Data: makeJPEG(t, 40, 40), MediaType: "image/jpeg"}}}
	if _, err := (ReadTool{PDFRenderer: fr}).Execute(ctx, ReadInput{FilePath: p}); err != nil {
		t.Fatal(err)
	}
	if fr.gotF != 1 || fr.gotL != pdfDefaultMaxPages {
		t.Fatalf("default range %d-%d, want 1-%d", fr.gotF, fr.gotL, pdfDefaultMaxPages)
	}
}

func TestReadPDFMissingTool(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadTool{PDFRenderer: unavailableRenderer{}}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatalf("missing tool must not fail: %v", err)
	}
	if res.Type != ReadTypeText || !strings.Contains(res.Text, "pdftoppm") || !strings.Contains(res.Text, "not installed") {
		t.Fatalf("expected graceful not-installed message, got %+v", res)
	}
}

func TestReadPDFTooManyPages(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 1-21 is 21 pages, over the cap of 20.
	_, err := (ReadTool{PDFRenderer: &fakeRenderer{}}).Execute(ctx, ReadInput{FilePath: p, Pages: "1-21"})
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("expected max-pages error, got %v", err)
	}
}

func TestPopplerMissingBinaryGraceful(t *testing.T) {
	// The default renderer degrades gracefully when the binary is not found.
	r := popplerRenderer{lookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	_, err := r.Render(context.Background(), "/nonexistent.pdf", 1, 1)
	if err != errPDFToolUnavailable {
		t.Fatalf("expected errPDFToolUnavailable, got %v", err)
	}
}

func TestParsePages(t *testing.T) {
	cases := []struct {
		in         string
		start, end int
		ok         bool
	}{
		{"3", 3, 3, true},
		{"1-5", 1, 5, true},
		{"10-20", 10, 20, true},
		{"10-", 10, 0, true},
		{"0", 0, 0, false},
		{"5-2", 0, 0, false},
		{"", 0, 0, false},
		{"abc", 0, 0, false},
	}
	for _, c := range cases {
		pr, ok := parsePages(c.in)
		if ok != c.ok {
			t.Fatalf("%q ok=%v want %v", c.in, ok, c.ok)
		}
		if ok && (pr.Start != c.start || pr.End != c.end) {
			t.Fatalf("%q => %+v want {%d,%d}", c.in, pr, c.start, c.end)
		}
	}
}
