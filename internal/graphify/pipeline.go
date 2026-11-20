package graphify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Options steers Run.
type Options struct {
	// Root is the directory to scan.
	Root string
	// Force re-extracts every file, ignoring the cache.
	Force bool
	// ClusterOnly skips detect and extract: it reloads the existing
	// graph.json and redoes clustering, analysis, report and export.
	ClusterOnly bool
	// Resolution tunes the community size (0 means 1; higher: more, smaller
	// communities).
	Resolution float64
	// NoViz skips graph.html.
	NoViz bool
	// NodeLimit caps the nodes drawn in graph.html (0: the default of 2000,
	// negative: no cap).
	NodeLimit int
	// NoGitignore also scans files the ignore files hide.
	NoGitignore bool
	// Generator is recorded in graph.json (e.g. "gocode 1.2.3").
	Generator string
	// OutDir is where the outputs go; "" means graphify-out inside Root.
	OutDir string
}

// Result is what one pipeline run produced.
type Result struct {
	OutDir    string
	Detection *Detection // nil with ClusterOnly
	Stats     ExtractStats
	Graph     *Graph
	Analysis  *Analysis
	Diff      *Diff // against the previous graph.json; nil when there was none
	Written   []string
	Drawn     int // nodes in graph.html
}

// Paths of the outputs inside the output directory.
const (
	GraphFile  = "graph.json"
	ReportFile = "GRAPH_REPORT.md"
	HTMLFile   = "graph.html"
	cacheDir   = "cache"
)

// Run executes the pipeline on opt.Root and writes graphify-out/ there.
func Run(ctx context.Context, opt Options) (*Result, error) {
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(root); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New(root + " is not a directory")
	}
	res := &Result{OutDir: filepath.Join(root, OutDirName)}
	if opt.OutDir != "" {
		if res.OutDir, err = filepath.Abs(opt.OutDir); err != nil {
			return nil, err
		}
	}
	graphPath := filepath.Join(res.OutDir, GraphFile)
	old, oldErr := LoadJSON(graphPath)

	in := ReportInput{Title: filepath.Base(root)}
	var g *Graph
	if opt.ClusterOnly {
		if oldErr != nil {
			if errors.Is(oldErr, ErrNoGraph) {
				return nil, errors.New("no graph to re-cluster: " + graphPath + " does not exist; build it first")
			}
			return nil, oldErr
		}
		g = old
	} else {
		det, err := Detect(ctx, root, DetectOptions{NoGitignore: opt.NoGitignore, OutDir: res.OutDir})
		if err != nil {
			return nil, err
		}
		res.Detection, in.Detection = det, det
		if err := os.MkdirAll(res.OutDir, 0o755); err != nil {
			return nil, err
		}
		paths := det.Readable()
		// The cache is per machine and large; keep it out of version control
		// while leaving the graph and the report committable.
		if ignore := filepath.Join(res.OutDir, ".gitignore"); !fileExists(ignore) {
			_ = os.WriteFile(ignore, []byte("cache/\n"), 0o644)
		}
		files, stats, err := Extract(ctx, root, paths, ExtractOptions{CacheDir: filepath.Join(res.OutDir, cacheDir), Force: opt.Force})
		if err != nil {
			return nil, err
		}
		res.Stats, in.Stats = stats, stats
		for _, fe := range files {
			in.Words += fe.Words
			if fe.Err != "" {
				in.Errors = append(in.Errors, fe.Path+": "+fe.Err)
			}
		}
		nodes, edges := Resolve(root, files)
		g = Build(files, nodes, edges)
		if oldErr == nil {
			res.Diff = GraphDiff(old, g)
		}
	}

	communities := Cluster(g, opt.Resolution)
	a := Analyze(g, communities)
	g.Communities = a.Communities
	res.Graph, res.Analysis = g, a
	in.Graph, in.Analysis, in.Diff = g, a, res.Diff

	write := func(name string, do func(file string) error) error {
		file := filepath.Join(res.OutDir, name)
		if err := do(file); err != nil {
			return err
		}
		res.Written = append(res.Written, file)
		return nil
	}
	if err := write(GraphFile, func(f string) error { return WriteJSON(f, g, opt.Generator) }); err != nil {
		return nil, err
	}
	if err := write(ReportFile, func(f string) error { return writeFileAtomic(f, []byte(Report(in))) }); err != nil {
		return nil, err
	}
	if !opt.NoViz {
		limit := opt.NodeLimit
		if limit == 0 {
			limit = 2000
		}
		err := write(HTMLFile, func(f string) error {
			var err error
			res.Drawn, err = WriteHTML(f, g, in.Title+" — knowledge graph", limit)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func fileExists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}
