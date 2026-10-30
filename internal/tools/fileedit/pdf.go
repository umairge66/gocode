package fileedit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PDF rendering constants.
//
// PDF pages are rendered to JPEG images by shelling out to poppler's
// pdftoppm, then each page image runs through the same image transport budget
// as a normal image read.
const (
	// pdfRenderDPI is the pdftoppm resolution.
	pdfRenderDPI = 100

	// pdfMaxPagesPerRead is the hard cap on pages per Read request.
	pdfMaxPagesPerRead = 20

	// pdfDefaultMaxPages bounds a read that gives no `pages` range: the first
	// ten pages are rendered and the bound is noted, so a page-less read of a
	// large PDF degrades rather than fails.
	pdfDefaultMaxPages = 10

	// pdfMaxFileBytes is the PDF file-size ceiling.
	pdfMaxFileBytes = 104857600

	// pdfRenderTimeout bounds the pdftoppm run.
	pdfRenderTimeout = 120 * time.Second

	// pdfDetectTimeout bounds the `pdftoppm -v` availability probe.
	pdfDetectTimeout = 5 * time.Second

	// pdfMaxOutputBytes caps the total rendered-page bytes read back from disk,
	// a safety bound against a pathological render. 20 pages * ~16MB.
	pdfMaxOutputBytes = pdfMaxPagesPerRead * 16 * 1024 * 1024
)

// pdfToolMissingMessage is returned (as a text result, not an error) when
// pdftoppm is not available.
const pdfToolMissingMessage = "PDF preview needs the poppler `pdftoppm` tool; it is not installed. Install poppler-utils (e.g. `brew install poppler` or `apt-get install poppler-utils`) to enable PDF page rendering, or extract the text another way (e.g. `pdftotext`)."

// errPDFToolUnavailable signals that the PDF rendering backend is not present.
// readPDF turns it into the graceful pdfToolMissingMessage text result.
var errPDFToolUnavailable = errors.New("pdftoppm is not available")

// pdfPageImage is one rendered PDF page.
type pdfPageImage struct {
	Data      []byte
	MediaType string
}

// pdfRenderer renders a range of PDF pages to page images. It is the injection
// seam that lets tests supply fake pages and exercise the missing-binary path
// without poppler installed. firstPage is 1-based; lastPage == 0 means "to the
// end of the document".
type pdfRenderer interface {
	Render(ctx context.Context, pdfPath string, firstPage, lastPage int) ([]pdfPageImage, error)
}

// pageRange is the parsed `pages` input. End == 0 means "to the end".
type pageRange struct {
	Start int
	End   int
}

var pageRangeRe = regexp.MustCompile(`^(\d{1,9})(?:\s*(-)\s*(\d{1,9})?)?$`)

// parsePages parses the `pages` parameter.
//
// It returns ok=false for malformed input or a backwards range.
func parsePages(pages string) (pageRange, bool) {
	m := pageRangeRe.FindStringSubmatch(strings.TrimSpace(pages))
	if m == nil {
		return pageRange{}, false
	}
	start, _ := strconv.Atoi(m[1])
	end := start
	switch {
	case m[3] != "": // "A-B"
		end, _ = strconv.Atoi(m[3])
	case m[2] == "-": // "A-"  -> to the end
		end = 0
	}
	if start < 1 || (end != 0 && end < start) {
		return pageRange{}, false
	}
	return pageRange{Start: start, End: end}, true
}

