package graphify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ToolName is the model-facing tool's name.
const ToolName = "CodeGraph"

// ToolMaxResultSizeChars is the tool's inline result limit.
const ToolMaxResultSizeChars = 30000

// ToolDescription is the tool's description.
const ToolDescription = `Answers structural questions about the code in the working directory from a graph of its definitions and what calls, imports, inherits from and uses what. Built in memory on first use and kept current as files change.
- overview: main areas and most connected definitions
- query: definitions matching "question" and what connects to them
- explain: what "name" calls and uses, and what calls and uses it
- path: shortest chain between "from" and "to"
Use it to find where to start or what calls something before searching files. An ambiguous name returns ids to pass instead. Edges marked INFERRED were matched by name and can be wrong; read the code before changing it.`

// ToolInputSchema is the tool's JSON input schema.
var ToolInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"operation":{"type":"string","enum":["overview","query","explain","path"]},` +
	`"question":{"type":"string","description":"query: a question or keywords"},` +
	`"name":{"type":"string","description":"explain: a function, Type.method, type or file name, or an id"},` +
	`"from":{"type":"string"},` +
	`"to":{"type":"string"},` +
	`"depth":{"type":"integer","minimum":1,"maximum":4,"description":"query: edges to follow from the matches (default 2)"}` +
	`},"required":["operation"],"additionalProperties":false}`)

// ToolInput is the tool's input.
type ToolInput struct {
	Operation string `json:"operation"`
	Question  string `json:"question,omitempty"`
	Name      string `json:"name,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Depth     int    `json:"depth,omitempty"`
}

// ParseToolInput decodes and validates the tool input.
func ParseToolInput(raw json.RawMessage) (ToolInput, error) {
	var in ToolInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return ToolInput{}, fmt.Errorf("Invalid input: %v", err)
	}
	need := func(field, value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Invalid input: %s needs %q", in.Operation, field)
		}
		return nil
	}
	var err error
	switch in.Operation {
	case "overview":
	case "query":
		err = need("question", in.Question)
	case "explain":
		err = need("name", in.Name)
	case "path":
		if err = need("from", in.From); err == nil {
			err = need("to", in.To)
		}
	default:
		err = errors.New("Invalid input: operation must be one of overview, query, explain, path")
	}
	if err == nil && (in.Depth < 0 || in.Depth > 4) {
		err = errors.New("Invalid input: depth must be between 1 and 4")
	}
	return in, err
}

// ToolUseSummary is the one-line description of a call for the transcript.
func ToolUseSummary(raw json.RawMessage) string {
	var in ToolInput
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	switch in.Operation {
	case "query":
		return in.Question
	case "explain":
		return "explain " + in.Name
	case "path":
		return in.From + " → " + in.To
	}
	return in.Operation
}

// Tool answers graph questions about the tree under Root. The graph lives in
// memory (nothing is written to disk) and is shared by every Tool of the
// process with the same Root.
type Tool struct {
	// Root is the directory the graph covers; "" uses the process working
	// directory.
	Root string
	// Exclude reports absolute paths the session may not read; they are left
	// out of the graph. Nil excludes nothing.
	Exclude func(abs string) bool
}

// fileStamp identifies a file's content cheaply.
type fileStamp struct {
	size  int64
	mtime time.Time
}

// liveGraph is the in-memory graph of one root and what it was built from.
type liveGraph struct {
	mu       sync.Mutex
	files    map[string]*FileExtraction // by relative path
	stamps   map[string]fileStamp
	graph    *Graph
	analysis *Analysis
	detected *Detection
	checked  time.Time
}

var (
	liveMu     sync.Mutex
	liveGraphs = map[string]*liveGraph{}
)

// recheckAfter is how long a graph is used without looking at the tree
// again; calls in quick succession (parallel tool calls) share one check.
const recheckAfter = 2 * time.Second

