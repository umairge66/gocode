package graphify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates the files (path -> content) under a new temp directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// buildTree runs detect, extract, resolve and build on the files.
func buildTree(t *testing.T, files map[string]string) *Graph {
	t.Helper()
	root := writeTree(t, files)
	det, err := Detect(context.Background(), root, DetectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	paths := append(append([]string(nil), det.Files[CatCode]...), det.Files[CatDocument]...)
	fes, _, err := Extract(context.Background(), root, paths, ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nodes, edges := Resolve(root, fes)
	return Build(fes, nodes, edges)
}

// wantEdges fails the test for every "source relation target" triple (node
// ids) the graph lacks.
func wantEdges(t *testing.T, g *Graph, triples ...string) {
	t.Helper()
	have := map[string]Edge{}
	for _, e := range g.Edges {
		have[e.Source+" "+e.Relation+" "+e.Target] = e
	}
	for _, tr := range triples {
		if _, ok := have[tr]; !ok {
			var all []string
			for k := range have {
				all = append(all, k)
			}
			t.Errorf("missing edge %q\nhave:\n  %s", tr, strings.Join(sortedStrings(all), "\n  "))
		}
	}
}

func noEdges(t *testing.T, g *Graph, triples ...string) {
	t.Helper()
	for _, e := range g.Edges {
		for _, tr := range triples {
			if e.Source+" "+e.Relation+" "+e.Target == tr {
				t.Errorf("unexpected edge %q", tr)
			}
		}
	}
}

func sortedStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func edgeOf(g *Graph, src, rel, tgt string) (Edge, bool) {
	for _, e := range g.Edges {
		if e.Source == src && e.Relation == rel && e.Target == tgt {
			return e, true
		}
	}
	return Edge{}, false
}

func TestGoExtraction(t *testing.T) {
	g := buildTree(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"main.go": `package main

import (
	"fmt"
	st "example.com/app/store"
)

type Server struct {
	db *st.DB
	Base
}

type Base struct{}

func (s *Server) Start() { s.listen(); s.db.Put("k"); fmt.Println("x") }
func (s *Server) listen() {}

func main() {
	srv := &Server{db: st.Open()}
	srv.Start()
	helper()
}

func helper() {}
`,
		"store/db.go": `package store

type DB struct{}

func Open() *DB { return &DB{} }

func (d *DB) Put(key string) {}
`,
	})
	wantEdges(t, g,
		"pkg:. contains main.go",
		"pkg:store contains store/db.go",
		"main.go imports pkg:store",
		"main.go contains main.go#Server",
		"main.go#Server method main.go#Server.Start",
		"main.go#Server uses store/db.go#DB",
		"main.go#Server embeds main.go#Base",
		"main.go#Server.Start calls main.go#Server.listen", // receiver call: exact
		"main.go#Server.Start calls store/db.go#DB.Put",    // s.db.Put: through the declared type of the field
		"main.go#main calls store/db.go#Open",
		"main.go#main calls main.go#helper",
		"main.go#main uses main.go#Server",
		"main.go#main calls main.go#Server.Start", // srv.Start: unique method in the package
		"store/db.go#Open uses store/db.go#DB",
		"store/db.go#DB method store/db.go#DB.Put",
	)
	if e, _ := edgeOf(g, "main.go#Server.Start", "calls", "main.go#Server.listen"); e.Confidence != Extracted {
		t.Errorf("receiver call confidence = %s, want EXTRACTED", e.Confidence)
	}
	if e, _ := edgeOf(g, "main.go#Server.Start", "calls", "store/db.go#DB.Put"); e.Confidence != Extracted {
		t.Errorf("s.db.Put edge = %+v, want EXTRACTED (the field's type is declared)", e)
	}
	if e, _ := edgeOf(g, "main.go#main", "calls", "main.go#Server.Start"); e.Confidence != Extracted {
		t.Errorf("srv.Start edge = %+v, want EXTRACTED (srv := &Server{...})", e)
	}
	// fmt.Println is outside the tree and must not be matched to anything.
	for _, e := range g.Edges {
		if strings.Contains(e.Target, "Println") {
			t.Errorf("edge to an external function: %+v", e)
		}
	}
	if n, ok := g.Node("main.go#Server.Start"); !ok || n.Label != "Server.Start()" || n.SourceLocation != "L15" || n.Kind != KindMethod {
		t.Errorf("method node = %+v", n)
	}
}

func TestPythonExtraction(t *testing.T) {
	g := buildTree(t, map[string]string{
		"app/__init__.py": "",
		"app/models.py": `class Base:
    def save(self):
        pass

class User(Base):
    """A user. Not a call: fake()"""
    def rename(self, name):
        self.save()
        validate(name)  # not a call: other()

def validate(name):
    return len(name) > 0
`,
		"app/service.py": `from .models import User, validate
from app import models

def create(name):
    u = User()
    u.rename(name)
    validate(name)
    return helper(u)

def helper(u):
    return u
`,
	})
	wantEdges(t, g,
		"app/models.py#User inherits app/models.py#Base",
		"app/models.py#Base method app/models.py#Base.save",
		"app/models.py#User.rename calls app/models.py#validate",
		"app/service.py imports app/models.py",
		"app/service.py#create uses app/models.py#User",
		"app/service.py#create calls app/models.py#User.rename",
		"app/service.py#create calls app/models.py#validate",
		"app/service.py#create calls app/service.py#helper",
	)
	for _, e := range g.Edges {
		if strings.HasSuffix(e.Target, "#fake") || strings.HasSuffix(e.Target, "#other") {
			t.Errorf("call found inside a string or comment: %+v", e)
		}
	}
	if _, ok := g.Node("app/models.py#validate"); !ok {
		t.Error("module-level function after a class was not recognized as a function")
	}
}

func TestJavaScriptExtraction(t *testing.T) {
	g := buildTree(t, map[string]string{
		"src/util.ts": `export function slugify(s: string): string {
  return s.toLowerCase();
}
export const double = (n: number) => n * 2;
export interface Shape { area(): number }
`,
		"src/app.ts": `import { slugify, double } from './util';
import { Shape } from "./util.js";

export class Circle implements Shape {
  constructor(private r: number) {}
  area(): number {
    return double(this.r);
  }
  label() {
    return slugify(` + "`circle ${this.area()}`" + `) + this.area();
  }
}

export default function main() {
  const c = new Circle(2);
  return c.label();
}
`,
	})
	wantEdges(t, g,
		"src/app.ts imports src/util.ts",
		"src/app.ts#Circle implements src/util.ts#Shape",
		"src/app.ts#Circle method src/app.ts#Circle.area",
		"src/app.ts#Circle.area calls src/util.ts#double",
		"src/app.ts#Circle.label calls src/util.ts#slugify",
		"src/app.ts#Circle.label calls src/app.ts#Circle.area",
		"src/app.ts#main uses src/app.ts#Circle",
		"src/app.ts#main calls src/app.ts#Circle.label",
	)
}

func TestBraceLanguages(t *testing.T) {
	g := buildTree(t, map[string]string{
		"src/com/x/Animal.java": `package com.x;

public interface Animal {
    String name();
}
`,
		"src/com/x/Dog.java": `package com.x;

import com.x.util.Text;

public class Dog extends Pet implements Animal {
    private final String n = "rex";

    @Override
    public String name() {
        return Text.upper(n);
    }

    public void bark() {
        System.out.println(name());
    }
}
`,
		"src/com/x/Pet.java":       "package com.x;\n\npublic abstract class Pet {\n}\n",
		"src/com/x/util/Text.java": "package com.x.util;\n\npublic class Text {\n    public static String upper(String s) {\n        return s.toUpperCase();\n    }\n}\n",

		"lib/shape.rs": `pub trait Shape {
    fn area(&self) -> f64;
}

pub struct Square { side: f64 }

impl Square {
    pub fn new(side: f64) -> Self { Square { side } }
}

impl Shape for Square {
    fn area(&self) -> f64 { square(self.side) }
}

fn square<'a>(x: f64) -> f64 { x * x }

pub fn demo() -> f64 {
    let s = Square::new(2.0);
    s.area()
}
`,
		"c/math.h": "int add(int a, int b);\nint twice(int a);\n",
		"c/math.c": `#include "math.h"

int add(int a, int b)
{
    return a + b;
}

int twice(int a) {
    return add(a, a);
}
`,
		"c/main.c": "#include \"math.h\"\n\nint main(void) {\n    return twice(2);\n}\n",
	})
	wantEdges(t, g,
		// Java
		"src/com/x/Dog.java imports src/com/x/util/Text.java",
		"src/com/x/Dog.java#Dog inherits src/com/x/Pet.java#Pet",
		"src/com/x/Dog.java#Dog implements src/com/x/Animal.java#Animal",
		"src/com/x/Dog.java#Dog.name calls src/com/x/util/Text.java#Text.upper",
		"src/com/x/Dog.java#Dog.bark calls src/com/x/Dog.java#Dog.name",
		// Rust
		"lib/shape.rs#Square implements lib/shape.rs#Shape",
		"lib/shape.rs#Square method lib/shape.rs#Square.new",
		"lib/shape.rs#Square.area calls lib/shape.rs#square",
		"lib/shape.rs#demo calls lib/shape.rs#Square.new",
		// C
		"c/math.c imports c/math.h",
		"c/math.c#twice calls c/math.c#add",
		"c/main.c#main calls c/math.c#twice",
	)
	// Prototypes in the header are declarations, not definitions.
	if _, ok := g.Node("c/math.h#add"); ok {
		t.Error("a C prototype became a definition")
	}
	// A field initializer is not a method.
	if _, ok := g.Node("src/com/x/Dog.java#Dog.n"); ok {
		t.Error("a Java field became a method")
	}
}

func TestOtherLanguages(t *testing.T) {
	g := buildTree(t, map[string]string{
		"rb/base.rb": "class Base\n  def run\n    setup()\n  end\n\n  def setup\n  end\nend\n",
		"rb/job.rb":  "require_relative 'base'\n\nclass Job < Base\n  def go\n    run()\n  end\nend\n",
		"php/Repo.php": `<?php
class Repo {
    public function find($id) {
        return $this->load($id);
    }
    private function load($id) { return $id; }
}
function boot() {
    $r = new Repo();
    return $r->find(1);
}
`,
		"kt/Greeter.kt": `package demo

interface Named { fun name(): String }

open class Greeter(val who: String) : Named {
    override fun name(): String = who
    fun greet(): String {
        return format(name())
    }
}

fun format(s: String) = "Hello, $s"
`,
		"sw/Stack.swift": `protocol Container {
    func count() -> Int
}

struct Stack: Container {
    var items: [Int] = []
    func count() -> Int { return items.count }
    mutating func push(_ x: Int) {
        items.append(x)
        log(count())
    }
}

func log(_ n: Int) {}
`,
		"cs/Shop.cs": `namespace Demo
{
    public interface IShop { decimal Total(); }

    public class Shop : IShop
    {
        public decimal Total()
        {
            return Tax(10m);
        }

        private static decimal Tax(decimal x) => x * 1.2m;
    }
}
`,
	})
	wantEdges(t, g,
		"rb/job.rb imports rb/base.rb",
		"rb/job.rb#Job inherits rb/base.rb#Base",
		"rb/base.rb#Base.run calls rb/base.rb#Base.setup",
		"rb/job.rb#Job.go calls rb/base.rb#Base.run",
		"php/Repo.php#Repo.find calls php/Repo.php#Repo.load",
		"php/Repo.php#boot uses php/Repo.php#Repo",
		"php/Repo.php#boot calls php/Repo.php#Repo.find",
		"kt/Greeter.kt#Greeter inherits kt/Greeter.kt#Named",
		"kt/Greeter.kt#Greeter.greet calls kt/Greeter.kt#format",
		"kt/Greeter.kt#Greeter.greet calls kt/Greeter.kt#Greeter.name",
		"sw/Stack.swift#Stack inherits sw/Stack.swift#Container",
		"sw/Stack.swift#Stack.push calls sw/Stack.swift#log",
		"sw/Stack.swift#Stack.push calls sw/Stack.swift#Stack.count",
		"cs/Shop.cs#Shop inherits cs/Shop.cs#IShop",
		"cs/Shop.cs#Shop.Total calls cs/Shop.cs#Shop.Tax",
	)
}

func TestMarkdownExtraction(t *testing.T) {
	g := buildTree(t, map[string]string{
		"go.mod":  "module example.com/app\n",
		"main.go": "package main\n\nfunc main() { bootstrap() }\n\nfunc bootstrap() {}\n",
		"README.md": "# App\n\nSee [the guide](docs/guide.md) and `main.go`.\n\n## Startup\n\n" +
			"Everything begins in `bootstrap()`.\n\n```\nignored `main()` in a fence\n# not a heading\n```\n",
		"docs/guide.md": "# Guide\n\nBack to [home](../README.md).\n",
	})
	wantEdges(t, g,
		"README.md contains README.md#app",
		"README.md#app contains README.md#startup",
		"README.md#app references docs/guide.md",
		"README.md#app references main.go",
		"README.md#startup mentions main.go#bootstrap",
		"docs/guide.md#guide references README.md",
	)
	if _, ok := g.Node("README.md#not-a-heading"); ok {
		t.Error("a heading inside a code fence became a section")
	}
	if n, _ := g.Node("README.md"); n == nil || n.FileType != "document" {
		t.Errorf("README node = %+v", n)
	}
}

func TestDetectIgnores(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go":                    "package a\n",
		"notes.md":                "# n\n",
		"paper.pdf":               "%PDF",
		"node_modules/x/index.js": "function x() {}\n",
		"graphify-out/old.go":     "package old\n",
		"gen/big.go":              "package gen\n",
		"data/keep.py":            "def k(): pass\n",
		"data/skip.py":            "def s(): pass\n",
		".graphifyignore":         "# generated\ngen/\ndata/skip.*\n",
		"unknown.xyz":             "?",
	})
	det, err := Detect(context.Background(), root, DetectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(det.Files[CatCode], ","); got != "a.go,data/keep.py" {
		t.Errorf("code files = %s", got)
	}
	if len(det.Files[CatDocument]) != 1 || len(det.Files[CatPaper]) != 1 || det.TotalFiles != 4 {
		t.Errorf("detection = %+v", det)
	}
}

