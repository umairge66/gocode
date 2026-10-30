package fileedit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ToolNameRead is the Read tool name.
const ToolNameRead = "Read"

// Read constants.
const (
	// DefaultLineLimit is the documented default number of lines a Read
	// returns ("Reads up to 2000 lines by default").
	DefaultLineLimit = 2000

	// DefaultMaxSizeBytes is the default cap on a whole-file text read (no
	// limit given).
	DefaultMaxSizeBytes = 262144

	// AllowLargeMaxSizeBytes is the hard ceiling for the buffered read path,
	// used here as the cap when allow_large is set.
	AllowLargeMaxSizeBytes = 10485760

	// DefaultMaxLineChars truncates a single very long line. This cap and its
	// notice keep output bounded without a tokenizer. Set ReadTool.MaxLineChars < 0 to disable.
	DefaultMaxLineChars = 2000

	// partialViewPrefix starts the paginated-read banner.
	partialViewPrefix = "[Truncated: PARTIAL view — "

	// Empty-content reminders.
	msgEmptyFile = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
)

// ReadPrompt is the model-facing usage text.
const ReadPrompt = `Reads a file from the local filesystem. You can access any file directly by using this tool.
Assume this tool is able to read all files on the machine. If the User provides a path to a file assume that path is valid. It is okay to read a file that does not exist; an error will be returned.

Usage:
- The file_path parameter must be an absolute path, not a relative path
- By default, it reads up to 2000 lines starting from the beginning of the file
- When you already know which part of the file you need, only read that part. This can be important for larger files.
- Results are returned using cat -n format, with line numbers starting at 1
- This tool allows gocode to read images (eg PNG, JPG, etc). When reading an image file the contents are presented visually as gocode is a multimodal LLM.
- This tool can read PDF files (.pdf). For large PDFs (more than 10 pages), you MUST provide the pages parameter to read specific page ranges (e.g., pages: "1-5"). Reading a large PDF without the pages parameter will fail. Maximum 20 pages per request.
- This tool can read Jupyter notebooks (.ipynb files) and returns all cells with their outputs, combining code, text, and visualizations.
- This tool can only read files, not directories. To list files in a directory, use the registered shell tool.
- You will regularly be asked to read screenshots. If the user provides a path to a screenshot, ALWAYS use this tool to view the file at the path. This tool will work with all temporary file paths.
- If you read a file that exists but has empty contents you will receive a system reminder warning in place of file contents.
- Do NOT re-read a file you just edited to verify ` + "—" + ` Edit/Write would have errored if the change failed, and the harness tracks file state for you.`

// ReadCompactPrompt is the compact usage text a session
// advertises: the same rules as ReadPrompt, without repetition.
const ReadCompactPrompt = `Reads a file from the local filesystem; any file on the machine can be read, and a missing file returns an error.
- file_path must be absolute.
- Reads up to 2000 lines from the start by default; when you know which part you need, read only that part (offset/limit).
- Output uses cat -n format, line numbers starting at 1.
- Images (PNG, JPG, …) are shown visually. PDFs over 10 pages need pages (max 20 per request). Jupyter notebooks return all cells with outputs.
- Files only, not directories: list a directory with the shell tool.
- An existing empty file returns a system reminder instead of contents.
- Do NOT re-read a file you just edited to verify ` + "—" + ` Edit/Write would have errored if the change failed.`

// readCompactInputSchema is readInputSchema with shorter parameter
// descriptions; properties, types, bounds and required fields match.
var readCompactInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"file_path":{"type":"string","description":"Absolute path of the file"},` +
	`"offset":{"type":"integer","minimum":0,"description":"Line to start from (large files)"},` +
	`"limit":{"type":"integer","exclusiveMinimum":0,"description":"Number of lines to read (large files)"},` +
	`"pages":{"type":"string","description":"PDF page range, e.g. \"1-5\" (max 20 pages)"},` +
	`"allow_large":{"type":"boolean","description":"Read text over the usual size limit, up to what fits in context. Only when you genuinely need all of it."}` +
	`},"required":["file_path"],"additionalProperties":false}`)

// ReadCompactInputSchema returns the Read input schema a session
// advertises.
func ReadCompactInputSchema() json.RawMessage { return readCompactInputSchema }

// readInputSchema is the Read input schema.
var readInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"file_path":{"type":"string","description":"The absolute path to the file to read"},` +
	`"offset":{"type":"integer","minimum":0,"description":"The line number to start reading from. Only provide if the file is too large to read at once"},` +
	`"limit":{"type":"integer","exclusiveMinimum":0,"description":"The number of lines to read. Only provide if the file is too large to read at once."},` +
	`"pages":{"type":"string","description":"Page range for PDF files (e.g., \"1-5\", \"3\", \"10-20\"). Only applicable to PDF files. Maximum 20 pages per request."},` +
	`"allow_large":{"type":"boolean","description":"Set to true to read a text file, or a line range of one, that is over the usual size limits, up to what still fits in your context. Only use this when you genuinely need all of it or the user asked for the whole file; otherwise read it in parts with offset and limit, or search it."}` +
	`},"required":["file_path"],"additionalProperties":false}`)

