package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
)

// ToolName is the model-facing tool's name.
const ToolName = "LSP"

// ToolSearchHint is the keyword hint for deferred tool search.
const ToolSearchHint = "code intelligence (definitions, references, symbols, hover)"

// MaxResultSizeChars is the tool's inline result limit.
const MaxResultSizeChars = 100000

// maxAnalyzedFileSize is the largest file the tool opens on a server.
const maxAnalyzedFileSize = 10_000_000

// ToolDescription is the tool's description.
const ToolDescription = `Interact with Language Server Protocol (LSP) servers to get code intelligence features.

Supported operations:
- goToDefinition: Find where a symbol is defined
- findReferences: Find all references to a symbol
- hover: Get hover information (documentation, type info) for a symbol
- documentSymbol: Get all symbols (functions, classes, variables) in a document
- workspaceSymbol: Search for symbols matching a query across the entire workspace
- goToImplementation: Find implementations of an interface or abstract method
- prepareCallHierarchy: Get call hierarchy item at a position (functions/methods)
- incomingCalls: Find all functions/methods that call the function at a position
- outgoingCalls: Find all functions/methods called by the function at a position

All operations require:
- filePath: The file to operate on
- line: The line number (1-based, as shown in editors)
- character: The character offset (1-based, as shown in editors)

The workspaceSymbol operation also takes:
- query: The symbol name or partial name to search for. Always provide it ` + "—" + ` most language servers return no results for an empty query.

Note: LSP servers must be configured for the file type. If no server is available, an error will be returned.`

// Operations are the supported operations, in schema order.
var Operations = []string{
	"goToDefinition", "findReferences", "hover", "documentSymbol", "workspaceSymbol",
	"goToImplementation", "prepareCallHierarchy", "incomingCalls", "outgoingCalls",
}

