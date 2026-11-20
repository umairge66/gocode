package graphify

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// ReportInput is everything GRAPH_REPORT.md is written from.
type ReportInput struct {
	Title     string
	Detection *Detection
	Stats     ExtractStats
	Graph     *Graph
	Analysis  *Analysis
	Diff      *Diff
	Words     int
	Errors    []string // files that did not parse, "path: message"
}

// Report renders GRAPH_REPORT.md.
func Report(in ReportInput) string {
	g, a := in.Graph, in.Analysis
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Graph report: %s\n\n", in.Title)
	w("Built by the gocode graphify pipeline (detect → extract → build → cluster → analyze → report → export). ")
	w("Code is read locally; no model is involved.\n\n")

	w("## Summary\n\n")
	if d := in.Detection; d != nil {
		w("- Files read: %d code, %d documents (%d words)\n", len(d.Files[CatCode]), len(d.Files[CatDocument]), in.Words)
		if n := len(d.Files[CatPaper]) + len(d.Files[CatImage]) + len(d.Files[CatMedia]); n > 0 {
			w("- Not read: %d PDFs, %d images, %d audio/video files (no extractor for these)\n",
				len(d.Files[CatPaper]), len(d.Files[CatImage]), len(d.Files[CatMedia]))
		}
		if un := d.Files[CatUnparsed]; len(un) > 0 {
			// Say so plainly: code that is in the tree but not in the graph
			// must not read as code that does not exist.
			counts := map[string]int{}
			for _, f := range un {
				counts[strings.ToLower(path.Ext(f))]++
			}
			exts := make([]string, 0, len(counts))
			for e := range counts {
				exts = append(exts, e)
			}
			sort.Slice(exts, func(i, j int) bool {
				if counts[exts[i]] != counts[exts[j]] {
					return counts[exts[i]] > counts[exts[j]]
				}
				return exts[i] < exts[j]
			})
			parts := make([]string, 0, len(exts))
			for _, e := range exts[:min(8, len(exts))] {
				parts = append(parts, fmt.Sprintf("%s %d", e, counts[e]))
			}
			w("- Not parsed: %d source files in languages without an extractor (%s). They are file nodes only; their functions and calls are not in the graph\n", len(un), strings.Join(parts, ", "))
		}
		if in.Stats.Skipped > 0 {
			w("- Skipped: %d files (unreadable, binary, minified or over %d KB)\n", in.Stats.Skipped, maxFileBytes>>10)
		}
	}
	w("- Graph: %d nodes, %d edges, %d communities", len(g.Nodes), len(g.Edges), len(a.Communities))
	if a.Isolated > 0 {
		w(" (+%d unconnected nodes)", a.Isolated)
	}
	w("\n")
	if total := len(g.Edges); total > 0 {
		w("- Edge confidence: %d EXTRACTED (stated in the source), %d INFERRED (resolved by name)\n", a.Confidence[Extracted], a.Confidence[Inferred])
		rels := make([]string, 0, len(a.Relations))
		for r := range a.Relations {
			rels = append(rels, r)
		}
		sort.Slice(rels, func(i, j int) bool { return a.Relations[rels[i]] > a.Relations[rels[j]] })
		parts := make([]string, len(rels))
		for i, r := range rels {
			parts[i] = fmt.Sprintf("%s %d", r, a.Relations[r])
		}
		w("- Relations: %s\n", strings.Join(parts, ", "))
	}
	w("\n")

	if !in.Diff.Empty() {
		d := in.Diff
		w("## Changes since the last run\n\n")
		w("- Nodes: +%d, -%d\n- Edges: +%d, -%d\n", len(d.NodesAdded), len(d.NodesRemoved), d.EdgesAdded, d.EdgesRemoved)
		list := func(title string, items []string) {
			if len(items) == 0 {
				return
			}
			w("- %s: ", title)
			shown := items[:min(8, len(items))]
			w("%s", codeList(shown))
			if len(items) > len(shown) {
				w(" and %d more", len(items)-len(shown))
			}
			w("\n")
		}
		list("Added", d.NodesAdded)
		list("Removed", d.NodesRemoved)
		w("\n")
	}

	w("## God nodes\n\nThe most connected definitions: what most of the code leans on.\n\n")
	if len(a.GodNodes) == 0 {
		w("None: the graph has no connected definitions.\n")
	}
	for i, n := range a.GodNodes {
		w("%d. %s — %d edges (%s)\n", i+1, code(n.Label), n.Degree, code(n.SourceFile))
	}
	w("\n")

	w("## Communities\n\n")
	if len(a.Communities) == 0 {
		w("None.\n")
	} else {
		w("| # | Community | Nodes | Cohesion | Most connected |\n|---|---|---|---|---|\n")
		shown := a.Communities[:min(30, len(a.Communities))]
		for _, c := range shown {
			w("| %d | %s | %d | %.3f | %s |\n", c.ID, escapeCell(code(c.Label)), c.Size, c.Cohesion, escapeCell(codeList(c.Top[:min(3, len(c.Top))])))
		}
		if rest := len(a.Communities) - len(shown); rest > 0 {
			w("\n%d smaller communities are in graph.json.\n", rest)
		}
	}
	w("\n")

	w("## Surprising connections\n\nEdges between communities that otherwise hardly touch.\n\n")
	if len(a.Surprising) == 0 {
		w("None found.\n")
	}
	for _, s := range a.Surprising {
		conf := s.Edge.Confidence
		if conf == Inferred {
			conf = fmt.Sprintf("INFERRED %.2f", s.Edge.ConfidenceScore)
		}
		share := fmt.Sprintf("one of %d links", s.Links)
		if s.Links == 1 {
			share = "the only link"
		}
		w("- %s —%s→ %s [%s] (%s between communities %d and %d; %s)\n",
			code(s.From), s.Edge.Relation, code(s.To), conf, share, s.FromComm, s.ToComm, code(strings.TrimSpace(s.Edge.SourceFile+" "+s.Edge.SourceLocation)))
	}
	w("\n")

	w("## Import cycles\n\n")
	if len(a.Cycles) == 0 {
		w("None.\n")
	}
	for _, c := range a.Cycles[:min(10, len(a.Cycles))] {
		shown := c[:min(8, len(c))]
		w("- %s", strings.ReplaceAll(codeList(shown), ", ", " ↔ "))
		if len(c) > len(shown) {
			w(" and %d more", len(c)-len(shown))
		}
		w("\n")
	}
	w("\n")

	if len(a.Questions) > 0 {
		w("## Suggested questions\n\n")
		for _, q := range a.Questions {
			w("- %s\n", q)
		}
		w("\n")
	}

	if len(in.Errors) > 0 {
		w("## Files with parse errors\n\n")
		for _, e := range in.Errors[:min(20, len(in.Errors))] {
			w("- %s\n", code(e))
		}
		w("\n")
	}

	w("## Using the graph\n\n")
	w("- `gocode graphify query \"<question>\"` — the nodes and edges relevant to a question\n")
	w("- `gocode graphify explain \"<name>\"` — one node and everything connected to it\n")
	w("- `gocode graphify path \"<A>\" \"<B>\"` — the shortest connection between two nodes\n")
	w("- `graph.html` — interactive view; `graph.json` — the full graph (NetworkX node-link format)\n")
	return b.String()
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// code renders a name as a Markdown code span, so the underscores, asterisks
// and angle brackets that names contain are not read as formatting.
func code(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "`", "'")
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func codeList(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = code(s)
	}
	return strings.Join(out, ", ")
}
