package graphify

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Community describes one cluster of the graph.
type Community struct {
	ID       int      `json:"id"`
	Label    string   `json:"label"`
	Size     int      `json:"size"`
	Cohesion float64  `json:"cohesion"`
	Top      []string `json:"top,omitempty"` // labels of its most connected nodes
	members  []int
}

// Ranked is a node with its degree.
type Ranked struct {
	ID, Label, SourceFile string
	Degree                int
}

// Bridge is an edge that connects two otherwise loosely connected
// communities.
type Bridge struct {
	Edge     Edge
	From, To string // labels
	FromComm int
	ToComm   int
	Links    int // edges between the two communities
}

// Analysis is what the analyze stage finds in a clustered graph.
type Analysis struct {
	Communities []Community
	Isolated    int // communities of a single node, left out of Communities
	GodNodes    []Ranked
	Surprising  []Bridge
	Cycles      [][]string
	Questions   []string
	Confidence  map[string]int
	Relations   map[string]int
}

// Analyze derives the report's findings from a clustered graph.
func Analyze(g *Graph, communities [][]int) *Analysis {
	a := &Analysis{Confidence: map[string]int{}, Relations: map[string]int{}}
	for _, e := range g.Edges {
		a.Confidence[e.Confidence]++
		a.Relations[e.Relation]++
	}
	byDegree := func(idx []int) {
		sort.SliceStable(idx, func(x, y int) bool {
			if dx, dy := g.Degree(idx[x]), g.Degree(idx[y]); dx != dy {
				return dx > dy
			}
			return g.Nodes[idx[x]].ID < g.Nodes[idx[y]].ID
		})
	}

	for c, members := range communities {
		if len(members) < 2 {
			a.Isolated++
			continue
		}
		ranked := append([]int(nil), members...)
		byDegree(ranked)
		com := Community{ID: c, Size: len(members), Cohesion: Cohesion(g, members), members: members}
		dirs := map[string]int{}
		for _, i := range members {
			n := &g.Nodes[i]
			d := n.SourceFile
			if n.Kind != KindPackage {
				d = path.Dir(d)
			}
			dirs[d]++
		}
		dir, best := "", 0
		for d, k := range dirs {
			if k > best || (k == best && d < dir) {
				dir, best = d, k
			}
		}
		for _, i := range ranked {
			if !isStructural(&g.Nodes[i]) && len(com.Top) < 5 {
				com.Top = append(com.Top, g.Nodes[i].Label)
			}
		}
		lead := g.Nodes[ranked[0]].Label
		if len(com.Top) > 0 {
			lead = com.Top[0]
		}
		if dir == "." || dir == "" {
			com.Label = lead
		} else {
			com.Label = dir + " · " + lead
		}
		a.Communities = append(a.Communities, com)
	}

	// God nodes: the most connected definitions.
	var all []int
	for i := range g.Nodes {
		if !isStructural(&g.Nodes[i]) && g.Degree(i) > 0 {
			all = append(all, i)
		}
	}
	byDegree(all)
	for _, i := range all[:min(10, len(all))] {
		n := g.Nodes[i]
		a.GodNodes = append(a.GodNodes, Ranked{ID: n.ID, Label: n.Label, SourceFile: n.SourceFile, Degree: g.Degree(i)})
	}

	a.Surprising = surprising(g, communities)
	a.Cycles = importCycles(g)
	a.Questions = suggestQuestions(g, a)
	return a
}

// surprising returns edges between communities that share few edges: the
// places where two separate areas of the code touch.
func surprising(g *Graph, communities [][]int) []Bridge {
	type pair [2]int
	links := map[pair]int{}
	var cross []int
	for ei, e := range g.Edges {
		if e.Relation == "contains" || e.Relation == "imports" || e.Relation == "method" {
			continue
		}
		s, t := &g.Nodes[g.index[e.Source]], &g.Nodes[g.index[e.Target]]
		if s.Community == t.Community || len(communities[s.Community]) < 3 || len(communities[t.Community]) < 3 {
			continue
		}
		links[pair{min(s.Community, t.Community), max(s.Community, t.Community)}]++
		cross = append(cross, ei)
	}
	key := func(ei int) pair {
		e := g.Edges[ei]
		s, t := g.Nodes[g.index[e.Source]].Community, g.Nodes[g.index[e.Target]].Community
		return pair{min(s, t), max(s, t)}
	}
	weight := func(ei int) int {
		e := g.Edges[ei]
		return g.Degree(g.index[e.Source]) + g.Degree(g.index[e.Target])
	}
	sort.SliceStable(cross, func(x, y int) bool {
		if lx, ly := links[key(cross[x])], links[key(cross[y])]; lx != ly {
			return lx < ly
		}
		return weight(cross[x]) > weight(cross[y])
	})
	var out []Bridge
	used := map[pair]bool{}
	for _, ei := range cross {
		k := key(ei)
		if used[k] {
			continue
		}
		used[k] = true
		e := g.Edges[ei]
		s, t := g.Nodes[g.index[e.Source]], g.Nodes[g.index[e.Target]]
		out = append(out, Bridge{Edge: e, From: s.Label, To: t.Label, FromComm: s.Community, ToComm: t.Community, Links: links[k]})
		if len(out) == 10 {
			break
		}
	}
	return out
}

