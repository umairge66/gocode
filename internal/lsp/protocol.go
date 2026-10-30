package lsp

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Position is a zero-based LSP position (character in UTF-16 code units).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range inside a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier names a document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// TextDocumentItem is the didOpen document payload.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// VersionedTextDocumentIdentifier names a document at a version.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// ContentChange is one textDocument/didChange change: a whole-document
// replacement.
type ContentChange struct {
	Text string `json:"text"`
}

// InitializeParams is the client's initialize request body.
type InitializeParams struct {
	ProcessID             int               `json:"processId"`
	ClientInfo            ClientInfo        `json:"clientInfo"`
	InitializationOptions json.RawMessage   `json:"initializationOptions"`
	WorkspaceFolders      []WorkspaceFolder `json:"workspaceFolders"`
	RootPath              string            `json:"rootPath"`
	RootURI               string            `json:"rootUri"`
	Capabilities          json.RawMessage   `json:"capabilities"`
}

// ClientInfo identifies the client to the server.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// WorkspaceFolder is one workspace root.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// clientCapabilities is the capability set gocode advertises. The only
// variable part is workspace.configuration, which is advertised exactly when
// the server has settings to answer workspace/configuration with.
func clientCapabilities(configuration bool) json.RawMessage {
	conf := "false"
	if configuration {
		conf = "true"
	}
	return json.RawMessage(`{` +
		`"workspace":{"configuration":` + conf + `,"workspaceFolders":false,"didChangeWatchedFiles":{"dynamicRegistration":false}},` +
		`"textDocument":{` +
		`"synchronization":{"dynamicRegistration":false,"willSave":false,"willSaveWaitUntil":false,"didSave":true},` +
		`"publishDiagnostics":{"relatedInformation":true,"tagSupport":{"valueSet":[1,2]},"versionSupport":false,"codeDescriptionSupport":true,"dataSupport":false},` +
		`"hover":{"dynamicRegistration":false,"contentFormat":["markdown","plaintext"]},` +
		`"definition":{"dynamicRegistration":false,"linkSupport":true},` +
		`"references":{"dynamicRegistration":false},` +
		`"documentSymbol":{"dynamicRegistration":false,"hierarchicalDocumentSymbolSupport":true},` +
		`"callHierarchy":{"dynamicRegistration":false}},` +
		`"general":{"positionEncodings":["utf-16"]}}`)
}

// NewInitializeParams builds the initialize body for a workspace root.
// initOptions defaults to an empty object; configuration says whether the
// server has settings (see clientCapabilities).
func NewInitializeParams(root, clientVersion string, initOptions json.RawMessage, configuration bool) InitializeParams {
	uri := PathToURI(root)
	if len(initOptions) == 0 || string(initOptions) == "null" {
		initOptions = json.RawMessage(`{}`)
	}
	return InitializeParams{
		ProcessID:             os.Getpid(),
		ClientInfo:            ClientInfo{Name: "gocode", Version: clientVersion},
		InitializationOptions: initOptions,
		WorkspaceFolders:      []WorkspaceFolder{{URI: uri, Name: filepath.Base(root)}},
		RootPath:              root,
		RootURI:               uri,
		Capabilities:          clientCapabilities(configuration),
	}
}

// PathToURI converts a filesystem path to a file:// URI (absolute, with the
// path percent-encoded).
func PathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

// URIToPath converts a file:// URI back to a filesystem path. A URI with
// another scheme is returned unchanged.
func URIToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}