// current returns the graph of the tree as it is now, re-reading only the
// files that changed since the last call.
func (t Tool) current(ctx context.Context) (*liveGraph, error) {
	root := t.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	liveMu.Lock()
	lg := liveGraphs[root]
	if lg == nil {
		lg = &liveGraph{files: map[string]*FileExtraction{}, stamps: map[string]fileStamp{}}
		liveGraphs[root] = lg
	}
	liveMu.Unlock()

	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.graph != nil && time.Since(lg.checked) < recheckAfter {
		return lg, nil
	}
	var exclude func(rel string) bool
	if t.Exclude != nil {
		exclude = func(rel string) bool { return t.Exclude(filepath.Join(root, filepath.FromSlash(rel))) }
	}
	det, err := Detect(ctx, root, DetectOptions{Exclude: exclude})
	if err != nil {
		return nil, err
	}
	paths := det.Readable()
	stamps := make(map[string]fileStamp, len(paths))
	var changed []string
	for _, rel := range paths {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		st := fileStamp{size: info.Size(), mtime: info.ModTime()}
		stamps[rel] = st
		if old, ok := lg.stamps[rel]; !ok || old != st {
			changed = append(changed, rel)
		}
	}
	dirty := len(changed) > 0 || len(stamps) != len(lg.stamps) || lg.graph == nil
	if dirty {
		fresh, _, err := Extract(ctx, root, changed, ExtractOptions{})
		if err != nil {
			return nil, err
		}
		files := make(map[string]*FileExtraction, len(stamps))
		for rel := range stamps {
			if fe := lg.files[rel]; fe != nil {
				files[rel] = fe
			}
		}
		for _, rel := range changed {
			delete(files, rel) // unreadable now: drop the old extraction
		}
		for _, fe := range fresh {
			files[fe.Path] = fe
		}
		ordered := make([]*FileExtraction, 0, len(files))
		for _, rel := range paths {
			if fe := files[rel]; fe != nil {
				ordered = append(ordered, fe)
			}
		}
		nodes, edges := Resolve(root, ordered)
		g := Build(ordered, nodes, edges)
		a := Analyze(g, Cluster(g, 0))
		g.Communities = a.Communities
		lg.files, lg.stamps, lg.graph, lg.analysis = files, stamps, g, a
	}
	lg.detected, lg.checked = det, time.Now()
	return lg, nil
}

// Execute runs one operation and returns its text. A name that matches
// nothing, or several things, is an ordinary answer (it tells the model what
// to try), not an error.
func (t Tool) Execute(ctx context.Context, in ToolInput) (string, error) {
	lg, err := t.current(ctx)
	if err != nil {
		return "", err
	}
	g := lg.graph
	if len(g.Nodes) == 0 {
		return "The working directory has no source or Markdown files in a supported language (" + strings.Join(Languages(), ", ") + "), so there is no graph.", nil
	}
	var out string
	switch in.Operation {
	case "overview":
		out = overview(lg)
	case "query":
		out, _ = g.Query(in.Question, in.Depth, 1500)
	case "explain":
		out, _ = g.Explain(in.Name)
	case "path":
		out, _ = g.Path(in.From, in.To)
	}
	return strings.TrimRight(out, "\n"), nil
}

func overview(lg *liveGraph) string {
	g, a, d := lg.graph, lg.analysis, lg.detected
	var b strings.Builder
	fmt.Fprintf(&b, "%d code files and %d documents; %d nodes, %d edges, %d communities.\n",
		len(d.Files[CatCode]), len(d.Files[CatDocument]), len(g.Nodes), len(g.Edges), len(a.Communities))
	b.WriteString("\nMost connected definitions:\n")
	for _, n := range a.GodNodes {
		fmt.Fprintf(&b, "  %s — %d edges (%s)\n", n.Label, n.Degree, n.SourceFile)
	}
	b.WriteString("\nMain areas (community: size, most connected members):\n")
	for _, c := range a.Communities[:min(15, len(a.Communities))] {
		fmt.Fprintf(&b, "  %s: %d nodes — %s\n", c.Label, c.Size, strings.Join(c.Top[:min(4, len(c.Top))], ", "))
	}
	if rest := len(a.Communities) - 15; rest > 0 {
		fmt.Fprintf(&b, "  … and %d smaller ones\n", rest)
	}
	if len(a.Cycles) > 0 {
		fmt.Fprintf(&b, "\nImport cycles: %d (first: %s)\n", len(a.Cycles), strings.Join(a.Cycles[0][:min(5, len(a.Cycles[0]))], " ↔ "))
	}
	b.WriteString("\nNext: query for a topic, explain for one definition, path between two.")
	return b.String()
}