// InputSchema is the tool's JSON input schema.
var InputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"operation":{"type":"string","enum":["goToDefinition","findReferences","hover","documentSymbol","workspaceSymbol","goToImplementation","prepareCallHierarchy","incomingCalls","outgoingCalls"],"description":"The LSP operation to perform"},` +
	`"filePath":{"type":"string","description":"The absolute or relative path to the file"},` +
	`"line":{"type":"integer","exclusiveMinimum":0,"description":"The line number (1-based, as shown in editors)"},` +
	`"character":{"type":"integer","exclusiveMinimum":0,"description":"The character offset (1-based, as shown in editors)"},` +
	`"query":{"type":"string","description":"The symbol name or partial name to search for (workspaceSymbol only). Most language servers return no results for an empty query, so always provide it when using workspaceSymbol."}` +
	`},"required":["operation","filePath","line","character"],"additionalProperties":false}`)

// Input is the tool's input.
type Input struct {
	Operation string  `json:"operation"`
	FilePath  string  `json:"filePath"`
	Line      float64 `json:"line"`
	Character float64 `json:"character"`
	Query     *string `json:"query,omitempty"`
}

// ParseInput decodes and validates the tool input (an input error is
// reported to the model as an invalid-input error).
func ParseInput(raw json.RawMessage) (Input, error) {
	var in Input
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Input{}, fmt.Errorf("Invalid input: %v", err)
	}
	valid := false
	for _, op := range Operations {
		if op == in.Operation {
			valid = true
		}
	}
	if !valid {
		return Input{}, fmt.Errorf("Invalid input: operation must be one of %s", strings.Join(Operations, ", "))
	}
	if in.FilePath == "" {
		return Input{}, errors.New("Invalid input: filePath is required")
	}
	for name, v := range map[string]float64{"line": in.Line, "character": in.Character} {
		if v <= 0 || v != math.Trunc(v) || v > math.MaxInt32 {
			return Input{}, fmt.Errorf("Invalid input: %s must be a positive integer", name)
		}
	}
	return in, nil
}

// Tool runs LSP operations against a Manager.
type Tool struct {
	Manager *Manager
	// Cwd resolves relative paths and shortens result paths; "" uses the
	// process working directory.
	Cwd string
}

func (t Tool) cwd() string {
	if t.Cwd != "" {
		return t.Cwd
	}
	wd, _ := os.Getwd()
	return wd
}

// ResolvePath makes a tool path absolute against the working directory.
func ResolvePath(p, cwd string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// Validate checks that the target is an existing regular file.
func (t Tool) Validate(in Input) error {
	abs := ResolvePath(in.FilePath, t.cwd())
	fi, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("File does not exist: %s", in.FilePath)
		}
		return fmt.Errorf("Cannot access file: %s. %v", in.FilePath, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("Path is not a file: %s", in.FilePath)
	}
	return nil
}

// Execute runs one operation and returns the text for the model. Server
// failures are reported in the text; only invalid input is an error.
func (t Tool) Execute(ctx context.Context, in Input) (string, error) {
	if err := t.Validate(in); err != nil {
		return "", err
	}
	cwd := t.cwd()
	abs := ResolvePath(in.FilePath, cwd)
	if t.Manager == nil {
		return "LSP server manager not initialized. This may indicate a startup issue.", nil
	}
	out, err := t.run(ctx, in, abs, cwd)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return fmt.Sprintf("Error performing %s: %v", in.Operation, err), nil
	}
	return out, nil
}

func (t Tool) run(ctx context.Context, in Input, abs, cwd string) (string, error) {
	m := t.Manager
	if m.ServerFor(abs) == nil {
		return "No LSP server available for file type: " + filepath.Ext(abs), nil
	}
	if !m.IsFileOpen(abs) {
		if fi, err := os.Stat(abs); err == nil && fi.Size() > maxAnalyzedFileSize {
			return fmt.Sprintf("File too large for LSP analysis (%dMB exceeds 10MB limit)", int(math.Ceil(float64(fi.Size())/1e6))), nil
		}
		text, err := readFileLimited(abs, maxAnalyzedFileSize)
		if err != nil {
			return "", err
		}
		if err := m.OpenFile(ctx, abs, text); err != nil {
			return "", err
		}
	}
	method, params := requestFor(in, abs)
	res, found, err := m.Request(ctx, abs, method, params)
	if err != nil {
		return "", err
	}
	if !found {
		return "No LSP server available for file type: " + filepath.Ext(abs), nil
	}
	switch in.Operation {
	case "goToDefinition", "goToImplementation":
		return FormatDefinition(filterIgnored(ctx, decodeLocations(res), cwd), cwd), nil
	case "findReferences":
		return FormatReferences(filterIgnored(ctx, decodeLocations(res), cwd), cwd), nil
	case "hover":
		return FormatHover(res), nil
	case "documentSymbol":
		return FormatDocumentSymbols(decodeSymbols(res), cwd), nil
	case "workspaceSymbol":
		return FormatWorkspaceSymbols(filterIgnoredSymbols(ctx, decodeSymbols(res), cwd), cwd), nil
	case "prepareCallHierarchy":
		var items []callItem
		_ = json.Unmarshal(nonNull(res), &items)
		return FormatCallItems(items, cwd), nil
	case "incomingCalls", "outgoingCalls":
		var items []json.RawMessage
		_ = json.Unmarshal(nonNull(res), &items)
		if len(items) == 0 {
			return noCallItem, nil
		}
		method := "callHierarchy/incomingCalls"
		if in.Operation == "outgoingCalls" {
			method = "callHierarchy/outgoingCalls"
		}
		res, _, err := m.Request(ctx, abs, method, map[string]json.RawMessage{"item": items[0]})
		if err != nil {
			return "", err
		}
		var edges []callEdge
		_ = json.Unmarshal(nonNull(res), &edges)
		return FormatCalls(edges, in.Operation == "incomingCalls", cwd), nil
	}
	return "", fmt.Errorf("unsupported operation %q", in.Operation)
}

func nonNull(raw json.RawMessage) json.RawMessage {
	if isNull(trimJSON(raw)) {
		return json.RawMessage("[]")
	}
	return raw
}

// requestFor maps an operation to its LSP method and params. Positions are
// converted from the tool's 1-based line/character to LSP's 0-based ones.
func requestFor(in Input, abs string) (string, any) {
	doc := TextDocumentIdentifier{URI: PathToURI(abs)}
	pos := Position{Line: int(in.Line) - 1, Character: int(in.Character) - 1}
	at := map[string]any{"textDocument": doc, "position": pos}
	switch in.Operation {
	case "goToDefinition":
		return "textDocument/definition", at
	case "findReferences":
		at["context"] = map[string]bool{"includeDeclaration": true}
		return "textDocument/references", at
	case "hover":
		return "textDocument/hover", at
	case "documentSymbol":
		return "textDocument/documentSymbol", map[string]any{"textDocument": doc}
	case "workspaceSymbol":
		q := ""
		if in.Query != nil {
			q = *in.Query
		}
		return "workspace/symbol", map[string]string{"query": q}
	case "goToImplementation":
		return "textDocument/implementation", at
	default: // prepareCallHierarchy, incomingCalls, outgoingCalls
		return "textDocument/prepareCallHierarchy", at
	}
}

func readFileLimited(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// gitCheckIgnoreBatch and gitCheckIgnoreTimeout bound the ignore check.
const (
	gitCheckIgnoreBatch   = 50
	gitCheckIgnoreTimeout = 5 * time.Second
)

// ignoredPaths returns which of paths git ignores in the repository at cwd
// (none when cwd is not a repository or git is unavailable).
func ignoredPaths(ctx context.Context, paths []string, cwd string) map[string]bool {
	ignored := map[string]bool{}
	for i := 0; i < len(paths); i += gitCheckIgnoreBatch {
		end := min(i+gitCheckIgnoreBatch, len(paths))
		cctx, cancel := context.WithTimeout(ctx, gitCheckIgnoreTimeout)
		cmd := exec.CommandContext(cctx, "git", append([]string{"check-ignore", "--"}, paths[i:end]...)...)
		cmd.Dir = cwd
		cmd.Env = envsec.ChildEnv(nil)
		out, err := cmd.Output()
		cancel()
		if err != nil && len(out) == 0 {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if l := strings.TrimSpace(line); l != "" {
				ignored[l] = true
			}
		}
	}
	return ignored
}

// filterIgnored drops locations in git-ignored files (build output,
// vendored dependencies).
func filterIgnored(ctx context.Context, locs []Location, cwd string) []Location {
	if len(locs) == 0 {
		return locs
	}
	paths, byURI := uriPaths(func(yield func(string)) {
		for _, l := range locs {
			yield(l.URI)
		}
	})
	ignored := ignoredPaths(ctx, paths, cwd)
	if len(ignored) == 0 {
		return locs
	}
	out := locs[:0:0]
	for _, l := range locs {
		if !ignored[byURI[l.URI]] {
			out = append(out, l)
		}
	}
	return out
}

func filterIgnoredSymbols(ctx context.Context, syms []wireSymbol, cwd string) []wireSymbol {
	if len(syms) == 0 {
		return syms
	}
	paths, byURI := uriPaths(func(yield func(string)) {
		for _, s := range syms {
			if u := symbolURI(s); u != "" {
				yield(u)
			}
		}
	})
	ignored := ignoredPaths(ctx, paths, cwd)
	if len(ignored) == 0 {
		return syms
	}
	out := syms[:0:0]
	for _, s := range syms {
		if u := symbolURI(s); u == "" || !ignored[byURI[u]] {
			out = append(out, s)
		}
	}
	return out
}

// uriPaths maps each distinct URI to its decoded path.
func uriPaths(each func(func(string))) ([]string, map[string]string) {
	byURI := map[string]string{}
	var paths []string
	each(func(u string) {
		if _, ok := byURI[u]; ok {
			return
		}
		p := strings.TrimPrefix(u, "file://")
		if driveLetterRe.MatchString(p) {
			p = p[1:]
		}
		if dec, err := url.PathUnescape(p); err == nil {
			p = dec
		}
		byURI[u] = p
		paths = append(paths, p)
	})
	return paths, byURI
}

// symbolAt matches identifier-like and operator tokens on a line.
var symbolAt = regexp.MustCompile(`[\w$'!]+|[+\-*/%&|^~<>=]+`)

// symbolAtPosition returns the token at a 0-based line/character of a file
// (read up to 64 KiB), cut to 30 characters, or "".
func symbolAtPosition(path string, line, char int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 65536)
	n, _ := io.ReadFull(f, buf)
	lines := strings.Split(string(buf[:n]), "\n")
	if line < 0 || line >= len(lines) {
		return ""
	}
	if n == len(buf) && line == len(lines)-1 {
		return ""
	}
	text := lines[line]
	if char < 0 || char >= len(text) {
		return ""
	}
	for _, loc := range symbolAt.FindAllStringIndex(text, -1) {
		if char >= loc[0] && char < loc[1] {
			tok := text[loc[0]:loc[1]]
			if r := []rune(tok); len(r) > 30 {
				tok = string(r[:29]) + "…"
			}
			return tok
		}
	}
	return ""
}

