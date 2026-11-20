package graphify

import (
	"math"
	"sort"
)

// Graph is the assembled knowledge graph.
type Graph struct {
	Nodes []Node
	Edges []Edge
	// Communities describes the clusters once Analyze ran (or as stored in a
	// loaded graph.json); it may be empty.
	Communities []Community

	index map[string]int
	adj   [][]int // node index -> indices into Edges (both directions)
}

// Build merges the per-file extractions and the cross-file nodes and edges
// into one graph: nodes are unique by id, edges by (source, target,
// relation) keeping the most certain one, and edges to unknown nodes are
// dropped. The only self-loops kept are recursive calls.
func Build(files []*FileExtraction, extraNodes []Node, extraEdges []Edge) *Graph {
	g := &Graph{index: map[string]int{}}
	addNode := func(n Node) {
		if _, dup := g.index[n.ID]; !dup {
			g.index[n.ID] = len(g.Nodes)
			g.Nodes = append(g.Nodes, n)
		}
	}
	for _, n := range extraNodes {
		addNode(n)
	}
	for _, fe := range files {
		for _, n := range fe.Nodes {
			addNode(n)
		}
	}
	seen := map[[3]string]int{}
	addEdge := func(e Edge) {
		if e.Source == e.Target && e.Relation != "calls" {
			return
		}
		if _, ok := g.index[e.Source]; !ok {
			return
		}
		if _, ok := g.index[e.Target]; !ok {
			return
		}
		key := [3]string{e.Source, e.Target, e.Relation}
		if i, dup := seen[key]; dup {
			if edgeWeight(e) > edgeWeight(g.Edges[i]) {
				g.Edges[i] = e
			}
			return
		}
		seen[key] = len(g.Edges)
		g.Edges = append(g.Edges, e)
	}
	for _, fe := range files {
		for _, e := range fe.Edges {
			addEdge(e)
		}
	}
	for _, e := range extraEdges {
		addEdge(e)
	}
	g.link()
	return g
}

// edgeWeight is how much an edge counts: 1 when stated in the source, its
// score when inferred.
func edgeWeight(e Edge) float64 {
	if e.Confidence == Extracted {
		return 1
	}
	if e.ConfidenceScore > 0 {
		return e.ConfidenceScore
	}
	return 0.2
}

// link (re)builds the lookup structures from Nodes and Edges.
func (g *Graph) link() {
	g.index = make(map[string]int, len(g.Nodes))
	for i, n := range g.Nodes {
		g.index[n.ID] = i
	}
	g.adj = make([][]int, len(g.Nodes))
	for ei, e := range g.Edges {
		s, ok1 := g.index[e.Source]
		t, ok2 := g.index[e.Target]
		if !ok1 || !ok2 {
			continue
		}
		g.adj[s] = append(g.adj[s], ei)
		if t != s {
			g.adj[t] = append(g.adj[t], ei)
		}
	}
}

// Node returns the node with the given id.
func (g *Graph) Node(id string) (*Node, bool) {
	i, ok := g.index[id]
	if !ok {
		return nil, false
	}
	return &g.Nodes[i], true
}

// Degree is the number of edges at the node with index i.
func (g *Graph) Degree(i int) int { return len(g.adj[i]) }

// other returns the index of the node at the far end of edge ei from node i.
func (g *Graph) other(ei, i int) int {
	e := g.Edges[ei]
	if s := g.index[e.Source]; s != i {
		return s
	}
	return g.index[e.Target]
}

// isStructural reports nodes that exist to hold others (files, packages):
// they are hubs by construction and say little as analysis results.
func isStructural(n *Node) bool { return n.Kind == KindFile || n.Kind == KindPackage }

