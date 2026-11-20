package graphify

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Exit codes of the query commands, as the graphify CLI defines them.
const (
	ExitOK        = 0
	ExitAmbiguous = 3
	ExitNoMatch   = 4
)

// absentNote keeps "not in the graph" from being read as "not in the code".
const absentNote = "The graph holds the names of definitions and files, not their bodies, and reads some languages by pattern or not at all, so this does not show the code lacks it; search the files to be sure."

// isTestPath reports files that hold tests rather than the code under test.
func isTestPath(p string) bool {
	base := p[strings.LastIndex(p, "/")+1:]
	stem := strings.TrimSuffix(base, pathExt(base))
	lower := strings.ToLower(base)
	if strings.Contains(lower, "_test.") || strings.Contains(lower, ".test.") || strings.Contains(lower, ".spec.") ||
		strings.HasPrefix(lower, "test_") || strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests") || strings.HasSuffix(stem, "_spec") {
		return true
	}
	dir := "/" + strings.ToLower(p)
	for _, d := range []string{"/test/", "/tests/", "/__tests__/", "/testdata/", "/spec/"} {
		if strings.Contains(dir, d) {
			return true
		}
	}
	return false
}

func pathExt(base string) string {
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[i:]
	}
	return ""
}

// Find returns the nodes a name refers to, best tier first: the exact id,
// then the exact label (with or without "()" and a "Type." prefix), then a
// substring of the label. Only the first tier that matches is returned.
func (g *Graph) Find(name string) []int {
	name = strings.TrimSpace(name)
	if i, ok := g.index[name]; ok {
		return []int{i}
	}
	want := strings.ToLower(strings.TrimSuffix(name, "()"))
	if want == "" {
		return nil
	}
	var exact, short, partial []int
	for i, n := range g.Nodes {
		label := strings.ToLower(strings.TrimSuffix(n.Label, "()"))
		switch {
		case label == want:
			exact = append(exact, i)
		case lastSegment(label) == want:
			short = append(short, i)
		case strings.Contains(label, want) || strings.Contains(strings.ToLower(n.ID), want):
			partial = append(partial, i)
		}
	}
	for _, tier := range [][]int{exact, short, partial} {
		if len(tier) > 0 {
			sort.SliceStable(tier, func(a, b int) bool { return g.Degree(tier[a]) > g.Degree(tier[b]) })
			return tier
		}
	}
	return nil
}

func (g *Graph) describe(i int) string {
	n := g.Nodes[i]
	where := n.SourceFile
	if n.SourceLocation != "" {
		where += ":" + n.SourceLocation
	}
	kind := n.Kind
	if kind == "" {
		kind = n.FileType
	}
	return fmt.Sprintf("%s  [%s] %s", n.Label, kind, where)
}

func (g *Graph) communityLabel(c int) string {
	for _, com := range g.Communities {
		if com.ID == c {
			return fmt.Sprintf("%d (%s)", c, com.Label)
		}
	}
	return fmt.Sprint(c)
}

// resolveOne finds the single node a name refers to, or explains why not.
func (g *Graph) resolveOne(name string) (int, string, int) {
	hits := g.Find(name)
	if len(hits) > 1 {
		// A definition wins over the file or package of the same name.
		var defs []int
		for _, i := range hits {
			if !isStructural(&g.Nodes[i]) {
				defs = append(defs, i)
			}
		}
		if len(defs) == 1 {
			hits = defs
		}
	}
	switch {
	case len(hits) == 0:
		return -1, fmt.Sprintf("No node matches %q.\n%s\n", name, absentNote), ExitNoMatch
	case len(hits) > 1:
		var b strings.Builder
		fmt.Fprintf(&b, "%q is ambiguous: %d nodes match. Use one of these ids:\n", name, len(hits))
		for _, i := range hits[:min(12, len(hits))] {
			fmt.Fprintf(&b, "  %s\n      %s\n", g.Nodes[i].ID, g.describe(i))
		}
		if len(hits) > 12 {
			fmt.Fprintf(&b, "  … and %d more\n", len(hits)-12)
		}
		return -1, b.String(), ExitAmbiguous
	}
	return hits[0], "", ExitOK
}

func (g *Graph) edgeLine(ei int) string {
	e := g.Edges[ei]
	conf := e.Confidence
	if conf == Inferred {
		conf = fmt.Sprintf("INFERRED %.2f", e.ConfidenceScore)
	}
	line := fmt.Sprintf("%s --%s--> %s  [%s]", g.Nodes[g.index[e.Source]].Label, e.Relation, g.Nodes[g.index[e.Target]].Label, conf)
	if e.SourceFile != "" && e.Relation != "contains" && e.Relation != "method" {
		line += "  " + e.SourceFile
		if e.SourceLocation != "" {
			line += ":" + e.SourceLocation
		}
	}
	return line
}