// readPDF renders PDF pages to images via the configured renderer (default:
// poppler's pdftoppm). A missing renderer degrades to a clear text result. The
// path has already passed Read's stat/validation gating before this is called.
func (r ReadTool) readPDF(ctx context.Context, path string, in ReadInput, info os.FileInfo) (ReadResult, error) {
	if info.Size() == 0 {
		return ReadResult{}, fmt.Errorf("PDF file is empty: %s", path)
	}
	if info.Size() > pdfMaxFileBytes {
		return ReadResult{}, fmt.Errorf("PDF file (%s) exceeds the maximum allowed size (%s).", formatBytes(info.Size()), formatBytes(pdfMaxFileBytes))
	}

	first, last := 1, pdfDefaultMaxPages
	if in.Pages != "" {
		pr, ok := parsePages(in.Pages)
		if !ok {
			return ReadResult{}, fmt.Errorf(`Invalid pages value %q. Use a page or range such as "3", "1-5" or "10-20".`, in.Pages)
		}
		first = pr.Start
		last = pr.End
		if last != 0 && last-first+1 > pdfMaxPagesPerRead {
			return ReadResult{}, fmt.Errorf("Requested %d pages, but a Read renders a maximum of %d PDF pages per request. Narrow the pages range.", last-first+1, pdfMaxPagesPerRead)
		}
	}

	renderer := r.PDFRenderer
	if renderer == nil {
		renderer = defaultPDFRenderer()
	}
	pages, err := renderer.Render(ctx, path, first, last)
	if err != nil {
		if errors.Is(err, errPDFToolUnavailable) {
			return ReadResult{
				Type:     ReadTypeText,
				FilePath: path,
				Text:     pdfToolMissingMessage,
			}, nil
		}
		return ReadResult{}, err
	}
	if len(pages) == 0 {
		return ReadResult{}, fmt.Errorf("pdftoppm produced no output pages for %s. The PDF may be invalid.", path)
	}

	// Cap the number of pages defensively and run each page image through the
	// same transport budget as a normal image read.
	if len(pages) > pdfMaxPagesPerRead {
		pages = pages[:pdfMaxPagesPerRead]
	}
	images := make([]ReadImage, 0, len(pages))
	for _, pg := range pages {
		img, format, derr := decodeImage(pg.Data)
		if derr != nil {
			// Should not happen for pdftoppm JPEG output; fall back to raw bytes
			// if small enough, else skip the page with no hard failure.
			if int64(len(pg.Data)) <= imageByteBudget {
				images = append(images, ReadImage{Base64: b64(pg.Data), MediaType: mediaOrDefault(pg.MediaType)})
			}
			continue
		}
		ri, eerr := encodeImageForTransport(pg.Data, img, format, mediaOrDefault(pg.MediaType))
		if eerr != nil {
			continue
		}
		images = append(images, ri)
	}
	if len(images) == 0 {
		return ReadResult{}, fmt.Errorf("could not process any rendered page of %s", path)
	}

	summary := fmt.Sprintf("PDF read: %s — %d page(s) rendered as images.", path, len(images))
	if in.Pages != "" {
		summary = fmt.Sprintf("PDF read: %s — %d page(s) rendered as images (requested pages %q).", path, len(images), in.Pages)
	}
	return ReadResult{
		Type:      ReadTypeImage,
		FilePath:  path,
		Images:    images,
		PageCount: len(images),
		Text:      summary,
	}, nil
}

func mediaOrDefault(m string) string {
	if m == "" {
		return "image/jpeg"
	}
	return m
}

// --- poppler (pdftoppm) renderer -------------------------------------------

// popplerRenderer shells out to pdftoppm. lookPath and tempDir are overridable
// for tests; both default to the obvious implementations.
type popplerRenderer struct {
	lookPath func(string) (string, error)
	tempDir  string // base dir for the scratch render directory ("" -> os.TempDir)
}

func defaultPDFRenderer() pdfRenderer {
	return popplerRenderer{lookPath: exec.LookPath}
}

// available reports whether pdftoppm can be found and runs (`pdftoppm -v`:
// available if exit==0 or (exit!=127 && stderr non-empty)).
func (p popplerRenderer) available(ctx context.Context) bool {
	look := p.lookPath
	if look == nil {
		look = exec.LookPath
	}
	bin, err := look("pdftoppm")
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, pdfDetectTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "-v")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code := exitErr.ExitCode()
		return code != 127 && stderr.Len() > 0
	}
	return false
}

func (p popplerRenderer) Render(ctx context.Context, pdfPath string, firstPage, lastPage int) ([]pdfPageImage, error) {
	if !p.available(ctx) {
		return nil, errPDFToolUnavailable
	}
	bin, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, errPDFToolUnavailable
	}

	base := p.tempDir
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "gocode-pdf-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir) // temp cleanup

	prefix := filepath.Join(dir, "page")
	args := []string{"-jpeg", "-r", strconv.Itoa(pdfRenderDPI)}
	if firstPage > 0 {
		args = append(args, "-f", strconv.Itoa(firstPage))
	}
	if lastPage > 0 {
		args = append(args, "-l", strconv.Itoa(lastPage))
	}
	args = append(args, pdfPath, prefix)

	cctx, cancel := context.WithTimeout(ctx, pdfRenderTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("pdftoppm timed out after %s rendering %s", pdfRenderTimeout, pdfPath)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		if looksCorruptPDF(msg) {
			return nil, fmt.Errorf("PDF file is corrupted or invalid: %s", pdfPath)
		}
		return nil, fmt.Errorf("pdftoppm failed: %s", msg)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var pages []pdfPageImage
	var total int
	for _, name := range names {
		data, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, rerr
		}
		total += len(data)
		if total > pdfMaxOutputBytes {
			return nil, fmt.Errorf("rendered PDF pages exceed the output budget (%s)", formatBytes(pdfMaxOutputBytes))
		}
		pages = append(pages, pdfPageImage{Data: data, MediaType: "image/jpeg"})
		if len(pages) >= pdfMaxPagesPerRead {
			break
		}
	}
	return pages, nil
}

var corruptRe = regexp.MustCompile(`(?i)damaged|corrupt|invalid|Couldn't (?:find trailer dictionary|read xref table)`)

func looksCorruptPDF(stderr string) bool { return corruptRe.MatchString(stderr) }