// Cluster groups the nodes into communities with the Louvain method and
// stores each node's community id in the graph. Ids are assigned by size,
// largest first. resolution above 1 yields more, smaller communities; 0
// means 1. It returns the members of each community.
func Cluster(g *Graph, resolution float64) [][]int {
	if resolution <= 0 {
		resolution = 1
	}
	n := len(g.Nodes)
	type arc struct {
		to int
		w  float64
	}
	// Weighted undirected adjacency, parallel edges merged.
	merged := make([]map[int]float64, n)
	for _, e := range g.Edges {
		s, t := g.index[e.Source], g.index[e.Target]
		if s == t {
			continue
		}
		w := edgeWeight(e)
		for _, p := range [2][2]int{{s, t}, {t, s}} {
			if merged[p[0]] == nil {
				merged[p[0]] = map[int]float64{}
			}
			merged[p[0]][p[1]] += w
		}
	}
	toArcs := func(m []map[int]float64) [][]arc {
		out := make([][]arc, len(m))
		for i, nb := range m {
			for to, w := range nb {
				out[i] = append(out[i], arc{to, w})
			}
			sort.Slice(out[i], func(a, b int) bool { return out[i][a].to < out[i][b].to })
		}
		return out
	}
	adj := toArcs(merged)
	member := make([]int, n) // original node -> current super-node
	for i := range member {
		member[i] = i
	}

	for level := 0; level < 32; level++ {
		k := len(adj)
		comm := make([]int, k)
		deg := make([]float64, k)
		tot := make([]float64, k)
		var m2 float64
		for i := range adj {
			comm[i] = i
			for _, a := range adj[i] {
				if a.to == i {
					deg[i] += 2 * a.w
				} else {
					deg[i] += a.w
				}
			}
			tot[i] = deg[i]
			m2 += deg[i]
		}
		if m2 == 0 {
			break
		}
		moved := false
		weightTo := map[int]float64{}
		for pass := 0; pass < 64; pass++ {
			changed := false
			for i := range adj {
				clear(weightTo)
				for _, a := range adj[i] {
					if a.to != i {
						weightTo[comm[a.to]] += a.w
					}
				}
				old := comm[i]
				tot[old] -= deg[i]
				best, bestGain := old, weightTo[old]-resolution*tot[old]*deg[i]/m2
				for _, a := range adj[i] { // neighbour order: deterministic
					c := comm[a.to]
					if c == old || a.to == i {
						continue
					}
					if gain := weightTo[c] - resolution*tot[c]*deg[i]/m2; gain > bestGain+1e-12 {
						best, bestGain = c, gain
					}
				}
				tot[best] += deg[i]
				if best != old {
					comm[i] = best
					changed, moved = true, true
				}
			}
			if !changed {
				break
			}
		}
		if !moved {
			break
		}
		// Collapse each community into one node of the next level.
		renum := map[int]int{}
		for i := range comm {
			if _, ok := renum[comm[i]]; !ok {
				renum[comm[i]] = len(renum)
			}
			comm[i] = renum[comm[i]]
		}
		next := make([]map[int]float64, len(renum))
		for i := range adj {
			ci := comm[i]
			if next[ci] == nil {
				next[ci] = map[int]float64{}
			}
			for _, a := range adj[i] {
				cj := comm[a.to]
				// Every undirected edge appears once from each end; an
				// edge inside a community becomes one self-loop of its
				// weight (seen twice, so halve), an original self-loop
				// stays as it is.
				switch {
				case ci != cj || a.to == i:
					next[ci][cj] += a.w
				default:
					next[ci][cj] += a.w / 2
				}
			}
		}
		for i := range member {
			member[i] = comm[member[i]]
		}
		adj = toArcs(next)
	}

	groups := map[int][]int{}
	for i, c := range member {
		groups[c] = append(groups[c], i)
	}
	out := make([][]int, 0, len(groups))
	for _, members := range groups {
		out = append(out, members)
	}
	sort.Slice(out, func(a, b int) bool {
		if len(out[a]) != len(out[b]) {
			return len(out[a]) > len(out[b])
		}
		return out[a][0] < out[b][0]
	})
	for c, members := range out {
		for _, i := range members {
			g.Nodes[i].Community = c
		}
	}
	return out
}

// Cohesion is the share of the possible edges inside a community that exist
// (1 for a clique, 0 for unconnected nodes).
func Cohesion(g *Graph, members []int) float64 {
	if len(members) < 2 {
		return 1
	}
	c := g.Nodes[members[0]].Community
	pairs := map[[2]int]bool{}
	for _, i := range members {
		for _, ei := range g.adj[i] {
			j := g.other(ei, i)
			if j != i && g.Nodes[j].Community == c {
				pairs[[2]int{min(i, j), max(i, j)}] = true
			}
		}
	}
	possible := float64(len(members)) * float64(len(members)-1) / 2
	return math.Round(float64(len(pairs))/possible*1000) / 1000
}
