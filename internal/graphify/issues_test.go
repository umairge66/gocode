package graphify

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests pin behaviour for problems reported against the upstream
// graphify project (github.com/Graphify-Labs/graphify, issue numbers in the
// comments) that this implementation could share.

// #2313: a word that is a keyword in one language is an ordinary function
// name in another, so the not-a-call list is per language.
func TestKeywordsArePerLanguage(t *testing.T) {
	g := buildTree(t, map[string]string{
		"a.js": "function list() {}\nfunction match() {}\nfunction print() {}\nfunction run() {\n  list();\n  match();\n  print();\n  if (list()) { return; }\n}\n",
		"a.py": "def select():\n    pass\n\ndef use():\n    pass\n\ndef go():\n    select()\n    use()\n",
	})
	wantEdges(t, g,
		"a.js#run calls a.js#list", "a.js#run calls a.js#match", "a.js#run calls a.js#print",
		"a.py#go calls a.py#select", "a.py#go calls a.py#use",
	)
	if _, ok := g.Node("a.js#if"); ok {
		t.Error("a control keyword became a definition")
	}
}

// #437, #2460, #5229, #2315: a name that comes from outside the tree must not
// be bound to a local definition that happens to share it, and obj.method()
// is not bound to an arbitrary method of that name elsewhere in the tree.
func TestNoFalseEdgesFromSharedNames(t *testing.T) {
	g := buildTree(t, map[string]string{
		"app/handlers.py": "from requests import get\nimport flask\n\ndef fetch(url):\n    return get(url)\n\ndef make(app):\n    flask.route('/x')\n    app.save()\n",
		"lib/http.py":     "def get(url):\n    return url\n\ndef route(p):\n    return p\n",
		"lib/store.py":    "class Store:\n    def save(self):\n        pass\n",
		"web/api.ts":      "import { parse } from 'yaml';\nexport function load(s: string, db: any) {\n  db.commit();\n  return parse(s);\n}\n",
		"web/local.ts":    "export function parse(s: string) { return s; }\nexport class Tx { commit() {} }\n",
	})
	noEdges(t, g,
		"app/handlers.py#fetch calls lib/http.py#get",        // get is requests.get
		"app/handlers.py#make calls lib/http.py#route",       // flask.route
		"app/handlers.py#make calls lib/store.py#Store.save", // app.save(): unknown receiver, nothing imported
		"web/api.ts#load calls web/local.ts#parse",           // parse is yaml's
		"web/api.ts#load calls web/local.ts#Tx.commit",       // db.commit()
	)
}

// #2447, #3272, #2428, #2943, #4619, #5047: calls through an imported name,
// an imported module, an alias or a namespace import resolve exactly.
func TestCallsThroughImports(t *testing.T) {
	g := buildTree(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/util.py":     "def helper():\n    pass\n\nclass Tool:\n    @staticmethod\n    def build():\n        pass\n",
		"pkg/other.py":    "def helper():\n    pass\n",
		"main.py": "from pkg import util\nfrom pkg.util import helper as h, Tool\nimport pkg.other as o\n\n" +
			"def run():\n    util.helper()\n    h()\n    Tool.build()\n    o.helper()\n",
		"src/math.ts": "export function add(a: number, b: number) { return a + b; }\nexport function sub(a: number, b: number) { return a - b; }\n",
		"src/use.ts": "import * as m from './math';\nimport {\n  add as plus,\n  sub,\n} from './math';\n\n" +
			"export function calc() {\n  return m.add(1, 2) + plus(1, 2) + sub(2, 1);\n}\n",
	})
	wantEdges(t, g,
		"main.py imports pkg/util.py",
		"main.py imports pkg/other.py",
		"main.py#run calls pkg/util.py#helper",
		"main.py#run calls pkg/util.py#Tool.build",
		"main.py#run calls pkg/other.py#helper",
		"src/use.ts#calc calls src/math.ts#add",
		"src/use.ts#calc calls src/math.ts#sub",
	)
	for _, tr := range [][3]string{
		{"main.py#run", "calls", "pkg/util.py#helper"},
		{"main.py#run", "calls", "pkg/other.py#helper"},
		{"src/use.ts#calc", "calls", "src/math.ts#add"},
	} {
		if e, _ := edgeOf(g, tr[0], tr[1], tr[2]); e.Confidence != Extracted {
			t.Errorf("%v: confidence %s, want EXTRACTED (the import names it)", tr, e.Confidence)
		}
	}
}