// ToolUseSummary renders the call for the transcript header, e.g.
// `operation: "hover", symbol: "Run", in: "cmd/main.go"`.
func ToolUseSummary(raw json.RawMessage, cwd string) string {
	var in Input
	if json.Unmarshal(raw, &in) != nil || in.Operation == "" {
		return ""
	}
	parts := []string{fmt.Sprintf("operation: %q", in.Operation)}
	shown := in.FilePath
	if shown != "" && cwd != "" {
		shown = DisplayPath(ResolvePath(in.FilePath, cwd), cwd)
	}
	switch in.Operation {
	case "goToDefinition", "findReferences", "hover", "goToImplementation":
		if in.FilePath != "" && in.Line > 0 && in.Character > 0 {
			if sym := symbolAtPosition(ResolvePath(in.FilePath, cwd), int(in.Line)-1, int(in.Character)-1); sym != "" {
				parts = append(parts, fmt.Sprintf("symbol: %q", sym), fmt.Sprintf("in: %q", shown))
			} else {
				parts = append(parts, fmt.Sprintf("file: %q", shown), fmt.Sprintf("position: %d:%d", int(in.Line), int(in.Character)))
			}
			return strings.Join(parts, ", ")
		}
	}
	if in.FilePath != "" {
		parts = append(parts, fmt.Sprintf("file: %q", shown))
	}
	return strings.Join(parts, ", ")
}
