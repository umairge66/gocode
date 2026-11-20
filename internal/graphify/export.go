package graphify

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// graphFile is graph.json: NetworkX node-link data. "directed" is false
// because the graph is analysed as undirected, but each link's source ->
// target is the direction of its relation.
type graphFile struct {
	Directed      bool      `json:"directed"`
	Multigraph    bool      `json:"multigraph"`
	Graph         graphMeta `json:"graph"`
	SchemaVersion int       `json:"schema_version"`
	Generator     string    `json:"generator,omitempty"`
	// Nodes and links are decoded one by one so that a malformed element is
	// dropped instead of failing the whole file. Edges is the name newer
	// NetworkX versions use for Links.
	Nodes []json.RawMessage `json:"nodes"`
	Links []json.RawMessage `json:"links"`
	Edges []json.RawMessage `json:"edges"`
}

type graphMeta struct {
	Communities []Community `json:"communities,omitempty"`
}

// WriteJSON writes the graph as graph.json: one node or link per line, in a
// stable order, so the file diffs cleanly and identical trees give identical
// bytes. The file is replaced in one step.
func WriteJSON(file string, g *Graph, generator string) error {
	head, err := json.Marshal(struct {
		Directed      bool      `json:"directed"`
		Multigraph    bool      `json:"multigraph"`
		Graph         graphMeta `json:"graph"`
		SchemaVersion int       `json:"schema_version"`
		Generator     string    `json:"generator,omitempty"`
	}{Graph: graphMeta{Communities: g.Communities}, SchemaVersion: SchemaVersion, Generator: generator})
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Grow(256*len(g.Nodes) + 224*len(g.Edges) + len(head))
	b.Write(head[:len(head)-1]) // reopen the object
	list := func(key string, n int, item func(i int) any) error {
		b.WriteString(`,"` + key + `":[`)
		for i := 0; i < n; i++ {
			data, err := json.Marshal(item(i))
			if err != nil {
				return err
			}
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
			b.Write(data)
		}
		b.WriteString("\n]")
		return nil
	}
	if err := list("nodes", len(g.Nodes), func(i int) any { return g.Nodes[i] }); err != nil {
		return err
	}
	if err := list("links", len(g.Edges), func(i int) any { return g.Edges[i] }); err != nil {
		return err
	}
	b.WriteString("}\n")
	return writeFileAtomic(file, b.Bytes())
}

// writeFileAtomic replaces file with data without ever leaving a partly
// written file behind: an interrupted run keeps the previous content.
func writeFileAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// ErrNoGraph is returned by LoadJSON when the file does not exist.
var ErrNoGraph = errors.New("no graph found")

// LoadJSON reads a graph.json written by WriteJSON or by another tool that
// writes node-link data with the same attributes.
func LoadJSON(file string) (*Graph, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoGraph
		}
		return nil, err
	}
	// A well-formed file decodes in one pass; only one with a malformed
	// element is taken apart element by element (much slower).
	var fast struct {
		Graph graphMeta `json:"graph"`
		Nodes []*Node   `json:"nodes"`
		Links []*Edge   `json:"links"`
		Edges []*Edge   `json:"edges"`
	}
	var gf graphFile
	if json.Unmarshal(data, &fast) == nil {
		gf.Graph = fast.Graph
	} else if err := json.Unmarshal(data, &gf); err != nil {
		return nil, err
	} else {
		decode := func(raws []json.RawMessage, add func(raw json.RawMessage)) {
			for _, raw := range raws {
				add(raw)
			}
		}
		decode(gf.Nodes, func(raw json.RawMessage) {
			var n Node
			if json.Unmarshal(raw, &n) == nil {
				fast.Nodes = append(fast.Nodes, &n)
			}
		})
		edge := func(raw json.RawMessage) {
			var e Edge
			if json.Unmarshal(raw, &e) == nil {
				fast.Links = append(fast.Links, &e)
			}
		}
		decode(gf.Links, edge)
		if len(gf.Links) == 0 {
			decode(gf.Edges, edge)
		}
	}
	g := &Graph{Communities: gf.Graph.Communities}
	seen := map[string]bool{}
	for _, np := range fast.Nodes {
		if np == nil || np.ID == "" || seen[np.ID] {
			continue
		}
		n := *np
		seen[n.ID] = true
		if n.Label == "" {
			n.Label = n.ID
		}
		n.Community = max(n.Community, 0)
		g.Nodes = append(g.Nodes, n)
	}
	links := fast.Links
	if len(links) == 0 {
		links = fast.Edges
	}
	for _, e := range links {
		if e != nil && (e.Source != e.Target || e.Relation == "calls") {
			g.Edges = append(g.Edges, *e)
		}
	}
	g.link()
	// Drop links whose ends are missing so traversals can trust the index.
	kept := g.Edges[:0]
	for _, e := range g.Edges {
		_, ok1 := g.index[e.Source]
		_, ok2 := g.index[e.Target]
		if ok1 && ok2 {
			kept = append(kept, e)
		}
	}
	if len(kept) != len(g.Edges) {
		g.Edges = kept
		g.link()
	}
	return g, nil
}

