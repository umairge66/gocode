package fileedit

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestTransportImage(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	ri, err := TransportImage(b.Bytes(), "image/png")
	if err != nil || ri.MediaType != "image/png" || ri.Base64 == "" {
		t.Fatalf("png: %+v %v", ri, err)
	}
	// WebP cannot be decoded here but the model accepts it as is.
	if ri, err := TransportImage([]byte("RIFF....WEBPVP8 "), "image/webp"); err != nil || ri.MediaType != "image/webp" {
		t.Fatalf("webp: %+v %v", ri, err)
	}
	for _, c := range []struct {
		data []byte
		mime string
	}{{nil, "image/png"}, {[]byte("<svg/>"), "image/svg+xml"}, {[]byte("garbage"), "image/png"}} {
		if _, err := TransportImage(c.data, c.mime); err == nil {
			t.Errorf("%s %q: expected an error", c.mime, c.data)
		}
	}
}