// #147, #2340, #5223: tsconfig path aliases and baseUrl resolve to files.
func TestTSConfigPathAliases(t *testing.T) {
	g := buildTree(t, map[string]string{
		"tsconfig.json": `{
  // comments and trailing commas are allowed here
  "compilerOptions": {
    "baseUrl": ".",
    "paths": { "@/*": ["src/*"], "@cfg": ["${configDir}/src/config.ts"], },
  },
}`,
		"src/lib/db.ts":   "export function connect() {}\n",
		"src/config.ts":   "export function settings() {}\n",
		"src/app/main.ts": "import { connect } from '@/lib/db';\nimport { settings } from '@cfg';\nimport { connect as c2 } from 'src/lib/db';\n\nexport function start() {\n  connect();\n  settings();\n}\n",
	})
	wantEdges(t, g,
		"src/app/main.ts imports src/lib/db.ts",
		"src/app/main.ts imports src/config.ts",
		"src/app/main.ts#start calls src/lib/db.ts#connect",
		"src/app/main.ts#start calls src/config.ts#settings",
	)
}

// #3159, #1193: imports that exist only for the type checker, and import-like
// text in comments, do not make import cycles.
func TestTypeOnlyImportsAreNotCycles(t *testing.T) {
	g := buildTree(t, map[string]string{
		"a.py": "from typing import TYPE_CHECKING\nif TYPE_CHECKING:\n    import b\n\ndef fa():\n    pass\n",
		"b.py": "import a\n",
		"x.ts": "import type { Y } from './y';\n// import { z } from './z';\nexport const one = 1;\n",
		"y.ts": "import { one } from './x';\nexport interface Y { n: number }\n",
		"z.ts": "import { one } from './x';\n",
	})
	if cycles := importCycles(g); len(cycles) != 0 {
		t.Errorf("cycles = %v, want none", cycles)
	}
	wantEdges(t, g, "a.py imports b.py", "x.ts imports y.ts") // still edges, marked type-only
	if e, _ := edgeOf(g, "x.ts", "imports", "y.ts"); !e.TypeOnly {
		t.Error("import type edge is not marked type_only")
	}
	noEdges(t, g, "x.ts imports z.ts")
}

// #2110, #2419: functions that live in an object literal are definitions.
// #2624: explicit type arguments at a call site do not hide the call.
func TestObjectLiteralMethodsAndGenericCalls(t *testing.T) {
	g := buildTree(t, map[string]string{
		"repo.js": "function query(sql) {}\n\nconst repo = {\n  find(id) {\n    return query('x');\n  },\n  save: async (row) => {\n    return query('y');\n  },\n  name: 'users',\n};\n\nmodule.exports = {\n  boot() {\n    return repo.find(1);\n  },\n};\n",
		"G.cs":    "public class G\n{\n    public T Make<T>() { return default; }\n    public void Run()\n    {\n        Make<int>();\n        this.Make<string>();\n    }\n}\n",
	})
	wantEdges(t, g,
		"repo.js#repo.find calls repo.js#query",
		"repo.js#repo.save calls repo.js#query",
		"repo.js#exports.boot calls repo.js#repo.find",
		"G.cs#G.Run calls G.cs#G.Make",
	)
	if _, ok := g.Node("repo.js#repo.name"); ok {
		t.Error("a data property became a method")
	}
}

// #438, #952, #1829, #3407, #3726, #4281, #5251, #5291: definitions that share
// a file stem, a name in different case, an overloaded name or a punctuated
// name stay distinct nodes.
func TestNodeIdentityIsNeverMerged(t *testing.T) {
	g := buildTree(t, map[string]string{
		"a/layout.py": "def render():\n    pass\n",
		"b/layout.py": "def render():\n    pass\n",
		"b/layout.ts": "export function render() {}\nexport interface Foo {}\nexport function foo() {}\n",
		"O.java":      "public class O {\n    void put(int a) {\n    }\n    void put(String a) {\n    }\n}\n",
		"m.rb":        "class M\n  def get\n  end\n  def get!\n  end\n  def _get\n  end\nend\n",
	})
	for _, id := range []string{
		"a/layout.py#render", "b/layout.py#render", "b/layout.ts#render", "b/layout.ts#Foo", "b/layout.ts#foo",
		"O.java#O.put", "O.java#O.put@L4", "m.rb#M.get", "m.rb#M.get!", "m.rb#M._get",
	} {
		if _, ok := g.Node(id); !ok {
			t.Errorf("node %s is missing (merged into another?)", id)
		}
	}
}

