package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// Result texts for empty answers.
const (
	noDefinition     = "No definition found. This may occur if the cursor is not on a symbol, or if the definition is in an external library not indexed by the LSP server."
	noReferences     = "No references found. This may occur if the symbol has no usages, or if the LSP server has not fully indexed the workspace."
	noHover          = "No hover information available. This may occur if the cursor is not on a symbol, or if the LSP server has not fully indexed the file."
	noDocSymbols     = "No symbols found in document. This may occur if the file is empty, not supported by the LSP server, or if the server has not fully indexed the file."
	noWsSymbols      = "No symbols found in workspace. This may occur if the workspace is empty, or if the LSP server has not finished indexing the project."
	noCallItem       = "No call hierarchy item found at this position"
	noIncomingCalls  = "No incoming calls found (nothing calls this function)"
	noOutgoingCalls  = "No outgoing calls found (this function calls nothing)"
	unknownLocation  = "<unknown location>"
	driveLetterSlash = `^/[A-Za-z]:`
)

var driveLetterRe = regexp.MustCompile(driveLetterSlash)

// DisplayURI renders a document URI (or path) for a result: the file path,
// relative to cwd when that is shorter and does not climb more than one
// directory, with forward slashes.
func DisplayURI(uri, cwd string) string {
	if uri == "" {
		return unknownLocation
	}
	p := strings.TrimPrefix(uri, "file://")
	if driveLetterRe.MatchString(p) {
		p = p[1:]
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	return DisplayPath(p, cwd)
}

// DisplayPath renders a file path relative to cwd when that is shorter and
// does not climb more than one directory, with forward slashes.
func DisplayPath(p, cwd string) string {
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, p); err == nil {
			rel = strings.ReplaceAll(rel, "\\", "/")
			if len(rel) < len(p) && !strings.HasPrefix(rel, "../../") {
				return rel
			}
		}
	}
	return strings.ReplaceAll(p, "\\", "/")
}

// symbolKinds names the LSP SymbolKind values.
var symbolKinds = map[int]string{
	1: "File", 2: "Module", 3: "Namespace", 4: "Package", 5: "Class", 6: "Method",
	7: "Property", 8: "Field", 9: "Constructor", 10: "Enum", 11: "Interface",
	12: "Function", 13: "Variable", 14: "Constant", 15: "String", 16: "Number",
	17: "Boolean", 18: "Array", 19: "Object", 20: "Key", 21: "Null",
	22: "EnumMember", 23: "Struct", 24: "Event", 25: "Operator", 26: "TypeParameter",
}