// Explain describes one node and everything connected to it.
func (g *Graph) Explain(name string) (string, int) {
	i, msg, code := g.resolveOne(name)
	if code != ExitOK {
		return msg, code
	}
	n := g.Nodes[i]
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  id: %s\n  community: %s\n  edges: %d\n", g.describe(i), n.ID, g.communityLabel(n.Community), g.Degree(i))
	groups := map[string][]int{}
	var order []string
	for _, ei := range g.adj[i] {
		e := g.Edges[ei]
		key := e.Relation + " →"
		switch {
		case e.Source == e.Target:
			key = e.Relation + " ↺ itself (recursive)"
		case e.Target == n.ID:
			key = "← " + e.Relation
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], g.other(ei, i))
	}
	sort.Strings(order)
	for _, key := range order {
		list := groups[key]
		sort.SliceStable(list, func(a, c int) bool { return g.Degree(list[a]) > g.Degree(list[c]) })
		if strings.Contains(key, "↺") {
			fmt.Fprintf(&b, "\n%s\n", key)
			continue
		}
		fmt.Fprintf(&b, "\n%s (%d)\n", key, len(list))
		for _, j := range list[:min(15, len(list))] {
			fmt.Fprintf(&b, "  %s\n", g.describe(j))
		}
		if len(list) > 15 {
			fmt.Fprintf(&b, "  … and %d more\n", len(list)-15)
		}
	}
	return b.String(), ExitOK
}

// Path prints the shortest connection between two nodes. Files and packages
// are avoided as stepping stones when a path through definitions exists.
func (g *Graph) Path(from, to string) (string, int) {
	a, msg, code := g.resolveOne(from)
	if code != ExitOK {
		return msg, code
	}
	z, msg, code := g.resolveOne(to)
	if code != ExitOK {
		return msg, code
	}
	if a == z {
		return "Both names refer to the same node: " + g.describe(a) + "\n", ExitOK
	}
	search := func(allowStructural bool) []int { // edge indices from a to z
		prev := map[int]int{a: -1}
		queue := []int{a}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur == z {
				var path []int
				for at := z; prev[at] != -1; at = g.other(prev[at], at) {
					path = append(path, prev[at])
				}
				for l, r := 0, len(path)-1; l < r; l, r = l+1, r-1 {
					path[l], path[r] = path[r], path[l]
				}
				return path
			}
			for _, ei := range g.adj[cur] {
				nb := g.other(ei, cur)
				if _, seen := prev[nb]; seen {
					continue
				}
				if !allowStructural && nb != z && isStructural(&g.Nodes[nb]) {
					continue
				}
				prev[nb] = ei
				queue = append(queue, nb)
			}
		}
		return nil
	}
	path := search(false)
	if path == nil {
		path = search(true)
	}
	if path == nil {
		return fmt.Sprintf("No path between %s and %s.\n", g.Nodes[a].Label, g.Nodes[z].Label), ExitNoMatch
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d hops from %s to %s:\n\n  %s\n", len(path), g.Nodes[a].Label, g.Nodes[z].Label, g.describe(a))
	at := a
	for _, ei := range path {
		e := g.Edges[ei]
		next := g.other(ei, at)
		arrow := fmt.Sprintf("--%s-->", e.Relation)
		if g.index[e.Target] == at {
			arrow = fmt.Sprintf("<--%s--", e.Relation)
		}
		fmt.Fprintf(&b, "    %s\n  %s\n", arrow, g.describe(next))
		at = next
	}
	return b.String(), ExitOK
}

