package fileedit

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"strings"

	// image/png and image/jpeg are imported by name (used for encoding) and so
	// also register their decoders; image/gif is imported blank purely to
	// register the GIF decoder for image.Decode / image.DecodeConfig. WebP and
	// BMP are deliberately absent: stdlib cannot decode them and x/image is not
	// a dependency.
	_ "image/gif"
)

// Image transport limits.
//
// Every image is resized and re-encoded to a transport budget before it is
// attached to a tool_result. The resampling uses only the standard library.
const (
	// imageLongEdgeCap is the maximum width/height of a returned image; larger
	// images are downscaled to fit, preserving aspect ratio.
	imageLongEdgeCap = 1568

	// imageMaxPixels rejects absurdly large images before decode (an 8000x8000
	// guard against decode bombs).
	imageMaxPixels = 8000 * 8000

	// imageByteBudget caps the encoded bytes of a returned image; the re-encode
	// ladder keeps trying lower-quality encodings until the result fits.
	imageByteBudget = 512000

	// imageMaxReadBytes is the raw-file ceiling for reading a file as an image.
	imageMaxReadBytes = 134217728

	// fallbackSide is the final-resort downscale when nothing else fits the
	// byte budget.
	fallbackSide = 400
)

// jpegQualityLadder is the descending JPEG quality sequence tried to fit the
// byte budget.
var jpegQualityLadder = []int{80, 60, 40, 20}

// ReadImage is one image content block produced by a Read: base64-encoded image
// data plus its media type, matching the structured image tool_result the model
// consumes ({type:"image",source:{type:"base64",media_type,data}}).
type ReadImage struct {
	Base64    string
	MediaType string
}

// unsupportedImageNote is returned (as a text result, not an error) when the
// file is an image stdlib cannot decode (WebP/BMP in this build).
func unsupportedImageNote(path, mediaType string) string {
	return fmt.Sprintf("[%s: unsupported image format for inline view (%s). This build decodes PNG, JPEG and GIF; convert the image to one of those to view it inline, or read it another way.]", path, mediaType)
}

// readImage reads an image file, resizing/re-encoding it to the transport
// budget, and returns it as structured image content. Decode failures on a
// decodable format produce a clear error; formats stdlib cannot decode produce
// a clear note (not a hard failure).
func (r ReadTool) readImage(path, mediaType string, info os.FileInfo) (ReadResult, error) {
	if info.Size() > imageMaxReadBytes {
		return ReadResult{}, fmt.Errorf("File is too large to read as an image (the limit is %s): %s. If it is an image, resize it or export a smaller copy, then read that.", formatBytes(imageMaxReadBytes), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ReadResult{}, err
	}
	r.record(path, data, info)
	if len(data) == 0 {
		return ReadResult{}, fmt.Errorf("Image file is empty: %s", path)
	}

	stdlibDecodable := map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true}

	img, srcFormat, dec := decodeImage(data)
	if dec != nil {
		// Header could not be read.
		if !stdlibDecodable[mediaType] {
			// WebP/BMP etc.: a clear note, not a failure.
			return imageNoteResult(path, mediaType, info.Size(), unsupportedImageNote(path, mediaType)), nil
		}
		// A PNG/JPEG/GIF whose pixels could not be decoded. Fall back to the raw
		// bytes when they still fit the byte budget; otherwise a clean error.
		if int64(len(data)) <= imageByteBudget {
			return singleImageResult(path, ReadImage{Base64: b64(data), MediaType: mediaType}, info.Size()), nil
		}
		return ReadResult{}, fmt.Errorf("Could not read this as an image: %s (%v). Re-export it as a PNG or JPEG and try again.", path, dec)
	}

	ri, err := encodeImageForTransport(data, img, srcFormat, mediaType)
	if err != nil {
		return ReadResult{}, err
	}
	return singleImageResult(path, ri, info.Size()), nil
}

// TransportImage fits image bytes from another source (an MCP tool result) to
// the same transport budget a Read applies: decodable images (PNG, JPEG, GIF)
// are resized and re-encoded like a read image file; a format this build
// cannot decode is passed through only when the model accepts it as is (WebP)
// and it already fits the byte budget. Anything else is an error, which the
// caller shows as a text note instead of the image.
func TransportImage(data []byte, mediaType string) (ReadImage, error) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if len(data) == 0 {
		return ReadImage{}, fmt.Errorf("empty image")
	}
	if len(data) > imageMaxReadBytes {
		return ReadImage{}, fmt.Errorf("image is too large (the limit is %s)", formatBytes(imageMaxReadBytes))
	}
	img, srcFormat, err := decodeImage(data)
	if err != nil {
		if mediaType == "image/webp" && len(data) <= imageByteBudget {
			return ReadImage{Base64: b64(data), MediaType: mediaType}, nil
		}
		return ReadImage{}, fmt.Errorf("unsupported or undecodable image (%s)", mediaType)
	}
	return encodeImageForTransport(data, img, srcFormat, mediaType)
}

