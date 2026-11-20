// Package graphify turns a source tree into a queryable knowledge graph.
//
// It is a native Go implementation of the pipeline the graphify project
// (https://github.com/Graphify-Labs/graphify) describes:
//
//	detect -> extract -> build -> cluster -> analyze -> report -> export
//
// and it writes the same output layout (graphify-out/graph.json in NetworkX
// node-link form, GRAPH_REPORT.md, graph.html, cache/), so a graph built here
// can be read by tools that understand that format.
//
// Everything runs locally and without a model: Go files are read with go/ast,
// the other supported languages with line-oriented pattern extractors, and
// Markdown by its headings and links. There is no pass over PDFs, images,
// audio or video; Detect counts those files and the report lists them as
// skipped.
package graphify

import (
	"path"
	"strconv"
)

// SchemaVersion is written to graph.json as schema_version.
const SchemaVersion = 1

// extractorVersion is part of every cache key; bump it when an extractor's
// output changes so stale cache entries are not reused.
const extractorVersion = "5"

// Confidence values of an edge.
const (
	Extracted = "EXTRACTED" // stated in the source
	Inferred  = "INFERRED"  // resolved by name; ConfidenceScore says how surely
)

// Node kinds (the "kind" attribute).
const (
	KindFile     = "file"
	KindPackage  = "package"
	KindFunction = "function"
	KindMethod   = "method"
	KindType     = "type"
	KindSection  = "section"
)

// Node is one graph node.
type Node struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	FileType       string `json:"file_type"` // "code" or "document"
	SourceFile     string `json:"source_file"`
	SourceLocation string `json:"source_location,omitempty"` // "L42"
	Kind           string `json:"kind,omitempty"`
	Community      int    `json:"community"`
}

// Edge is one graph edge. The graph is treated as undirected for clustering
// and traversal, but Source -> Target is the direction of the relation.
type Edge struct {
	Source          string  `json:"source"`
	Target          string  `json:"target"`
	Relation        string  `json:"relation"`
	Confidence      string  `json:"confidence"`
	ConfidenceScore float64 `json:"confidence_score,omitempty"` // INFERRED only
	SourceFile      string  `json:"source_file,omitempty"`
	SourceLocation  string  `json:"source_location,omitempty"`
	// TypeOnly marks an import that exists only for type checking (TypeScript
	// "import type", Python "if TYPE_CHECKING:"); it creates no runtime cycle.
	TypeOnly bool `json:"type_only,omitempty"`
}

// Def is a definition a file contributes to name resolution.
type Def struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Recv string `json:"recv,omitempty"` // owning type of a method
	Kind string `json:"kind"`           // KindFunction, KindMethod or KindType
	// Type facts, as written in the defining file ("T" or "pkg.T"): what a
	// function returns first, the types of a type's fields, and the types it
	// embeds (whose methods it has).
	Result string            `json:"result,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
	Embeds []string          `json:"embeds,omitempty"`
	// Iface marks an interface type: its methods are implemented elsewhere.
	Iface bool `json:"iface,omitempty"`
}

// Ref is a reference that is resolved against the definitions of the whole
// tree after every file was extracted.
type Ref struct {
	From     string `json:"from"`
	Name     string `json:"name"`
	Qual     string `json:"qual,omitempty"` // "" unqualified, "?" unknown expression
	Recv     string `json:"recv,omitempty"` // enclosing type, when the call is on it
	Relation string `json:"relation"`
	Line     int    `json:"line,omitempty"`
	// What is known about the type of the value a method is called on, as
	// written in this file: it has type TQual.TName; or, with TCall, it is
	// what the function TQual.TName returns; with TField, it is that field
	// of such a value.
	TQual  string `json:"tqual,omitempty"`
	TName  string `json:"tname,omitempty"`
	TField string `json:"tfield,omitempty"`
	TCall  bool   `json:"tcall,omitempty"`
}

// Import is one import statement of a file.
type Import struct {
	Path  string `json:"path"`
	Alias string `json:"alias,omitempty"`
	Line  int    `json:"line,omitempty"`
	// Names are the names the import binds in the file, and Origs (same
	// length) what each is called where it comes from ("" for the module
	// itself: "import x", "import * as x").
	Names    []string `json:"names,omitempty"`
	Origs    []string `json:"origs,omitempty"`
	TypeOnly bool     `json:"type_only,omitempty"`
}

// FileExtraction is what one file yields; it is the unit of the cache.
type FileExtraction struct {
	Path    string   `json:"path"` // "/"-separated, relative to the scan root
	Lang    string   `json:"lang"`
	Family  string   `json:"family,omitempty"`  // languages that share a namespace
	Package string   `json:"package,omitempty"` // Go package name
	Words   int      `json:"words,omitempty"`   // documents only
	Nodes   []Node   `json:"nodes"`
	Edges   []Edge   `json:"edges,omitempty"`
	Defs    []Def    `json:"defs,omitempty"`
	Refs    []Ref    `json:"refs,omitempty"`
	Imports []Import `json:"imports,omitempty"`
	Err     string   `json:"err,omitempty"`

	seenID  map[string]bool
	seenRef map[string]bool
}

func loc(line int) string {
	if line <= 0 {
		return ""
	}
	return "L" + strconv.Itoa(line)
}

func newFileExtraction(rel, lang, family, fileType string) *FileExtraction {
	fe := &FileExtraction{Path: rel, Lang: lang, Family: family, seenID: map[string]bool{rel: true}, seenRef: map[string]bool{}}
	fe.Nodes = append(fe.Nodes, Node{ID: rel, Label: path.Base(rel), FileType: fileType, SourceFile: rel, Kind: KindFile})
	return fe
}

// addDef adds a definition node, its "contains" edge from the file and its
// entry in Defs, and returns the node id. A second definition with the same
// qualified name in the file gets its line appended to stay distinct.
func (fe *FileExtraction) addDef(kind, name, recv string, line int) string {
	qual, label := name, name
	switch kind {
	case KindFunction:
		label = name + "()"
	case KindMethod:
		qual = recv + "." + name
		label = qual + "()"
	}
	id := fe.Path + "#" + qual
	if fe.seenID[id] {
		id += "@" + loc(line)
		if fe.seenID[id] {
			return id
		}
	}
	fe.seenID[id] = true
	fe.Nodes = append(fe.Nodes, Node{ID: id, Label: label, FileType: "code", SourceFile: fe.Path, SourceLocation: loc(line), Kind: kind})
	fe.Edges = append(fe.Edges, Edge{Source: fe.Path, Target: id, Relation: "contains", Confidence: Extracted, SourceFile: fe.Path, SourceLocation: loc(line)})
	fe.Defs = append(fe.Defs, Def{ID: id, Name: name, Recv: recv, Kind: kind})
	return id
}

// addRef records a reference once per (from, qualifier, name, relation).
func (fe *FileExtraction) addRef(r Ref) {
	if r.Name == "" || r.From == "" {
		return
	}
	key := r.From + "\x00" + r.Qual + "\x00" + r.Name + "\x00" + r.Relation + "\x00" + r.Recv + "\x00" + r.TQual + "\x00" + r.TName + "\x00" + r.TField
	if fe.seenRef[key] {
		return
	}
	fe.seenRef[key] = true
	fe.Refs = append(fe.Refs, r)
}