func TestClusterSeparatesGroups(t *testing.T) {
	// Two cliques of four joined by one edge must become two communities.
	g := &Graph{}
	for _, id := range []string{"a1", "a2", "a3", "a4", "b1", "b2", "b3", "b4"} {
		g.Nodes = append(g.Nodes, Node{ID: id, Label: id, Kind: KindFunction, SourceFile: id[:1] + "/f.go"})
	}
	link := func(s, t string) {
		g.Edges = append(g.Edges, Edge{Source: s, Target: t, Relation: "calls", Confidence: Extracted})
	}
	for _, p := range []string{"a", "b"} {
		for i := 1; i <= 4; i++ {
			for j := i + 1; j <= 4; j++ {
				link(p+string(rune('0'+i)), p+string(rune('0'+j)))
			}
		}
	}
	link("a1", "b1")
	g.link()
	comms := Cluster(g, 1)
	if len(comms) != 2 || len(comms[0]) != 4 || len(comms[1]) != 4 {
		t.Fatalf("communities = %v", comms)
	}
	for _, n := range g.Nodes {
		if want := map[byte]int{'a': 0, 'b': 1}[n.ID[0]]; n.Community != want {
			t.Errorf("%s in community %d, want %d", n.ID, n.Community, want)
		}
	}
	if c := Cohesion(g, comms[0]); c != 1 {
		t.Errorf("cohesion of a clique = %v", c)
	}
	a := Analyze(g, comms)
	if len(a.Surprising) != 1 || a.Surprising[0].From != "a1" || a.Surprising[0].To != "b1" || a.Surprising[0].Links != 1 {
		t.Errorf("surprising = %+v", a.Surprising)
	}
}