// decodeImage sniffs and decodes an image. It guards against pixel bombs via
// DecodeConfig before fully decoding.
func decodeImage(data []byte) (image.Image, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > imageMaxPixels {
		return nil, format, fmt.Errorf("image dimensions out of range (%dx%d)", cfg.Width, cfg.Height)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, format, err
	}
	return img, format, nil
}

// encodeImageForTransport resizes (if over the long-edge cap) and re-encodes an
// image to fit the byte budget, returning base64 content + media type. The
// ladder: pass raw through when already small enough; else
// resize, try PNG (for PNG sources), then a descending JPEG-quality ladder, then
// a last-resort small JPEG.
func encodeImageForTransport(raw []byte, img image.Image, srcFormat, mediaType string) (ReadImage, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	longEdge := w
	if h > longEdge {
		longEdge = h
	}
	needResize := longEdge > imageLongEdgeCap

	normMedia := normMediaType(srcFormat, mediaType)
	if !needResize && len(raw) <= imageByteBudget {
		return ReadImage{Base64: b64(raw), MediaType: normMedia}, nil
	}

	work := img
	if needResize {
		nw, nh := fitInside(w, h, imageLongEdgeCap, imageLongEdgeCap)
		work = downscale(img, nw, nh)
	}

	// PNG sources: prefer a lossless PNG re-encode when it fits.
	if normMedia == "image/png" {
		if enc, err := encodePNG(work); err == nil && len(enc) <= imageByteBudget {
			return ReadImage{Base64: b64(enc), MediaType: "image/png"}, nil
		}
	}
	for _, q := range jpegQualityLadder {
		if enc, err := encodeJPEG(work, q); err == nil && len(enc) <= imageByteBudget {
			return ReadImage{Base64: b64(enc), MediaType: "image/jpeg"}, nil
		}
	}
	// Last resort: a small JPEG, returned regardless of the budget.
	fw, fh := fitInside(work.Bounds().Dx(), work.Bounds().Dy(), fallbackSide, fallbackSide)
	small := downscale(work, fw, fh)
	enc, err := encodeJPEG(small, 20)
	if err != nil {
		return ReadImage{}, fmt.Errorf("image re-encode failed: %w", err)
	}
	return ReadImage{Base64: b64(enc), MediaType: "image/jpeg"}, nil
}

// fitInside returns the largest (w,h) within (maxW,maxH) preserving aspect.
func fitInside(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	rw := float64(maxW) / float64(w)
	rh := float64(maxH) / float64(h)
	s := rw
	if rh < s {
		s = rh
	}
	nw := int(float64(w)*s + 0.5)
	nh := int(float64(h)*s + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

// downscale resamples src to nw x nh using box averaging (high quality for
// downscaling, stdlib only). It is a no-op copy when the target matches src.
func downscale(src image.Image, nw, nh int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	if nw >= sw && nh >= sh {
		// No downscaling requested: straight copy.
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
		return dst
	}
	for dy := 0; dy < nh; dy++ {
		sy0 := b.Min.Y + dy*sh/nh
		sy1 := b.Min.Y + (dy+1)*sh/nh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < nw; dx++ {
			sx0 := b.Min.X + dx*sw/nw
			sx1 := b.Min.X + (dx+1)*sw/nw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sr, sg, sb, sa, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA() // 16-bit premultiplied
					sr += uint64(cr)
					sg += uint64(cg)
					sb += uint64(cb)
					sa += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8((sr / n) >> 8),
				G: uint8((sg / n) >> 8),
				B: uint8((sb / n) >> 8),
				A: uint8((sa / n) >> 8),
			})
		}
	}
	return dst
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// normMediaType resolves the media type of the source, preferring the decoded
// format over the extension-derived media type.
func normMediaType(srcFormat, fallback string) string {
	switch strings.ToLower(srcFormat) {
	case "png":
		return "image/png"
	case "jpeg", "jpg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	}
	if fallback != "" {
		return fallback
	}
	return "image/png"
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// singleImageResult builds a ReadResult carrying one image.
func singleImageResult(path string, ri ReadImage, originalSize int64) ReadResult {
	return ReadResult{
		Type:         ReadTypeImage,
		FilePath:     path,
		Images:       []ReadImage{ri},
		ImageBase64:  ri.Base64,
		MediaType:    ri.MediaType,
		OriginalSize: originalSize,
		Text:         fmt.Sprintf("Image file read: %s (%s, %s)", path, ri.MediaType, formatBytes(originalSize)),
	}
}

// imageNoteResult builds a text ReadResult carrying a note (unsupported format).
func imageNoteResult(path, mediaType string, originalSize int64, note string) ReadResult {
	return ReadResult{
		Type:         ReadTypeText,
		FilePath:     path,
		OriginalSize: originalSize,
		Text:         note,
	}
}