// ReadInput is the Read input.
type ReadInput struct {
	FilePath string `json:"file_path"`
	// Offset is the 1-based line to start from; 0 and 1 both mean the first
	// line.
	Offset *int `json:"offset,omitempty"`
	// Limit is the number of lines to read (positive).
	Limit *int `json:"limit,omitempty"`
	// Pages is the PDF page range (accepted, PDFs unsupported in this build).
	Pages string `json:"pages,omitempty"`
	// AllowLarge lifts the whole-file size cap.
	AllowLarge bool `json:"allow_large,omitempty"`
}

// Read result kinds.
const (
	ReadTypeText     = "text"
	ReadTypeImage    = "image"
	ReadTypeNotebook = "notebook"
	// ReadTypeDirectory is a Read of a directory: the result lists its entries
	// (rather than failing with EISDIR).
	ReadTypeDirectory = "directory"
)

// NotebookCellView is one rendered notebook cell.
type NotebookCellView struct {
	CellID   string   `json:"cell_id"`
	CellType string   `json:"cell_type"`
	Language string   `json:"language,omitempty"`
	Source   string   `json:"source"`
	Outputs  []string `json:"outputs,omitempty"`
}

// ReadResult reports what Read returned. Text is the tool_result text the
// model sees (for images it is a short caption; the image itself is in
// ImageBase64/MediaType).
type ReadResult struct {
	Type       string
	FilePath   string
	Content    string // selected raw content (text reads)
	NumLines   int
	StartLine  int
	TotalLines int
	Truncated  bool // the default line cap cut the read short

	// Images carries the structured image content blocks the model consumes:
	// one block for an image read, one per rendered page for a PDF read. Each
	// is resized/re-encoded to the transport budget.
	Images    []ReadImage
	PageCount int // number of rendered pages (PDF reads)

	// ImageBase64/MediaType mirror Images[0] for the single-image case.
	ImageBase64  string
	MediaType    string
	OriginalSize int64

	Cells []NotebookCellView

	Text string
}

// Read errors.
var (
	ErrReadEmptyPath = errors.New("file_path is required")
	ErrReadNotAbs    = errors.New("The file_path parameter must be an absolute path, not a relative path")
	ErrReadBadLimit  = errors.New("limit must be a positive integer")
	ErrReadBadOffset = errors.New("offset must be a non-negative integer")
)

// imageExts are the image types Read returns visually.
var imageExts = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
	"gif": "image/gif", "webp": "image/webp",
}

// binaryExts are refused outright (images and PDFs are handled separately).
var binaryExts = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`.png .jpg .jpeg .gif .bmp .ico .webp .tiff .tif .heic .heif .avif .mp4 .mov .avi .mkv .webm .wmv .flv .m4v .mpeg .mpg .mp3 .wav .ogg .flac .aac .m4a .wma .aiff .opus .zip .tar .gz .bz2 .7z .rar .xz .z .tgz .iso .zst .lz4 .whl .exe .dll .so .dylib .bin .o .a .obj .lib .app .msi .deb .rpm .dmg .apk .pdf .doc .docx .docm .dotx .dotm .xls .xlsx .xlsm .xlsb .xltx .xltm .ppt .pptx .pptm .ppsx .ppsm .potx .potm .odt .ods .odp .numbers .epub .ttf .otf .woff .woff2 .eot .pyc .pyo .class .jar .war .ear .node .wasm .rlib .sqlite .sqlite3 .db .mdb .idx .parquet .feather .npy .npz .pickle .psd .ai .eps .sketch .fig .xd .blend .3ds .max .swf .fla .lockb .dat .data`) {
		m[e] = true
	}
	return m
}()