func symbolKind(k int) string {
	if s, ok := symbolKinds[k]; ok {
		return s
	}
	return "Unknown"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// wireLocation decodes a Location or a LocationLink (whose target selection
// range, else target range, is used).
type wireLocation struct {
	URI                  string `json:"uri"`
	Range                *Range `json:"range"`
	TargetURI            string `json:"targetUri"`
	TargetRange          *Range `json:"targetRange"`
	TargetSelectionRange *Range `json:"targetSelectionRange"`
}

func (w wireLocation) location() (Location, bool) {
	if w.TargetURI != "" {
		r := w.TargetSelectionRange
		if r == nil {
			r = w.TargetRange
		}
		if r == nil {
			r = &Range{}
		}
		return Location{URI: w.TargetURI, Range: *r}, true
	}
	if w.URI == "" {
		return Location{}, false
	}
	if w.Range == nil {
		return Location{URI: w.URI}, true
	}
	return Location{URI: w.URI, Range: *w.Range}, true
}

// decodeLocations decodes a Location | Location[] | LocationLink[] | null
// result into its valid locations.
func decodeLocations(raw json.RawMessage) []Location {
	raw = trimJSON(raw)
	if isNull(raw) {
		return nil
	}
	var list []wireLocation
	if raw[0] == '[' {
		if json.Unmarshal(raw, &list) != nil {
			return nil
		}
	} else {
		var one wireLocation
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []wireLocation{one}
	}
	out := make([]Location, 0, len(list))
	for _, w := range list {
		if loc, ok := w.location(); ok {
			out = append(out, loc)
		}
	}
	return out
}

func trimJSON(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func formatLocation(loc Location, cwd string) string {
	return fmt.Sprintf("%s:%d:%d", DisplayURI(loc.URI, cwd), loc.Range.Start.Line+1, loc.Range.Start.Character+1)
}

// groupByFile groups items by their display path, keeping first-seen order.
func groupByFile[T any](items []T, uri func(T) string, cwd string) ([]string, map[string][]T) {
	var order []string
	groups := map[string][]T{}
	for _, it := range items {
		p := DisplayURI(uri(it), cwd)
		if _, ok := groups[p]; !ok {
			order = append(order, p)
		}
		groups[p] = append(groups[p], it)
	}
	return order, groups
}

func distinctURIs(locs []Location) int {
	seen := map[string]bool{}
	for _, l := range locs {
		seen[l.URI] = true
	}
	return len(seen)
}

// FormatDefinition renders a goToDefinition / goToImplementation result.
func FormatDefinition(locs []Location, cwd string) string {
	switch len(locs) {
	case 0:
		return noDefinition
	case 1:
		return "Defined in " + formatLocation(locs[0], cwd)
	}
	lines := make([]string, 0, len(locs))
	for _, l := range locs {
		lines = append(lines, "  "+formatLocation(l, cwd))
	}
	return fmt.Sprintf("Found %d definitions:\n%s", len(locs), strings.Join(lines, "\n"))
}

// FormatReferences renders a findReferences result grouped by file.
func FormatReferences(locs []Location, cwd string) string {
	switch len(locs) {
	case 0:
		return noReferences
	case 1:
		return "Found 1 reference:\n  " + formatLocation(locs[0], cwd)
	}
	order, groups := groupByFile(locs, func(l Location) string { return l.URI }, cwd)
	lines := []string{fmt.Sprintf("Found %d references across %d files:", len(locs), len(order))}
	for _, file := range order {
		lines = append(lines, "\n"+file+":")
		for _, l := range groups[file] {
			lines = append(lines, fmt.Sprintf("  Line %d:%d", l.Range.Start.Line+1, l.Range.Start.Character+1))
		}
	}
	return strings.Join(lines, "\n")
}

// FormatHover renders a hover result.
func FormatHover(raw json.RawMessage) string {
	raw = trimJSON(raw)
	if isNull(raw) {
		return noHover
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
		Range    *Range          `json:"range"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return noHover
	}
	text := hoverText(h.Contents)
	if h.Range != nil {
		return fmt.Sprintf("Hover info at %d:%d:\n\n%s", h.Range.Start.Line+1, h.Range.Start.Character+1, text)
	}
	return text
}

// hoverText flattens MarkupContent, a MarkedString, or an array of them.
func hoverText(raw json.RawMessage) string {
	raw = trimJSON(raw)
	if isNull(raw) {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return ""
		}
		parts := make([]string, 0, len(items))
		for _, it := range items {
			parts = append(parts, hoverText(it))
		}
		return strings.Join(parts, "\n\n")
	}
	var obj struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Value
}

// wireSymbol decodes a DocumentSymbol, a SymbolInformation or a
// WorkspaceSymbol.
type wireSymbol struct {
	Name          string        `json:"name"`
	Kind          int           `json:"kind"`
	Detail        string        `json:"detail"`
	Range         *Range        `json:"range"`
	Children      []wireSymbol  `json:"children"`
	ContainerName string        `json:"containerName"`
	Location      *wireLocation `json:"location"`
}

func decodeSymbols(raw json.RawMessage) []wireSymbol {
	raw = trimJSON(raw)
	if isNull(raw) {
		return nil
	}
	var out []wireSymbol
	_ = json.Unmarshal(raw, &out)
	return out
}

func symbolLines(s wireSymbol, depth int) []string {
	line := strings.Repeat("  ", depth) + s.Name + " (" + symbolKind(s.Kind) + ")"
	if s.Detail != "" {
		line += " " + s.Detail
	}
	start := 0
	if s.Range != nil {
		start = s.Range.Start.Line
	}
	line += fmt.Sprintf(" - Line %d", start+1)
	out := []string{line}
	for _, c := range s.Children {
		out = append(out, symbolLines(c, depth+1)...)
	}
	return out
}

func countSymbols(syms []wireSymbol) int {
	n := len(syms)
	for _, s := range syms {
		n += countSymbols(s.Children)
	}
	return n
}

// FormatDocumentSymbols renders a documentSymbol result as an indented tree
// (or, for flat SymbolInformation answers, grouped by file).
func FormatDocumentSymbols(syms []wireSymbol, cwd string) string {
	if len(syms) == 0 {
		return noDocSymbols
	}
	if syms[0].Location != nil {
		return FormatWorkspaceSymbols(syms, cwd)
	}
	lines := []string{"Document symbols:"}
	for _, s := range syms {
		lines = append(lines, symbolLines(s, 0)...)
	}
	return strings.Join(lines, "\n")
}

func symbolURI(s wireSymbol) string {
	if s.Location == nil {
		return ""
	}
	return s.Location.URI
}

// FormatWorkspaceSymbols renders a workspaceSymbol result grouped by file.
func FormatWorkspaceSymbols(syms []wireSymbol, cwd string) string {
	var valid []wireSymbol
	for _, s := range syms {
		if symbolURI(s) != "" {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		return noWsSymbols
	}
	lines := []string{fmt.Sprintf("Found %d %s in workspace:", len(valid), plural(len(valid), "symbol"))}
	order, groups := groupByFile(valid, symbolURI, cwd)
	for _, file := range order {
		lines = append(lines, "\n"+file+":")
		for _, s := range groups[file] {
			start := 0
			if s.Location.Range != nil {
				start = s.Location.Range.Start.Line
			}
			line := fmt.Sprintf("  %s (%s) - Line %d", s.Name, symbolKind(s.Kind), start+1)
			if s.ContainerName != "" {
				line += " in " + s.ContainerName
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// callItem is a CallHierarchyItem; Raw keeps the exact item for the
// incomingCalls / outgoingCalls request.
type callItem struct {
	Name   string `json:"name"`
	Kind   int    `json:"kind"`
	Detail string `json:"detail"`
	URI    string `json:"uri"`
	Range  Range  `json:"range"`
}

func formatCallItem(it callItem, cwd string) string {
	if it.URI == "" {
		return fmt.Sprintf("%s (%s) - %s", it.Name, symbolKind(it.Kind), unknownLocation)
	}
	s := fmt.Sprintf("%s (%s) - %s:%d", it.Name, symbolKind(it.Kind), DisplayURI(it.URI, cwd), it.Range.Start.Line+1)
	if it.Detail != "" {
		s += " [" + it.Detail + "]"
	}
	return s
}

// FormatCallItems renders a prepareCallHierarchy result.
func FormatCallItems(items []callItem, cwd string) string {
	switch len(items) {
	case 0:
		return noCallItem
	case 1:
		return "Call hierarchy item: " + formatCallItem(items[0], cwd)
	}
	lines := []string{fmt.Sprintf("Found %d call hierarchy items:", len(items))}
	for _, it := range items {
		lines = append(lines, "  "+formatCallItem(it, cwd))
	}
	return strings.Join(lines, "\n")
}

// callEdge is an incoming (From) or outgoing (To) call.
type callEdge struct {
	From       *callItem `json:"from"`
	To         *callItem `json:"to"`
	FromRanges []Range   `json:"fromRanges"`
}

// FormatCalls renders an incomingCalls (incoming true) or outgoingCalls
// result grouped by the file of the caller / callee.
func FormatCalls(edges []callEdge, incoming bool, cwd string) string {
	if len(edges) == 0 {
		if incoming {
			return noIncomingCalls
		}
		return noOutgoingCalls
	}
	item := func(e callEdge) *callItem {
		if incoming {
			return e.From
		}
		return e.To
	}
	var valid []callEdge
	for _, e := range edges {
		if item(e) != nil {
			valid = append(valid, e)
		}
	}
	dir, label := "incoming", "calls at"
	if !incoming {
		dir, label = "outgoing", "called from"
	}
	lines := []string{fmt.Sprintf("Found %d %s %s:", len(edges), dir, plural(len(edges), "call"))}
	order, groups := groupByFile(valid, func(e callEdge) string { return item(e).URI }, cwd)
	for _, file := range order {
		lines = append(lines, "\n"+file+":")
		for _, e := range groups[file] {
			it := item(e)
			line := fmt.Sprintf("  %s (%s) - Line %d", it.Name, symbolKind(it.Kind), it.Range.Start.Line+1)
			if len(e.FromRanges) > 0 {
				at := make([]string, 0, len(e.FromRanges))
				for _, r := range e.FromRanges {
					at = append(at, fmt.Sprintf("%d:%d", r.Start.Line+1, r.Start.Character+1))
				}
				line += " [" + label + ": " + strings.Join(at, ", ") + "]"
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