//go:embed graph.html.tmpl
var htmlTemplate string

type htmlNode struct {
	L string  `json:"l"`
	F string  `json:"f"`
	K string  `json:"k"`
	C int     `json:"c"`
	D int     `json:"d"`
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// WriteHTML writes a self-contained interactive view of the graph (no
// network access needed to open it). With nodeLimit > 0 only the nodeLimit
// most connected nodes are drawn. It returns how many nodes were drawn.
func WriteHTML(file string, g *Graph, title string, nodeLimit int) (int, error) {
	order := make([]int, len(g.Nodes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return g.Degree(order[a]) > g.Degree(order[b]) })
	if nodeLimit > 0 && len(order) > nodeLimit {
		order = order[:nodeLimit]
	}
	slot := make(map[int]int, len(order)) // graph index -> drawn index
	for k, i := range order {
		slot[i] = k
	}

	// Layout: each community is a disc of its nodes (most connected in the
	// middle), discs are packed along a spiral, largest first. Communities of
	// one drawn node share a last disc.
	groups := map[int][]int{}
	var ids []int
	for _, i := range order {
		c := g.Nodes[i].Community
		if _, ok := groups[c]; !ok {
			ids = append(ids, c)
		}
		groups[c] = append(groups[c], i)
	}
	var discs [][]int
	var single []int
	for _, c := range ids {
		if len(groups[c]) == 1 {
			single = append(single, groups[c][0])
		} else {
			discs = append(discs, groups[c])
		}
	}
	sort.SliceStable(discs, func(a, b int) bool { return len(discs[a]) > len(discs[b]) })
	if len(single) > 0 {
		discs = append(discs, single)
	}
	const spacing = 16.0
	nodes := make([]htmlNode, len(order))
	type circle struct{ x, y, r float64 }
	var placed []circle
	for _, members := range discs {
		r := spacing*math.Sqrt(float64(len(members))) + 30
		var cx, cy float64
		for t := 0.0; ; t += 0.25 {
			cx, cy = 9*t*math.Cos(t), 9*t*math.Sin(t)
			free := true
			for _, p := range placed {
				if math.Hypot(cx-p.x, cy-p.y) < r+p.r {
					free = false
					break
				}
			}
			if free {
				break
			}
		}
		placed = append(placed, circle{cx, cy, r})
		for k, i := range members {
			n := g.Nodes[i]
			rad, ang := spacing*math.Sqrt(float64(k)), float64(k)*2.399963
			nodes[slot[i]] = htmlNode{L: n.Label, F: n.SourceFile, K: n.Kind, C: n.Community, D: g.Degree(i),
				X: math.Round(cx + rad*math.Cos(ang)), Y: math.Round(cy + rad*math.Sin(ang))}
		}
	}

	relIndex := map[string]int{}
	var rels []string
	edges := [][3]int{}
	for _, e := range g.Edges {
		s, ok1 := slot[g.index[e.Source]]
		t, ok2 := slot[g.index[e.Target]]
		if !ok1 || !ok2 {
			continue
		}
		ri, ok := relIndex[e.Relation]
		if !ok {
			ri = len(rels)
			relIndex[e.Relation] = ri
			rels = append(rels, e.Relation)
		}
		edges = append(edges, [3]int{s, t, ri})
	}
	labels := map[int]string{}
	for _, c := range g.Communities {
		labels[c.ID] = c.Label
	}
	data, err := json.Marshal(map[string]any{"n": nodes, "e": edges, "r": rels, "c": labels, "total": len(g.Nodes)})
	if err != nil {
		return 0, err
	}
	titleJSON, _ := json.Marshal(title)
	page := strings.Replace(htmlTemplate, "/*DATA*/null", string(data), 1)
	page = strings.Replace(page, "/*TITLE*/null", string(titleJSON), 1)
	return len(order), writeFileAtomic(file, []byte(page))
}