// ReadTool reads files with line numbering, paging and size rules.
type ReadTool struct {
	// Tracker, when set, records each successful read so Edit/MultiEdit/Write
	// can verify the file is unchanged before modifying it.
	Tracker *FileStateTracker
	// MaxSizeBytes overrides DefaultMaxSizeBytes when > 0.
	MaxSizeBytes int64
	// MaxLineChars overrides DefaultMaxLineChars when > 0; < 0 disables.
	MaxLineChars int
	// MaxTokens overrides the token budget of a text read when > 0 (see
	// readtokens.go).
	MaxTokens int
	// PDFRenderer renders PDF pages to images; nil uses the poppler (pdftoppm)
	// renderer. Tests inject a fake renderer here.
	PDFRenderer pdfRenderer
	// GrepViaBash makes notices point to `grep` in Bash for finding a section
	// of a file, for sessions without the Grep tool.
	GrepViaBash bool
}

// Name returns the tool name.
func (ReadTool) Name() string { return ToolNameRead }

// searchToolName names what finds a section of a file in notices: the Grep
// tool, or `grep` in Bash when the session searches that way.
func (r ReadTool) searchToolName() string {
	if r.GrepViaBash {
		return "`grep` in Bash"
	}
	return "Grep"
}

// Prompt returns the model-facing usage text.
func (ReadTool) Prompt() string { return ReadPrompt }

// InputSchema returns the JSON input schema.
func (ReadTool) InputSchema() json.RawMessage { return readInputSchema }

// Execute reads in.FilePath.
func (r ReadTool) Execute(ctx context.Context, in ReadInput) (ReadResult, error) {
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	if in.FilePath == "" {
		return ReadResult{}, ErrReadEmptyPath
	}
	if !filepath.IsAbs(in.FilePath) {
		return ReadResult{}, ErrReadNotAbs
	}
	if in.Offset != nil && *in.Offset < 0 {
		return ReadResult{}, ErrReadBadOffset
	}
	if in.Limit != nil && *in.Limit <= 0 {
		return ReadResult{}, ErrReadBadLimit
	}
	path := filepath.Clean(in.FilePath)
	extDot := strings.ToLower(filepath.Ext(path))
	ext := strings.TrimPrefix(extDot, ".")

	// Binary refusal (validateInput errorCode 4).
	if binaryExts[extDot] && imageExts[ext] == "" && ext != "pdf" {
		return ReadResult{}, fmt.Errorf("This tool cannot read binary files. The file appears to be a binary %s file. Use a skill for this file type if one is available, or a shell command or script that can read the format.", extDot)
	}

	// Device / proc files that would block or stream forever (checked on the
	// requested spelling before any stat).
	if IsBlockingDevicePath(path) {
		return ReadResult{}, fmt.Errorf("Cannot read '%s': this device file would block or produce infinite output.", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReadResult{}, errors.New(fileNotExistMessage(path))
		}
		return ReadResult{}, err
	}
	if info.IsDir() {
		return readDirectory(path)
	}
	if !info.Mode().IsRegular() {
		return ReadResult{}, fmt.Errorf("Cannot read '%s': this device file would block or produce infinite output.", path)
	}

	switch {
	case ext == "ipynb":
		return r.readNotebook(path, in, info)
	case imageExts[ext] != "":
		return r.readImage(path, imageExts[ext], info)
	case ext == "pdf":
		return r.readPDF(ctx, path, in, info)
	}
	return r.readText(path, in, info)
}

// maxDirEntries caps the entries a directory Read lists.
const maxDirEntries = 200

// readDirectory answers a Read of a directory with its entries instead of an
// error. Models (especially smaller ones) often Read a folder to see what is in
// it and, given only the EISDIR error, retry the same call; listing it lets
// them move on.
func readDirectory(path string) (ReadResult, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return ReadResult{}, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a directory, not a file. It contains %d %s:\n", path, len(names), map[bool]string{true: "entry", false: "entries"}[len(names) == 1])
	for i, n := range names {
		if i == maxDirEntries {
			fmt.Fprintf(&b, "  … and %d more\n", len(names)-maxDirEntries)
			break
		}
		b.WriteString("  " + n + "\n")
	}
	b.WriteString("Read a file inside it to see its contents, or use Glob (for example pattern \"**/*.go\" with this path) to find files in its subfolders.")
	return ReadResult{Type: ReadTypeDirectory, FilePath: path, Text: b.String()}, nil
}