// #2479: a directory is skipped as dependencies or build output only when a
// marker says it is one; #728, #4425: minified bundles are not parsed.
func TestGeneratedDirectoriesAndBundles(t *testing.T) {
	long := "var a=1;" + strings.Repeat("function x(){return y()};", 200)
	root := writeTree(t, map[string]string{
		"build/real.py":             "def real():\n    pass\n", // a source directory named build
		"vendor/mine.go":            "package vendor\n",        // no modules.txt: project code
		"go/vendor/modules.txt":     "# x\n",
		"go/vendor/dep/dep.go":      "package dep\n",
		"rs/Cargo.toml":             "[package]\n",
		"rs/target/debug/gen.rs":    "fn gen() {}\n",
		"rs/src/main.rs":            "fn main() {}\n",
		"web/package.json":          "{}",
		"web/dist/bundle.js":        "function shipped() {}\n",
		"web/src/app.js":            "function app() {}\n",
		"web/src/vendor.min.js":     long,
		"py/venv/pyvenv.cfg":        "home = /usr\n",
		"py/venv/lib/site/thing.py": "def thing():\n    pass\n",
	})
	det, err := Detect(context.Background(), root, DetectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(det.Files[CatCode], " ")
	if want := "build/real.py rs/src/main.rs vendor/mine.go web/src/app.js web/src/vendor.min.js"; got != want {
		t.Errorf("code files:\n got %s\nwant %s", got, want)
	}
	fes, stats, err := Extract(context.Background(), root, det.Files[CatCode], ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 || len(fes) != 4 {
		t.Errorf("minified bundle not skipped: %+v, %d files", stats, len(fes))
	}
}

// #1689, #2108, #3295, #3752: source in a language without an extractor is
// not silently absent: its files are nodes and the report counts them.
func TestUnparsedLanguagesAreVisible(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.py":       "def main():\n    pass\n",
		"api/svc.proto": "service S {}\n",
		"lib/calc.ex":   "defmodule Calc do\nend\n",
		"README.md":     "# App\n\nThe API is in [svc](api/svc.proto).\n",
	})
	res, err := Run(context.Background(), Options{Root: root, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	wantEdges(t, res.Graph, "README.md#app references api/svc.proto")
	if n, ok := res.Graph.Node("lib/calc.ex"); !ok || n.Kind != KindFile || n.FileType != "code" {
		t.Errorf("unparsed file node = %+v", n)
	}
	report, _ := os.ReadFile(filepath.Join(res.OutDir, ReportFile))
	if !strings.Contains(string(report), "Not parsed: 2 source files") || !strings.Contains(string(report), ".ex 1") {
		t.Errorf("report does not say what was not parsed:\n%s", report)
	}
}

// #1667, #2817, #3407, #3643, #4942: the same tree gives the same bytes, one
// node or link per line; #2259, #2625, #2628: no absolute or host paths.
func TestOutputIsDeterministicAndRelocatable(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/app\n", "README.md": "# Doc\n\nSee `Start`.\n"}
	for _, d := range []string{"a", "b", "c", "d"} {
		files[d+"/x.go"] = "package " + d + "\n\ntype T struct{}\n\nfunc (t *T) Start() { helper() }\n\nfunc helper() {}\n"
		files[d+"/y.py"] = "import os\n\nclass K:\n    def run(self):\n        self.stop()\n    def stop(self):\n        pass\n"
	}
	var outputs [2][]byte
	for i := range outputs {
		root := writeTree(t, files)
		for range 2 { // a cached second run must match too
			res, err := Run(context.Background(), Options{Root: root, Generator: "test", NoViz: true})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(res.OutDir, GraphFile))
			report, _ := os.ReadFile(filepath.Join(res.OutDir, ReportFile))
			for _, out := range [][]byte{data, report} {
				if bytes.Contains(out, []byte(root)) || bytes.Contains(out, []byte(`\\`)) {
					t.Fatalf("output embeds a host path or a backslash separator")
				}
			}
			if outputs[i] != nil && !bytes.Equal(outputs[i], data) {
				t.Fatal("a cached run produced a different graph.json")
			}
			outputs[i] = data
		}
	}
	if !bytes.Equal(outputs[0], outputs[1]) {
		t.Error("two builds of the same tree in different places differ")
	}
	lines := bytes.Split(outputs[0], []byte("\n"))
	if len(lines) < 40 || !bytes.HasPrefix(lines[1], []byte(`{"id":`)) {
		t.Errorf("graph.json is not one element per line (%d lines)", len(lines))
	}
}

// #2405: a corrupt cache entry is re-extracted, not trusted or fatal.
// #3646-class: a truncated graph.json from an interrupted run is replaced.
func TestCorruptCacheAndGraphHeal(t *testing.T) {
	root := writeTree(t, map[string]string{"a.py": "def a():\n    b()\n\ndef b():\n    pass\n"})
	ctx := context.Background()
	res, err := Run(ctx, Options{Root: root, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(res.OutDir, cacheDir)
	entries, _ := os.ReadDir(cache)
	if len(entries) != 1 {
		t.Fatalf("cache entries: %d", len(entries))
	}
	for _, junk := range []string{"{not json", `{"path":"other.py","nodes":[]}`, ""} {
		if err := os.WriteFile(filepath.Join(cache, entries[0].Name()), []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(res.OutDir, GraphFile), []byte(`{"nodes":[{"id":"a.py"`), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err = Run(ctx, Options{Root: root, NoViz: true})
		if err != nil {
			t.Fatalf("cache %q: %v", junk, err)
		}
		if res.Stats.Extracted != 1 || len(res.Graph.Nodes) != 3 {
			t.Errorf("cache %q: stats %+v, %d nodes", junk, res.Stats, len(res.Graph.Nodes))
		}
		if _, err := LoadJSON(filepath.Join(res.OutDir, GraphFile)); err != nil {
			t.Errorf("graph.json not rewritten: %v", err)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(res.OutDir, ".*.tmp*")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// #5086, #5123, #4809, #2130, #2143, #2265: a malformed or hostile graph.json
// loads without a crash and without edges to nodes that do not exist.
func TestLoadMalformedGraph(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "graph.json")
	junk := `{"nodes":[{"id":"a","label":"A","community":-7},{"id":"a","label":"dup"},{"label":null},null,{"id":"b","community":999999}],
	 "links":[{"source":"a","target":"ghost","relation":"calls"},{"source":"a","target":"b","confidence":42},null,{"source":"a","target":"a"}]}`
	if err := os.WriteFile(file, []byte(junk), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadJSON(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, e := range g.Edges {
		if _, ok := g.Node(e.Source); !ok {
			t.Errorf("edge from a missing node: %+v", e)
		}
		if _, ok := g.Node(e.Target); !ok {
			t.Errorf("edge to a missing node: %+v", e)
		}
	}
	g.Explain("a")
	g.Path("a", "b")
	g.Query("dup label", 3, 100)
	Analyze(g, Cluster(g, 1))
	if _, err := WriteHTML(filepath.Join(dir, "g.html"), g, "t", 0); err != nil {
		t.Errorf("html: %v", err)
	}
}

// #4124: a label cannot break out of the page's script or markup.
// #2527: the page loads nothing from the network.
func TestHTMLIsSelfContainedAndInert(t *testing.T) {
	g := buildTree(t, map[string]string{
		"README.md": "# </script><script>alert(1)</script> <!-- x\n\ntext\n\n## <img src=x onerror=alert(2)>\n",
	})
	Analyze(g, Cluster(g, 1))
	file := filepath.Join(t.TempDir(), "g.html")
	if _, err := WriteHTML(file, g, "</title><script>alert(3)</script>", 0); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(file)
	if n := strings.Count(string(page), "<script"); n != 1 {
		t.Errorf("%d <script tags in the page, want only the viewer's own", n)
	}
	for _, bad := range []string{"alert(1)</script>", "<img src=x", "<!-- x", "http://", "https://"} {
		if strings.Contains(string(page), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}

// #3147: names in the report are code spans, so their underscores and
// asterisks are not Markdown formatting.
func TestReportNamesAreCodeSpans(t *testing.T) {
	root := writeTree(t, map[string]string{
		"my_pkg/do_work.py": "def run_all_jobs():\n    load_job_list()\n    save_job_list()\n\ndef load_job_list():\n    pass\n\ndef save_job_list():\n    load_job_list()\n",
	})
	res, err := Run(context.Background(), Options{Root: root, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := os.ReadFile(filepath.Join(res.OutDir, ReportFile))
	for _, line := range strings.Split(string(report), "\n") {
		outside := ""
		for i, part := range strings.Split(line, "`") {
			if i%2 == 0 {
				outside += part
			}
		}
		if strings.Contains(outside, "_job_") || strings.Contains(outside, "my_pkg") {
			t.Errorf("name outside a code span: %s", line)
		}
	}
}

// #2381, #2384: a test of the thing does not take the place of the thing.
// #1969: an ambiguous name is reported, not silently resolved.
// #2672: "not in the graph" is not presented as "not in the code".
// #3307: edges cite where they are.
func TestQueryBehaviour(t *testing.T) {
	g := buildTree(t, map[string]string{
		"go.mod":             "module example.com/app\n",
		"auth/login.go":      "package auth\n\nfunc Login() { verify() }\n\nfunc verify() {}\n",
		"auth/login_test.go": "package auth\n\nimport \"testing\"\n\nfunc TestLoginVerifyLoginFlow(t *testing.T) { Login() }\n",
		"other/verify.go":    "package other\n\nfunc verify() {}\n",
	})
	Analyze(g, Cluster(g, 1))
	out, code := g.Query("how does login verify", 1, 0)
	first := strings.SplitN(out[strings.Index(out, "Starting points:"):], "\n", 3)[1]
	if code != ExitOK || !strings.Contains(first, "auth/login.go") {
		t.Errorf("first seed is not the definition:\n%s", out)
	}
	if !strings.Contains(out, "Login() --calls--> verify()  [EXTRACTED]  auth/login.go:L3") {
		t.Errorf("edge has no location:\n%s", out)
	}
	if out, _ := g.Query("which test covers login", 1, 0); !strings.Contains(strings.SplitN(out[strings.Index(out, "Starting points:"):], "\n", 3)[1], "login_test.go") {
		t.Errorf("a question about tests should lead with the test:\n%s", out)
	}
	if out, code := g.Explain("verify"); code != ExitAmbiguous || !strings.Contains(out, "other/verify.go#verify") {
		t.Errorf("ambiguous explain (code %d):\n%s", code, out)
	}
	if out, code := g.Explain("nonexistent_thing"); code != ExitNoMatch || !strings.Contains(out, "does not show the code lacks it") {
		t.Errorf("no-match explain (code %d):\n%s", code, out)
	}
	for _, p := range []string{"pkg/latest.go", "src/contest.py", "manifest.json"} {
		if isTestPath(p) {
			t.Errorf("%s taken for a test file", p)
		}
	}
	for _, p := range []string{"a/b_test.go", "src/FooTest.java", "web/x.spec.ts", "tests/unit/a.py", "test_api.py"} {
		if !isTestPath(p) {
			t.Errorf("%s not taken for a test file", p)
		}
	}
}

// #1152, #1711, #1776, #2276, #2870, #2908, #3153, #3328: an update never
// leaves nodes of deleted or changed code behind, and never loses the edges
// of files that did not change.
func TestUpdateLeavesNoGhosts(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.py": "from b import shared, gone\n\ndef caller():\n    shared()\n    gone()\n",
		"b.py": "def shared():\n    pass\n\ndef gone():\n    pass\n",
		"c.py": "def lonely():\n    pass\n",
	})
	ctx := context.Background()
	if _, err := Run(ctx, Options{Root: root, NoViz: true}); err != nil {
		t.Fatal(err)
	}
	// Remove a symbol from b.py (a.py is unchanged and cached) and delete c.py.
	if err := os.WriteFile(filepath.Join(root, "b.py"), []byte("def shared():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "c.py")); err != nil {
		t.Fatal(err)
	}
	res, err := Run(ctx, Options{Root: root, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, ghost := range []string{"b.py#gone", "c.py", "c.py#lonely"} {
		if _, ok := res.Graph.Node(ghost); ok {
			t.Errorf("ghost node %s survived the update", ghost)
		}
	}
	wantEdges(t, res.Graph, "a.py#caller calls b.py#shared", "a.py imports b.py")
	if res.Stats.Extracted != 1 || res.Stats.Cached != 1 {
		t.Errorf("stats = %+v, want only b.py re-read", res.Stats)
	}
	if d := res.Diff; d == nil || len(d.NodesRemoved) != 3 {
		t.Errorf("diff = %+v, want 3 nodes removed", d)
	}
}

// #467, #536, #979, #1287, #3145: the output directory can be chosen, and is
// never scanned as input even when it lies inside the tree.
// #722, #2407: the cache is kept out of version control.
func TestCustomOutputDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{"a.py": "def a():\n    pass\n"})
	out := filepath.Join(root, "docs", "graph")
	ctx := context.Background()
	for range 2 {
		res, err := Run(ctx, Options{Root: root, OutDir: out})
		if err != nil {
			t.Fatal(err)
		}
		if res.OutDir != out || len(res.Graph.Nodes) != 2 {
			t.Fatalf("out %s, %d nodes (output scanned as input?)", res.OutDir, len(res.Graph.Nodes))
		}
	}
	if _, err := os.Stat(filepath.Join(root, OutDirName)); !os.IsNotExist(err) {
		t.Error("the default output directory was created as well")
	}
	if data, _ := os.ReadFile(filepath.Join(out, ".gitignore")); string(data) != "cache/\n" {
		t.Errorf(".gitignore = %q", data)
	}
}

// #1206, #189, #1043, #2773: nested .gitignore files are honoured, and a
// .graphifyignore pattern can pick one file type or one nested path.
func TestIgnoreFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":         "top_ignored.py\n",
		"top_ignored.py":     "def x(): pass\n",
		"keep.py":            "def k(): pass\n",
		"sub/.gitignore":     "local_only.py\n",
		"sub/local_only.py":  "def y(): pass\n",
		"sub/kept.py":        "def z(): pass\n",
		"sub/notes.md":       "# n\n",
		"deep/a/b/secret.py": "def s(): pass\n",
		"deep/a/b/fine.py":   "def f(): pass\n",
		".graphifyignore":    "*.md\ndeep/a/b/secret.py\n",
	})
	// ListPaths reads .gitignore only inside a git work tree.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := func() string {
		det, err := Detect(context.Background(), root, DetectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(det.Readable(), " ")
	}()
	if want := "deep/a/b/fine.py keep.py sub/kept.py"; g != want {
		t.Errorf("files:\n got %s\nwant %s", g, want)
	}
}

// #1699, #2023, #4296, #5242, #5304, #5355 (Go part): a method call binds to
// the method of the value's declared type, however many types share the
// method's name, and never to a tree method when the type is from outside.
func TestGoTypedReceivers(t *testing.T) {
	g := buildTree(t, map[string]string{
		"go.mod": "module example.com/app\n",
		"store/user.go": `package store

type UserRepo struct{}

func (r *UserRepo) Save() {}
func (r *UserRepo) Close() {}

func NewUserRepo() *UserRepo { return &UserRepo{} }

type OrderRepo struct{ Base }

func (r *OrderRepo) Save() {}

type Base struct{}

func (b Base) Audit() {}
`,
		"app/svc.go": `package app

import (
	"os"

	"example.com/app/store"
)

type Service struct {
	users  *store.UserRepo
	orders store.OrderRepo
}

var shared = store.NewUserRepo()

func (s *Service) Run(extra *store.OrderRepo, f *os.File) {
	s.users.Save()
	s.orders.Save()
	extra.Save()
	extra.Audit()
	local := store.NewUserRepo()
	local.Save()
	var again store.OrderRepo
	again.Save()
	shared.Save()
	f.Close()
	go func(r *store.UserRepo) { r.Save() }(local)
}

func Fact(n int) int {
	if n < 2 {
		return 1
	}
	return n * Fact(n-1)
}
`,
	})
	run := "app/svc.go#Service.Run"
	wantEdges(t, g,
		run+" calls store/user.go#UserRepo.Save",
		run+" calls store/user.go#OrderRepo.Save",
		run+" calls store/user.go#Base.Audit",   // promoted through the embedded Base
		"app/svc.go#Fact calls app/svc.go#Fact", // #3350: recursion is an edge
	)
	for _, target := range []string{"store/user.go#UserRepo.Save", "store/user.go#OrderRepo.Save", "store/user.go#Base.Audit"} {
		if e, _ := edgeOf(g, run, "calls", target); e.Confidence != Extracted {
			t.Errorf("%s: %s, want EXTRACTED", target, e.Confidence)
		}
	}
	// f is an *os.File: its Close is not UserRepo.Close, the only Close here.
	noEdges(t, g, run+" calls store/user.go#UserRepo.Close")
	out, _ := g.Explain("Fact")
	if !strings.Contains(out, "itself (recursive)") {
		t.Errorf("explain does not show the recursion:\n%s", out)
	}
	if g.Degree(g.index["app/svc.go#Fact"]) != 2 { // contains + the recursive call, counted once
		t.Errorf("degree of a recursive function = %d", g.Degree(g.index["app/svc.go#Fact"]))
	}
}

// #2841: a graph on disk that is older than the code says so.
func TestStaleGraphIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{"a.py": "def a():\n    pass\n", "b.py": "def b():\n    pass\n"})
	ctx := context.Background()
	res, err := Run(ctx, Options{Root: root, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(res.OutDir, GraphFile)
	if n, _ := Stale(ctx, root, file); n != 0 {
		t.Errorf("fresh graph reported stale (%d files)", n)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "b.py"), later, later); err != nil {
		t.Fatal(err)
	}
	if n, example := Stale(ctx, root, file); n != 1 || example != "b.py" {
		t.Errorf("stale = %d (%s), want 1 (b.py)", n, example)
	}
}

// .graphifyignore takes full gitignore syntax, re-inclusion with "!" too.
func TestGraphifyIgnoreNegation(t *testing.T) {
	root := writeTree(t, map[string]string{
		".graphifyignore": "# all generated code except the one hand-written file\n/gen/*\n!/gen/keep.py\n*.md\n!README.md\n",
		"gen/a.py":        "def a(): pass\n",
		"gen/keep.py":     "def k(): pass\n",
		"src/gen/b.py":    "def b(): pass\n", // "/gen/*" is anchored at the root
		"notes.md":        "# n\n",
		"README.md":       "# r\n",
	})
	det, err := Detect(context.Background(), root, DetectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(det.Readable(), " "), "gen/keep.py src/gen/b.py README.md"; got != want {
		t.Errorf("files:\n got %s\nwant %s", got, want)
	}
}

// #1699, #2023, #2041, #4296, #5355, #1682, #4294 (pattern-read languages): a
// method called on a variable binds to the method of the variable's declared
// type even when other types have a method of that name, and a variable
// whose type comes from outside the tree binds to nothing.
func TestTypedReceiversInOtherLanguages(t *testing.T) {
	g := buildTree(t, map[string]string{
		// TypeScript: typed parameter, typed field, new.
		"ts/repos.ts": "export class UserRepo {\n  save() {\n  }\n}\nexport class OrderRepo {\n  save() {\n  }\n}\n",
		"ts/svc.ts":   "import { UserRepo, OrderRepo } from './repos';\nimport { Client } from 'pg';\n\nexport class Svc {\n  private orders: OrderRepo;\n  run(users: UserRepo, db: Client) {\n    users.save();\n    this.orders.save();\n    const again = new UserRepo();\n    again.save();\n    db.save();\n  }\n}\n",
		// Python: annotation, constructor call, attribute set in __init__.
		"py/repos.py": "class UserRepo:\n    def save(self):\n        pass\n\nclass OrderRepo:\n    def save(self):\n        pass\n",
		"py/svc.py":   "from repos import UserRepo, OrderRepo\nimport sqlite3\n\nclass Svc:\n    def __init__(self):\n        self.orders = OrderRepo()\n\n    def run(self, users: UserRepo, conn: sqlite3.Connection):\n        users.save()\n        self.orders.save()\n        local = UserRepo()\n        local.save()\n        conn.save()\n",
		// Java: Type name declarations.
		"jv/UserRepo.java":  "public class UserRepo {\n    public void save() {\n    }\n}\n",
		"jv/OrderRepo.java": "public class OrderRepo {\n    public void save() {\n    }\n}\n",
		"jv/Svc.java":       "import java.util.List;\n\npublic class Svc {\n    private final OrderRepo orders = new OrderRepo();\n\n    public void run(UserRepo users, List<String> names) {\n        users.save();\n        orders.save();\n        names.save();\n    }\n}\n",
		// Kotlin, Rust, PHP, Ruby.
		"kt/Repo.kt":   "class UserRepo {\n    fun save() {}\n}\n\nclass OrderRepo {\n    fun save() {}\n}\n\nfun run(users: UserRepo) {\n    users.save()\n    val o = OrderRepo()\n    o.save()\n}\n",
		"rs/repo.rs":   "pub struct UserRepo;\npub struct OrderRepo;\n\nimpl UserRepo {\n    pub fn save(&self) {}\n}\n\nimpl OrderRepo {\n    pub fn new() -> Self { OrderRepo }\n    pub fn save(&self) {}\n}\n\npub fn run(users: &UserRepo) {\n    users.save();\n    let o = OrderRepo::new();\n    o.save();\n}\n",
		"php/Repo.php": "<?php\nclass UserRepo {\n    public function save() {}\n}\nclass OrderRepo {\n    public function save() {}\n}\nfunction run(UserRepo $users) {\n    $users->save();\n    $o = new OrderRepo();\n    $o->save();\n}\n",
		"rb/repo.rb":   "class UserRepo\n  def save\n  end\nend\n\nclass OrderRepo\n  def save\n  end\nend\n\ndef run\n  o = OrderRepo.new\n  o.save()\nend\n",
	})
	wantEdges(t, g,
		"ts/svc.ts#Svc.run calls ts/repos.ts#UserRepo.save",
		"ts/svc.ts#Svc.run calls ts/repos.ts#OrderRepo.save",
		"py/svc.py#Svc.run calls py/repos.py#UserRepo.save",
		"py/svc.py#Svc.run calls py/repos.py#OrderRepo.save",
		"jv/Svc.java#Svc.run calls jv/UserRepo.java#UserRepo.save",
		"jv/Svc.java#Svc.run calls jv/OrderRepo.java#OrderRepo.save",
		"kt/Repo.kt#run calls kt/Repo.kt#UserRepo.save",
		"kt/Repo.kt#run calls kt/Repo.kt#OrderRepo.save",
		"rs/repo.rs#run calls rs/repo.rs#UserRepo.save",
		"rs/repo.rs#run calls rs/repo.rs#OrderRepo.save",
		"php/Repo.php#run calls php/Repo.php#UserRepo.save",
		"php/Repo.php#run calls php/Repo.php#OrderRepo.save",
		"rb/repo.rb#run calls rb/repo.rb#OrderRepo.save",
	)
	for _, tr := range [][2]string{
		{"ts/svc.ts#Svc.run", "ts/repos.ts#UserRepo.save"}, {"py/svc.py#Svc.run", "py/repos.py#OrderRepo.save"},
		{"kt/Repo.kt#run", "kt/Repo.kt#UserRepo.save"}, {"rs/repo.rs#run", "rs/repo.rs#OrderRepo.save"},
	} {
		if e, _ := edgeOf(g, tr[0], "calls", tr[1]); e.Confidence != Extracted {
			t.Errorf("%v: %s, want EXTRACTED", tr, e.Confidence)
		}
	}
	// db: Client (pg), conn: sqlite3.Connection, names: List<String> are not
	// tree types; rb's run has only an OrderRepo.
	for _, e := range g.Edges {
		if e.Relation != "calls" {
			continue
		}
		n := 0
		for _, o := range g.Edges {
			if o.Relation == "calls" && o.Source == e.Source && o.SourceLocation == e.SourceLocation {
				n++
			}
		}
		if n > 1 {
			t.Errorf("one call site bound to %d methods: %s %s", n, e.Source, e.SourceLocation)
		}
	}
	for _, src := range []string{"ts/svc.ts#Svc.run", "py/svc.py#Svc.run", "jv/Svc.java#Svc.run"} {
		n := 0
		for _, e := range g.Edges {
			if e.Source == src && e.Relation == "calls" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%s has %d call edges, want 2 (the outside-typed variable binds to nothing)", src, n)
		}
	}
}

// Languages added after the upstream requests for them (#1275, #2825, #3361,
// #1230-style single-file components, Scala, Dart, Lua).
func TestAddedLanguages(t *testing.T) {
	g := buildTree(t, map[string]string{
		// Shell: functions, bare-name calls, sourced files; $# is not a comment.
		"scripts/lib.sh":   "#!/bin/sh\nlog() {\n  echo \"$1\"\n}\n\ncount_args() {\n  n=$#\n  log \"$n\"\n}\n",
		"scripts/build.sh": "#!/bin/bash\nsource \"$(dirname \"$0\")/lib.sh\"\n\nfunction compile {\n  log \"compiling\" && count_args a b\n}\n\ncompile\necho done | log\n",
		// SQL: tables, a foreign key, a view, a function; quoted names.
		"db/schema.sql": "-- users and orders\nCREATE TABLE [dbo].[users] (\n  id int primary key\n);\n\nCREATE TABLE IF NOT EXISTS \"public\".\"orders\" (\n  id int,\n  user_id int REFERENCES users(id)\n);\n",
		"db/views.sql":  "CREATE OR REPLACE VIEW order_totals AS\n  SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id;\n\nCREATE FUNCTION total_for(uid int) RETURNS int AS $$\nBEGIN\n  RETURN (SELECT count(*) FROM order_totals);\nEND;\n$$ LANGUAGE plpgsql;\n\nCREATE FUNCTION report() RETURNS int AS $$\n  SELECT total_for(1);\n$$ LANGUAGE sql;\n",
		// Scala.
		"sc/Shapes.scala": "package demo\n\ntrait Shape {\n  def area(): Double\n}\n\nclass Circle(r: Double) extends Shape with Serializable {\n  def area(): Double = scale(r)\n  private def scale(x: Double): Double = x * 3.14\n}\n\nobject Main {\n  def run(): Double = {\n    val c = new Circle(2.0)\n    c.area()\n  }\n}\n",
		// Dart.
		"lib/repo.dart": "class Repo {\n  int load(int id) {\n    return id;\n  }\n}\n",
		"lib/app.dart":  "import 'repo.dart';\nimport 'dart:core';\n\nclass App extends Base implements Runner {\n  final Repo repo = Repo();\n\n  int start() {\n    return repo.load(1) + helper();\n  }\n}\n\nint helper() {\n  return 2;\n}\n",
		// Lua.
		"lua/util.lua": "local M = {}\n\nfunction M.trim(s)\n  return s\nend\n\nlocal function pad(s)\n  return M.trim(s)\nend\n\nM.fmt = function(s)\n  return pad(s) -- pad(), not a call: other()\nend\n\nreturn M\n",
		"lua/main.lua": "local util = require(\"util\")\n\nlocal function main()\n  return util.trim(\"x\")\nend\n",
		// Vue single-file component.
		"web/api.ts":   "export function fetchUser(id: number) { return id; }\n",
		"web/Card.vue": "<template>\n  <div @click=\"load()\">{{ name }}</div>\n</template>\n\n<script setup lang=\"ts\">\nimport { fetchUser } from './api';\n\nfunction load() {\n  return fetchUser(1);\n}\n</script>\n\n<style>\n.a { color: red; }\n</style>\n",
	})
	wantEdges(t, g,
		"scripts/build.sh imports scripts/lib.sh",
		"scripts/build.sh#compile calls scripts/lib.sh#log",
		"scripts/build.sh#compile calls scripts/lib.sh#count_args",
		"scripts/build.sh calls scripts/build.sh#compile",
		"scripts/build.sh calls scripts/lib.sh#log",
		"scripts/lib.sh#count_args calls scripts/lib.sh#log",

		"db/schema.sql#orders references db/schema.sql#users",
		"db/views.sql#order_totals uses db/schema.sql#users",
		"db/views.sql#order_totals uses db/schema.sql#orders",
		"db/views.sql#total_for uses db/views.sql#order_totals",
		"db/views.sql#report calls db/views.sql#total_for",

		"sc/Shapes.scala#Circle inherits sc/Shapes.scala#Shape",
		"sc/Shapes.scala#Circle.area calls sc/Shapes.scala#Circle.scale",
		"sc/Shapes.scala#Main.run uses sc/Shapes.scala#Circle",
		"sc/Shapes.scala#Main.run calls sc/Shapes.scala#Circle.area",

		"lib/app.dart imports lib/repo.dart",
		"lib/app.dart#App.start calls lib/repo.dart#Repo.load",
		"lib/app.dart#App.start calls lib/app.dart#helper",

		"lua/main.lua imports lua/util.lua",
		"lua/main.lua#main calls lua/util.lua#M.trim",
		"lua/util.lua#pad calls lua/util.lua#M.trim",
		"lua/util.lua#M.fmt calls lua/util.lua#pad",

		"web/Card.vue imports web/api.ts",
		"web/Card.vue#load calls web/api.ts#fetchUser",
	)
	if n, ok := g.Node("web/Card.vue#load"); !ok || n.SourceLocation != "L8" {
		t.Errorf("vue function node = %+v, want line 8 of the component", n)
	}
	if n, ok := g.Node("db/schema.sql#users"); !ok || n.Label != "users" || n.Kind != KindType {
		t.Errorf("bracket-quoted table = %+v", n)
	}
	for _, e := range g.Edges {
		if strings.HasSuffix(e.Target, "#other") || strings.HasSuffix(e.Target, "#echo") {
			t.Errorf("unexpected edge %+v", e)
		}
	}
}