func TestImportCycles(t *testing.T) {
	g := buildTree(t, map[string]string{
		"a.py": "import b\n\ndef fa():\n    pass\n",
		"b.py": "import c\n",
		"c.py": "import a\n",
		"d.py": "import a\n",
	})
	cycles := importCycles(g)
	if len(cycles) != 1 || strings.Join(cycles[0], ",") != "a.py,b.py,c.py" {
		t.Errorf("cycles = %v", cycles)
	}
}

func TestRunPipelineAndQueries(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":    "module example.com/app\n",
		"main.go":   "package main\n\nimport \"example.com/app/auth\"\n\nfunc main() { auth.Login(\"u\") }\n",
		"auth/a.go": "package auth\n\n// Login signs a user in.\nfunc Login(user string) bool { return check(user) }\n\nfunc check(user string) bool { return user != \"\" }\n",
		"auth/s.go": "package auth\n\ntype Session struct{ User string }\n\nfunc (s *Session) Valid() bool { return check(s.User) }\n",
		"README.md": "# App\n\nCall `Login` to start.\n",
		"logo.png":  "\x89PNG",
	})
	ctx := context.Background()
	res, err := Run(ctx, Options{Root: root, Generator: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Extracted != 4 || res.Stats.Cached != 0 || res.Diff != nil {
		t.Errorf("first run: stats %+v diff %+v", res.Stats, res.Diff)
	}
	for _, name := range []string{GraphFile, ReportFile, HTMLFile} {
		if info, err := os.Stat(filepath.Join(res.OutDir, name)); err != nil || info.Size() == 0 {
			t.Errorf("%s not written: %v", name, err)
		}
	}
	report, _ := os.ReadFile(filepath.Join(res.OutDir, ReportFile))
	for _, want := range []string{"## God nodes", "`check()`", "## Communities", "1 images"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	html, _ := os.ReadFile(filepath.Join(res.OutDir, HTMLFile))
	if strings.Contains(string(html), "/*DATA*/null") || !strings.Contains(string(html), `"l":"Login()"`) {
		t.Error("graph.html does not carry the graph data")
	}

	// The saved graph round-trips and answers queries.
	g, err := LoadJSON(filepath.Join(res.OutDir, GraphFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != len(res.Graph.Nodes) || len(g.Edges) != len(res.Graph.Edges) || len(g.Communities) == 0 {
		t.Errorf("loaded %d nodes %d edges, built %d and %d", len(g.Nodes), len(g.Edges), len(res.Graph.Nodes), len(res.Graph.Edges))
	}
	out, code := g.Explain("Login")
	if code != ExitOK || !strings.Contains(out, "auth/a.go:L4") || !strings.Contains(out, "check()") || !strings.Contains(out, "← mentions") {
		t.Errorf("explain Login (code %d):\n%s", code, out)
	}
	out, code = g.Path("main", "Session.Valid")
	if code != ExitOK || !strings.Contains(out, "3 hops") || !strings.Contains(out, "<--calls--") {
		t.Errorf("path (code %d):\n%s", code, out)
	}
	out, code = g.Query("how does login check the user?", 0, 0)
	if code != ExitOK || !strings.Contains(out, "Login() --calls--> check()") {
		t.Errorf("query (code %d):\n%s", code, out)
	}
	if _, code = g.Explain("a"); code != ExitAmbiguous {
		t.Errorf("ambiguous name: code %d", code)
	}
	if _, code = g.Explain("nothing-like-this"); code != ExitNoMatch {
		t.Errorf("unknown name: code %d", code)
	}
	if _, code = g.Query("zzzz qqqq", 0, 0); code != ExitNoMatch {
		t.Errorf("query without matches: code %d", code)
	}

	// Second run: everything from the cache, nothing changed.
	res, err = Run(ctx, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Cached != 4 || res.Stats.Extracted != 0 || !res.Diff.Empty() {
		t.Errorf("second run: stats %+v diff %+v", res.Stats, res.Diff)
	}

	// Change one file: only it is re-read, and the diff shows the new node.
	if err := os.WriteFile(filepath.Join(root, "auth", "s.go"), []byte("package auth\n\ntype Session struct{ User string }\n\nfunc (s *Session) Valid() bool { return check(s.User) }\n\nfunc Logout() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Run(ctx, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Extracted != 1 || res.Stats.Cached != 3 {
		t.Errorf("third run: stats %+v", res.Stats)
	}
	if res.Diff == nil || len(res.Diff.NodesAdded) != 1 || !strings.HasPrefix(res.Diff.NodesAdded[0], "Logout()") || len(res.Diff.NodesRemoved) != 0 {
		t.Errorf("diff = %+v", res.Diff)
	}
	if entries, _ := os.ReadDir(filepath.Join(res.OutDir, cacheDir)); len(entries) != 4 {
		t.Errorf("cache holds %d entries, want 4 (stale ones removed)", len(entries))
	}

	// --cluster-only keeps the graph and rewrites the outputs.
	res, err = Run(ctx, Options{Root: root, ClusterOnly: true, Resolution: 2, NoViz: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Detection != nil || len(res.Graph.Nodes) != len(g.Nodes)+1 || len(res.Written) != 2 {
		t.Errorf("cluster-only: %d nodes, wrote %v", len(res.Graph.Nodes), res.Written)
	}
	if _, err := Run(ctx, Options{Root: t.TempDir(), ClusterOnly: true}); err == nil {
		t.Error("cluster-only without a graph did not fail")
	}
}