func (r ReadTool) record(path string, data []byte, info os.FileInfo) {
	if r.Tracker != nil {
		r.Tracker.Record(path, data, info)
	}
}

func (r ReadTool) readText(path string, in ReadInput, info os.FileInfo) (ReadResult, error) {
	maxSize := int64(DefaultMaxSizeBytes)
	if r.MaxSizeBytes > 0 {
		maxSize = r.MaxSizeBytes
	}
	if in.AllowLarge {
		maxSize = AllowLargeMaxSizeBytes
	}
	// The whole-file size cap applies only when no limit is given
	//.
	if in.Limit == nil && info.Size() > maxSize {
		return ReadResult{}, fmt.Errorf("File content (%s) exceeds maximum allowed size (%s). Use offset and limit parameters to read specific portions of the file, or search for specific content instead of reading the whole file.", formatBytes(info.Size()), formatBytes(maxSize))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ReadResult{}, err
	}
	r.record(path, raw, info)

	notText := classifyText(raw)
	s := string(raw)
	if notText == notTextUTF8 {
		s = strings.ToValidUTF8(s, "�")
	}
	s = strings.TrimPrefix(s, utf8BOM)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\r")
	lines := strings.Split(s, "\n")
	total := len(lines)

	start := 0
	if in.Offset != nil && *in.Offset > 0 {
		start = *in.Offset - 1
	}
	limit := DefaultLineLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	var sel []string
	if start < total {
		end := total
		if start+limit < end {
			end = start + limit
		}
		sel = lines[start:end]
	}
	truncated := in.Limit == nil && start+len(sel) < total

	startLine := start + 1
	content := strings.Join(sel, "\n")

	// Token budget (readtokens.go).
	tokenBanner := ""
	if !in.AllowLarge && content != "" {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		limit := r.maxReadTokens()
		if tokens := estimateReadTokens(content, ext); tokens > limit {
			if start > 0 || in.Limit != nil {
				return ReadResult{}, readTokensExceeded(tokens, limit)
			}
			page := firstTokenPage(sel, content, tokens, limit)
			tokenBanner = tokenPageBanner(path, page, total, utf8.RuneCountInString(s), tokens, limit, r.searchToolName())
			sel = page.lines
			content = strings.Join(sel, "\n")
			truncated = false
		}
	}
	res := ReadResult{
		Type:       ReadTypeText,
		FilePath:   path,
		Content:    content,
		NumLines:   len(sel),
		StartLine:  startLine,
		TotalLines: total,
		Truncated:  truncated || tokenBanner != "",
	}

	// Render.
	var out string
	switch {
	case content != "":
		out = r.renderNumbered(sel, startLine)
	case res.NumLines >= 1 && total > 1:
		out = strconv.Itoa(startLine) + "\t"
	case res.NumLines >= 1 || total == 0:
		out = msgEmptyFile
	default:
		out = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", startLine, total)
	}
	if truncated {
		last := start + len(sel)
		out = partialViewPrefix + fmt.Sprintf("%s: showing lines %d-%d of %d total (default cap %d lines). Call %s with offset=%d limit=%d for the next page, or Grep to find a specific section. Do NOT answer from this page alone if the answer may be further in the file.]\n", path, startLine, last, total, DefaultLineLimit, ToolNameRead, last+1, DefaultLineLimit) + out
	}
	if tokenBanner != "" {
		out = tokenBanner + "\n" + out
	}
	if notText != notTextNone {
		out += "\n\n" + notTextReadNotice(notText)
	}
	res.Text = out
	return res, nil
}

