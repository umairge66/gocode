package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/graphify"
)

// `gocode graphify` and the /graphify slash command: build a knowledge graph
// of a source tree and query it (internal/graphify). The pipeline reads the
// files of the tree and writes graphify-out/ into it; it runs no command,
// makes no network call and uses no model.
//
//	gocode graphify [path] [--force] [--cluster-only] [--resolution N]
//	                       [--no-viz] [--node-limit N] [--no-gitignore]
//	gocode graphify query "<question>" [--budget N] [--depth N]
//	gocode graphify path "<A>" "<B>"
//	gocode graphify explain "<name>"
//
// The query verbs read graphify-out/graph.json from the current directory or
// the nearest parent that has one (--graph <file> names another). Their exit
// codes follow the graphify CLI: 0 answered, 3 ambiguous name, 4 no match.

const graphifyUsage = `Usage:
  gocode graphify [path]             Build (or update) the knowledge graph of a directory
  gocode graphify query "<question>" Show the part of the graph relevant to a question
  gocode graphify path "<A>" "<B>"   Shortest connection between two nodes
  gocode graphify explain "<name>"   One node and everything connected to it

Build options:
  --force            Re-read every file instead of using the cache
  --cluster-only     Re-cluster the existing graph without reading files
  --resolution <n>   Community granularity (default 1; higher gives more, smaller communities)
  --no-viz           Do not write graph.html
  --node-limit <n>   Nodes drawn in graph.html (default 2000; 0 draws all)
  --no-gitignore     Also read files ignored by .gitignore
  --out <dir>        Write the outputs to this directory instead of <path>/graphify-out
  --update           Accepted for graphify compatibility; builds are always incremental

Query options:
  --budget <n>       Approximate size of the answer in tokens (default 2000)
  --depth <n>        How many edges to follow from the matched nodes (default 2)
  --graph <file>     The graph.json to read (default: graphify-out/graph.json here or in a parent)
  --out <dir>        Read graph.json from this output directory

Output goes to <path>/graphify-out/: graph.json, GRAPH_REPORT.md, graph.html, cache/.`

// runGraphifyCmd is the `gocode graphify` command action.
func runGraphifyCmd(env *Env, t []string) int {
	args := t
	for i, a := range t {
		if a == "graphify" {
			args = t[i+1:]
			break
		}
	}
	cwd, err := env.Cwd()
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode graphify: %v\n", err)
		return 1
	}
	text, code := graphifyRun(context.Background(), cwd, args)
	out := env.Stdout
	if code != 0 && code != graphify.ExitAmbiguous && code != graphify.ExitNoMatch {
		out = env.Stderr
	}
	fmt.Fprintln(out, strings.TrimRight(text, "\n"))
	return code
}

// graphifySlash runs /graphify <args> and returns the text to show.
func graphifySlash(env *Env, args string) string {
	cwd, err := env.Cwd()
	if err != nil {
		return "graphify: " + err.Error()
	}
	text, _ := graphifyRun(context.Background(), cwd, splitQuotedArgs(args))
	return strings.TrimRight(text, "\n")
}

// splitQuotedArgs splits a slash command's argument string into words,
// keeping "double" and 'single' quoted parts together.
func splitQuotedArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// graphifyArgs is the parsed command line.
type graphifyArgs struct {
	pos         []string
	force       bool
	clusterOnly bool
	noViz       bool
	noGitignore bool
	resolution  float64
	nodeLimit   int
	budget      int
	depth       int
	graph       string
	out         string
}