var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for are what where when which who how why does did can could would should
		this that these those with from into about between connects connect connected use uses used using work works
		call calls called file files code function functions method methods class classes get has have not all any its`) {
		m[w] = true
	}
	return m
}()

// words splits text into lower-case words, also at camelCase and separator
// boundaries.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// Query returns the part of the graph relevant to a free-text question: the
// nodes whose names match its words and what lies within depth edges of
// them, cut off at about budget tokens. depth and budget of 0 mean 2 and
// 2000.
func (g *Graph) Query(question string, depth, budget int) (string, int) {
	if depth <= 0 {
		depth = 2
	}
	if budget <= 0 {
		budget = 2000
	}
	var terms []string
	seenTerm := map[string]bool{}
	add := func(t string) {
		if len(t) >= 3 && !stopWords[t] && !seenTerm[t] {
			seenTerm[t] = true
			terms = append(terms, t)
		}
	}
	for _, f := range strings.FieldsFunc(question, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(`"'?,;:()`, r) }) {
		add(strings.ToLower(strings.TrimSuffix(f, "()")))
		for _, w := range words(f) {
			add(w)
		}
	}
	if len(terms) == 0 {
		return "The question has no words to search for.\n", ExitNoMatch
	}

	aboutTests := false
	for _, t := range terms {
		if strings.HasPrefix(t, "test") || t == "spec" || t == "specs" {
			aboutTests = true
		}
	}
	type scored struct {
		i     int
		score float64
	}
	var hits []scored
	for i, n := range g.Nodes {
		label := strings.ToLower(strings.TrimSuffix(n.Label, "()"))
		parts := map[string]bool{}
		for _, w := range words(strings.TrimSuffix(n.Label, "()")) {
			parts[w] = true
		}
		file := strings.ToLower(n.SourceFile)
		var s float64
		for _, t := range terms {
			switch {
			case label == t || lastSegment(label) == t:
				s += 10
			case parts[t]:
				s += 5
			case strings.Contains(label, t):
				s += 3
			case strings.Contains(file, t):
				s += 1
			}
		}
		if s > 0 {
			if isStructural(&n) {
				s *= 0.6
			}
			switch isTest := isTestPath(n.SourceFile); {
			case isTest && !aboutTests:
				s *= 0.4 // a test of the thing is rarely the thing asked about
			case isTest:
				s *= 1.5
			}
			hits = append(hits, scored{i, s + 0.2*math.Log1p(float64(g.Degree(i)))})
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("Nothing in the graph matches: %s\n%s\n", strings.Join(terms, ", "), absentNote), ExitNoMatch
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	var seeds []int
	for _, h := range hits {
		if len(seeds) == 6 || h.score < 0.5*hits[0].score {
			break
		}
		seeds = append(seeds, h.i)
	}

	limit := budget * 4 // about four characters per token
	var b strings.Builder
	fmt.Fprintf(&b, "Query: %s\nMatched on: %s\n\nStarting points:\n", strings.TrimSpace(question), strings.Join(terms, ", "))
	for _, i := range seeds {
		fmt.Fprintf(&b, "  %s  — community %s\n", g.describe(i), g.communityLabel(g.Nodes[i].Community))
	}
	b.WriteString("\nConnections:\n")

	// Breadth-first from the seeds; "contains" edges last at each level, and
	// no expansion through files and packages (they connect everything).
	dist := map[int]int{}
	for _, s := range seeds {
		dist[s] = 0
	}
	frontier := seeds
	seenEdge := map[int]bool{}
	shown, dropped := 0, 0
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var edges []int
		var next []int
		for _, i := range frontier {
			if d > 0 && isStructural(&g.Nodes[i]) {
				continue
			}
			for _, ei := range g.adj[i] {
				if seenEdge[ei] {
					continue
				}
				seenEdge[ei] = true
				edges = append(edges, ei)
				if nb := g.other(ei, i); dist[nb] == 0 && !contains(seeds, nb) {
					dist[nb] = d + 1
					next = append(next, nb)
				}
			}
		}
		rank := func(ei int) int {
			switch g.Edges[ei].Relation {
			case "contains":
				return 2
			case "imports", "method":
				return 1
			}
			return 0
		}
		sort.SliceStable(edges, func(x, y int) bool { return rank(edges[x]) < rank(edges[y]) })
		for _, ei := range edges {
			line := "  " + g.edgeLine(ei) + "\n"
			if b.Len()+len(line) > limit {
				dropped++
				continue
			}
			b.WriteString(line)
			shown++
		}
		frontier = next
	}
	if shown == 0 && dropped == 0 {
		b.WriteString("  (none: the matched nodes have no edges)\n")
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n… %d more edges left out to stay within the budget of %d tokens; raise --budget or lower --depth.\n", dropped, budget)
	}
	return b.String(), ExitOK
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Stale reports how many source files under root are newer than the graph
// file (changed since it was built), and one of them as an example. It looks
// only at files the pipeline would read.
func Stale(ctx context.Context, root, graphFile string) (changed int, example string) {
	info, err := os.Stat(graphFile)
	if err != nil {
		return 0, ""
	}
	det, err := Detect(ctx, root, DetectOptions{OutDir: filepath.Dir(graphFile)})
	if err != nil {
		return 0, ""
	}
	built := info.ModTime()
	for _, rel := range det.Readable() {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil && fi.ModTime().After(built) {
			if changed++; example == "" {
				example = rel
			}
		}
	}
	return changed, example
}