// renderNumbered renders lines in cat -n style: "<n>\t<line>" with no
// padding, a trailing "\r" stripped from each line.
func (r ReadTool) renderNumbered(lines []string, startLine int) string {
	maxChars := DefaultMaxLineChars
	if r.MaxLineChars > 0 {
		maxChars = r.MaxLineChars
	} else if r.MaxLineChars < 0 {
		maxChars = 0
	}
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		ln = strings.TrimSuffix(ln, "\r")
		if maxChars > 0 && utf8.RuneCountInString(ln) > maxChars {
			rs := []rune(ln)
			ln = string(rs[:maxChars]) + fmt.Sprintf("... [line truncated: %d characters total]", len(rs))
		}
		b.WriteString(strconv.Itoa(startLine + i))
		b.WriteByte('\t')
		b.WriteString(ln)
	}
	return b.String()
}

// --- notebooks --------------------------------------------------------------

// nbInvalidMessage is the error for a malformed notebook.
const nbInvalidMessage = `Notebook file is not a valid Jupyter notebook (top-level "cells" must be an array of cell objects, each with a string or string-array "source").`

type nbFile struct {
	Cells    []nbCell `json:"cells"`
	Metadata struct {
		LanguageInfo struct {
			Name string `json:"name"`
		} `json:"language_info"`
	} `json:"metadata"`
}

type nbCell struct {
	ID       string          `json:"id"`
	CellType string          `json:"cell_type"`
	Source   json.RawMessage `json:"source"`
	Outputs  []nbOutput      `json:"outputs"`
}

type nbOutput struct {
	OutputType string                     `json:"output_type"`
	Text       json.RawMessage            `json:"text"`
	Data       map[string]json.RawMessage `json:"data"`
	Ename      string                     `json:"ename"`
	Evalue     string                     `json:"evalue"`
	Traceback  []string                   `json:"traceback"`
}

// joinSource accepts a string or a string array.
func joinSource(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return strings.Join(arr, ""), true
	}
	return "", false
}

func (r ReadTool) readNotebook(path string, in ReadInput, info os.FileInfo) (ReadResult, error) {
	maxSize := int64(DefaultMaxSizeBytes)
	if r.MaxSizeBytes > 0 {
		maxSize = r.MaxSizeBytes
	}
	if in.AllowLarge {
		maxSize = AllowLargeMaxSizeBytes
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ReadResult{}, err
	}
	var nb nbFile
	if err := json.Unmarshal(raw, &nb); err != nil {
		return ReadResult{}, fmt.Errorf("Notebook file is not valid JSON (it may be truncated, corrupted, or still being written): %v", err)
	}
	if nb.Cells == nil {
		return ReadResult{}, errors.New(nbInvalidMessage)
	}
	lang := nb.Metadata.LanguageInfo.Name
	if lang == "" {
		lang = "python"
	}
	var cells []NotebookCellView
	var parts []string
	var images []ReadImage
	for i, c := range nb.Cells {
		src, ok := joinSource(c.Source)
		if !ok {
			return ReadResult{}, errors.New(nbInvalidMessage)
		}
		id := c.ID
		if id == "" {
			id = "cell-" + strconv.Itoa(i)
		}
		v := NotebookCellView{CellID: id, CellType: c.CellType, Source: src}
		if c.CellType == "code" {
			v.Language = lang
			texts := make([]string, len(c.Outputs))
			imgs := make([]*ReadImage, len(c.Outputs))
			for j, o := range c.Outputs {
				texts[j], imgs[j] = outputText(o), outputImage(o)
			}
			if outputsTooLarge(texts, imgs) {
				v.Outputs = []string{notebookOutputsTooLarge(i)}
			} else {
				for j := range texts {
					if texts[j] != "" {
						v.Outputs = append(v.Outputs, texts[j])
					}
					if imgs[j] != nil {
						images = append(images, *imgs[j])
					}
				}
			}
		}
		cells = append(cells, v)
		parts = append(parts, renderCell(v))
	}
	text := strings.Join(parts, "\n")
	if int64(len(text)) > maxSize {
		return ReadResult{}, fmt.Errorf("Notebook content (%s) exceeds maximum allowed size (%s).", formatBytes(int64(len(text))), formatBytes(maxSize))
	}
	r.record(path, raw, info)
	return ReadResult{Type: ReadTypeNotebook, FilePath: path, Cells: cells, Text: text, Images: images}, nil
}

// maxNotebookOutputChars caps a code cell's combined output text and image
// data; above it the outputs are replaced by a pointer to read them another
// way.
const maxNotebookOutputChars = 10000