func parseGraphifyArgs(args []string) (graphifyArgs, error) {
	var a graphifyArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		next := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		number := func() (int, error) {
			v, err := next()
			if err != nil {
				return 0, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("%s needs a whole number, not %q", name, v)
			}
			return n, nil
		}
		var err error
		switch name {
		case "--force":
			a.force = true
		case "--cluster-only":
			a.clusterOnly = true
		case "--no-viz":
			a.noViz = true
		case "--no-gitignore":
			a.noGitignore = true
		case "--update":
		case "--resolution":
			var v string
			if v, err = next(); err == nil {
				if a.resolution, err = strconv.ParseFloat(v, 64); err != nil || a.resolution <= 0 {
					err = fmt.Errorf("--resolution needs a number above 0, not %q", v)
				}
			}
		case "--node-limit":
			if a.nodeLimit, err = number(); err == nil && a.nodeLimit == 0 {
				a.nodeLimit = -1 // draw all
			}
		case "--budget":
			a.budget, err = number()
		case "--depth":
			a.depth, err = number()
		case "--graph":
			a.graph, err = next()
		case "--out":
			a.out, err = next()
		default:
			if strings.HasPrefix(arg, "-") && arg != "-" {
				err = fmt.Errorf("unknown option %s", arg)
			} else {
				a.pos = append(a.pos, arg)
			}
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

// graphifyRun executes one graphify command line (the words after
// "graphify") and returns its output and exit code.
func graphifyRun(ctx context.Context, cwd string, args []string) (string, int) {
	a, err := parseGraphifyArgs(args)
	if err != nil {
		return "gocode graphify: " + err.Error() + "\n\n" + graphifyUsage, 2
	}
	verb := ""
	if len(a.pos) > 0 {
		verb = a.pos[0]
	}
	switch verb {
	case "help":
		return graphifyUsage, 0
	case "query", "path", "explain":
		rest := a.pos[1:]
		want := map[string]int{"query": 1, "path": 2, "explain": 1}[verb]
		if verb == "query" && len(rest) > 1 {
			rest = []string{strings.Join(rest, " ")} // an unquoted question
		}
		if len(rest) != want {
			return fmt.Sprintf("gocode graphify %s takes %d argument(s).\n\n%s", verb, want, graphifyUsage), 2
		}
		file := a.graph
		switch {
		case file == "" && a.out != "":
			file = filepath.Join(absFrom(cwd, a.out), graphify.GraphFile)
		case file == "":
			file = findGraphFile(cwd)
		default:
			file = absFrom(cwd, file)
		}
		g, err := graphify.LoadJSON(file)
		if errors.Is(err, graphify.ErrNoGraph) {
			return "No graph yet: " + file + " does not exist. Build it with `gocode graphify` (or /graphify) first.", 1
		}
		if err != nil {
			return "gocode graphify: cannot read " + file + ": " + err.Error(), 1
		}
		var out string
		var code int
		switch verb {
		case "query":
			out, code = g.Query(rest[0], a.depth, a.budget)
		case "path":
			out, code = g.Path(rest[0], rest[1])
		default:
			out, code = g.Explain(rest[0])
		}
		// A graph on disk is a snapshot: say so when the code moved on. The
		// tree is known only for the default layout (<root>/graphify-out).
		if dir := filepath.Dir(file); filepath.Base(dir) == graphify.OutDirName {
			if n, example := graphify.Stale(ctx, filepath.Dir(dir), file); n > 0 {
				out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n\nNote: %d source file(s) changed since this graph was built (e.g. %s), so the answer may be out of date. Run `gocode graphify` to refresh it.", n, example)
			}
		}
		return out, code
	}

	// Build. "update" is the graphify verb for an incremental rebuild, which
	// every build here is.
	pos := a.pos
	if verb == "update" {
		pos = pos[1:]
	}
	if len(pos) > 1 {
		return "gocode graphify: expected one directory, got " + strings.Join(pos, " ") + "\n\n" + graphifyUsage, 2
	}
	root := cwd
	if len(pos) == 1 {
		root = pos[0]
		if !filepath.IsAbs(root) {
			root = filepath.Join(cwd, root)
		}
	}
	res, err := graphify.Run(ctx, graphify.Options{
		Root: root, Force: a.force, ClusterOnly: a.clusterOnly, Resolution: a.resolution,
		NoViz: a.noViz, NodeLimit: a.nodeLimit, NoGitignore: a.noGitignore,
		Generator: "gocode " + updateCurrentVersion(),
		OutDir:    absFrom(cwd, a.out),
	})
	if err != nil {
		return "gocode graphify: " + err.Error(), 1
	}
	return graphifySummary(cwd, res), 0
}

// absFrom makes a command-line path absolute against the working directory
// ("" stays "").
func absFrom(cwd, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(cwd, p)
}

// findGraphFile is graphify-out/graph.json in dir or its nearest parent that
// has one; when none has, the path it would have in dir.
func findGraphFile(dir string) string {
	first := filepath.Join(dir, graphify.OutDirName, graphify.GraphFile)
	for d := dir; ; {
		file := filepath.Join(d, graphify.OutDirName, graphify.GraphFile)
		if _, err := os.Stat(file); err == nil {
			return file
		}
		parent := filepath.Dir(d)
		if parent == d {
			return first
		}
		d = parent
	}
}

// graphifySummary is the build's report to the user.
func graphifySummary(cwd string, res *graphify.Result) string {
	var b strings.Builder
	rel := func(p string) string {
		if r, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	g, a := res.Graph, res.Analysis
	if d := res.Detection; d != nil {
		fmt.Fprintf(&b, "Read %d code files and %d documents", len(d.Files[graphify.CatCode]), len(d.Files[graphify.CatDocument]))
		fmt.Fprintf(&b, " (%d parsed, %d unchanged from cache", res.Stats.Extracted, res.Stats.Cached)
		if res.Stats.Skipped > 0 {
			fmt.Fprintf(&b, ", %d skipped", res.Stats.Skipped)
		}
		b.WriteString(").\n")
		if n := len(d.Files[graphify.CatUnparsed]); n > 0 {
			fmt.Fprintf(&b, "Not parsed: %d source files in languages without an extractor; they are file nodes only (see the report).\n", n)
		}
		if other := len(d.Files[graphify.CatPaper]) + len(d.Files[graphify.CatImage]) + len(d.Files[graphify.CatMedia]); other > 0 {
			fmt.Fprintf(&b, "Not read: %d PDF, image, audio or video files (no extractor for these).\n", other)
		}
		if d.Warning != "" {
			fmt.Fprintf(&b, "Warning: %s.\n", d.Warning)
		}
	} else {
		b.WriteString("Re-clustered the existing graph.\n")
	}
	fmt.Fprintf(&b, "Graph: %d nodes, %d edges, %d communities.\n", len(g.Nodes), len(g.Edges), len(a.Communities))
	if d := res.Diff; d != nil {
		if d.Empty() {
			b.WriteString("No change since the last run.\n")
		} else {
			fmt.Fprintf(&b, "Since the last run: nodes +%d -%d, edges +%d -%d.\n", len(d.NodesAdded), len(d.NodesRemoved), d.EdgesAdded, d.EdgesRemoved)
		}
	}
	if len(a.GodNodes) > 0 {
		b.WriteString("\nMost connected:\n")
		for _, n := range a.GodNodes[:min(5, len(a.GodNodes))] {
			fmt.Fprintf(&b, "  %-40s %4d edges  %s\n", n.Label, n.Degree, n.SourceFile)
		}
	}
	b.WriteString("\nWrote:\n")
	for _, w := range res.Written {
		fmt.Fprintf(&b, "  %s\n", rel(w))
	}
	if res.Drawn > 0 && res.Drawn < len(g.Nodes) {
		fmt.Fprintf(&b, "graph.html draws the %d most connected nodes; --node-limit 0 draws all %d.\n", res.Drawn, len(g.Nodes))
	}
	b.WriteString("\nNext: gocode graphify query \"<question>\" · explain \"<name>\" · path \"<A>\" \"<B>\"")
	return b.String()
}