// importCycles finds groups of files (Go: packages) that import each other.
func importCycles(g *Graph) [][]string {
	succ := map[string][]string{}
	var order []string
	for _, e := range g.Edges {
		if e.Relation != "imports" || e.TypeOnly || strings.HasSuffix(e.Source, "_test.go") {
			continue
		}
		from := e.Source
		if strings.HasPrefix(e.Target, "pkg:") {
			from = "pkg:" + path.Dir(e.Source)
		}
		if from == e.Target {
			continue
		}
		if _, ok := succ[from]; !ok {
			order = append(order, from)
		}
		succ[from] = append(succ[from], e.Target)
	}
	// Tarjan's strongly connected components.
	index, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var cycles [][]string
	next := 0
	var visit func(v string)
	visit = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range succ[v] {
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, strings.TrimPrefix(w, "pkg:"))
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Strings(comp)
				cycles = append(cycles, comp)
			}
		}
	}
	for _, v := range order {
		if _, seen := index[v]; !seen {
			visit(v)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i][0] < cycles[j][0] })
	return cycles
}

func suggestQuestions(g *Graph, a *Analysis) []string {
	var qs []string
	if len(a.GodNodes) > 0 {
		n := a.GodNodes[0]
		qs = append(qs, fmt.Sprintf("What depends on `%s` (`%s`), and what would break if it changed?", n.Label, n.SourceFile))
	}
	if len(a.Surprising) > 0 {
		b := a.Surprising[0]
		share := fmt.Sprintf("one of only %d links", b.Links)
		if b.Links == 1 {
			share = "the only link"
		}
		qs = append(qs, fmt.Sprintf("Why does `%s` reach `%s`? It is %s between those two areas.", b.From, b.To, share))
	}
	var loose *Community
	for i := range a.Communities {
		c := &a.Communities[i]
		if c.Size >= 20 && (loose == nil || c.Cohesion < loose.Cohesion) {
			loose = c
		}
	}
	if loose != nil {
		qs = append(qs, fmt.Sprintf("Is `%s` one responsibility or several? It has %d nodes but a cohesion of only %.3f.", loose.Label, loose.Size, loose.Cohesion))
	}
	if len(a.Cycles) > 0 {
		qs = append(qs, fmt.Sprintf("Can the import cycle between `%s` be broken?", strings.Join(a.Cycles[0][:min(4, len(a.Cycles[0]))], "`, `")))
	}
	if len(a.GodNodes) > 1 {
		qs = append(qs, fmt.Sprintf("How are `%s` and `%s` connected?", a.GodNodes[0].Label, a.GodNodes[1].Label))
	}
	return qs
}

// Diff is how a graph differs from an earlier one.
type Diff struct {
	NodesAdded, NodesRemoved []string // labels
	EdgesAdded, EdgesRemoved int
}

// Empty reports whether nothing changed.
func (d *Diff) Empty() bool {
	return d == nil || len(d.NodesAdded)+len(d.NodesRemoved)+d.EdgesAdded+d.EdgesRemoved == 0
}

// GraphDiff compares two graphs by node id and by (source, target, relation).
func GraphDiff(old, cur *Graph) *Diff {
	d := &Diff{}
	for _, n := range cur.Nodes {
		if _, ok := old.index[n.ID]; !ok {
			d.NodesAdded = append(d.NodesAdded, n.Label+" ("+n.SourceFile+")")
		}
	}
	for _, n := range old.Nodes {
		if _, ok := cur.index[n.ID]; !ok {
			d.NodesRemoved = append(d.NodesRemoved, n.Label+" ("+n.SourceFile+")")
		}
	}
	keys := func(g *Graph) map[[3]string]bool {
		m := make(map[[3]string]bool, len(g.Edges))
		for _, e := range g.Edges {
			m[[3]string{e.Source, e.Target, e.Relation}] = true
		}
		return m
	}
	ok, ck := keys(old), keys(cur)
	for k := range ck {
		if !ok[k] {
			d.EdgesAdded++
		}
	}
	for k := range ok {
		if !ck[k] {
			d.EdgesRemoved++
		}
	}
	return d
}