// outputsTooLarge reports whether a cell's outputs exceed the cap, counting
// text in UTF-16 code units.
func outputsTooLarge(texts []string, imgs []*ReadImage) bool {
	n := 0
	for j := range texts {
		n += len(utf16.Encode([]rune(texts[j])))
		if imgs[j] != nil {
			n += len(imgs[j].Base64)
		}
		if n > maxNotebookOutputChars {
			return true
		}
	}
	return false
}

// notebookOutputsTooLarge is the stand-in output for an oversized cell: the
// Bash/jq form, or the PowerShell form on Windows.
func notebookOutputsTooLarge(cell int) string {
	how := fmt.Sprintf("Bash with: cat <notebook_path> | jq '.cells[%d].outputs'", cell)
	if runtime.GOOS == "windows" {
		how = fmt.Sprintf("PowerShell with: Get-Content <notebook_path> | ConvertFrom-Json | Select-Object -ExpandProperty cells | Select-Object -Index %d | Select-Object -ExpandProperty outputs", cell)
	}
	return "Outputs are too large to include. Use " + how
}

// outputImage extracts an execute_result/display_data output's inline image:
// the first of image/png, image/jpeg holding a string, with whitespace
// removed, kept only when its decoded bytes carry a recognised image
// signature. The images ride on ReadResult.Images after the text.
func outputImage(o nbOutput) *ReadImage {
	if o.OutputType != "execute_result" && o.OutputType != "display_data" {
		return nil
	}
	for _, mt := range []string{"image/png", "image/jpeg"} {
		raw, ok := o.Data[mt]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		v = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, v)
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil || !hasImageSignature(dec) {
			return nil
		}
		return &ReadImage{Base64: v, MediaType: mt}
	}
	return nil
}

// hasImageSignature reports whether b starts with a PNG, JPEG, GIF or WebP
// signature.
func hasImageSignature(b []byte) bool {
	switch {
	case len(b) < 4:
		return false
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return true
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return true
	case len(b) >= 6 && string(b[:4]) == "GIF8" && (b[4] == '7' || b[4] == '9') && b[5] == 'a':
		return true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return true
	}
	return false
}

// outputText renders one output's text.
func outputText(o nbOutput) string {
	switch o.OutputType {
	case "stream":
		s, _ := joinSource(o.Text)
		return s
	case "execute_result", "display_data":
		s, _ := joinSource(o.Data["text/plain"])
		return s
	case "error":
		return o.Ename + ": " + o.Evalue + "\n" + strings.Join(o.Traceback, "\n")
	}
	return ""
}

// renderCell renders a cell as a <cell id="..."> block followed
// by its outputs, each on a new line.
func renderCell(c NotebookCellView) string {
	var tags string
	if c.CellType != "code" {
		tags += "<cell_type>" + c.CellType + "</cell_type>"
	}
	if c.CellType == "code" && c.Language != "python" {
		tags += "<language>" + c.Language + "</language>"
	}
	s := fmt.Sprintf(`<cell id="%s">%s%s</cell id="%s">`, c.CellID, tags, c.Source, c.CellID)
	for _, o := range c.Outputs {
		s += "\n\n" + o
	}
	return s
}

// blockingDevicePaths are device paths a Read would hang on or stream
// forever from.
var blockingDevicePaths = map[string]bool{
	"/dev/zero": true, "/dev/random": true, "/dev/urandom": true, "/dev/full": true,
	"/dev/stdin": true, "/dev/tty": true, "/dev/console": true, "/dev/stdout": true,
	"/dev/stderr": true, "/dev/fd/0": true, "/dev/fd/1": true, "/dev/fd/2": true,
}

var procInfoFile = regexp.MustCompile(`^/proc/[^/]+/(environ|cmdline|auxv|maps|mem|stat)$`)

// IsBlockingDevicePath reports whether p is one of the blocking device paths,
// a process's standard descriptors under /proc, or the /proc/<pid> files that
// expose a process's environment, memory or command line. Any other non-regular file (a FIFO, a
// socket, another device) is refused after the stat.
func IsBlockingDevicePath(p string) bool {
	if blockingDevicePaths[p] {
		return true
	}
	if strings.HasPrefix(p, "/proc/") && (strings.HasSuffix(p, "/fd/0") || strings.HasSuffix(p, "/fd/1") || strings.HasSuffix(p, "/fd/2")) {
		return true
	}
	return procInfoFile.MatchString(p)
}
