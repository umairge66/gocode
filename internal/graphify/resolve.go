package graphify

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// defIndex finds definitions by name.
type defIndex struct {
	byName    map[string][]Def // functions and types
	methods   map[string][]Def // methods by bare name
	qualified map[string][]Def // methods by "Type.name"
}

func newDefIndex() *defIndex {
	return &defIndex{byName: map[string][]Def{}, methods: map[string][]Def{}, qualified: map[string][]Def{}}
}

func (x *defIndex) add(d Def) {
	if d.Kind == KindMethod {
		x.methods[d.Name] = append(x.methods[d.Name], d)
		x.qualified[d.Recv+"."+d.Name] = append(x.qualified[d.Recv+"."+d.Name], d)
		return
	}
	x.byName[d.Name] = append(x.byName[d.Name], d)
}

// goPkg is the Go files of one directory.
type goPkg struct {
	dir, name string
	funcs     map[string]string // name -> node id
	types     map[string]string
	idx       *defIndex
}

type goModule struct{ modPath, dir string }

type resolver struct {
	root    string
	byPath  map[string]*FileExtraction
	byBase  map[string][]string // file base name -> paths
	byDir   map[string][]string // family + "\x00" + dir -> paths
	fileIdx map[string]*defIndex
	famIdx  map[string]*defIndex
	allIdx  *defIndex
	goPkgs  map[string]*goPkg
	goMods  []goModule
	// jsConfigs caches the tsconfig/jsconfig that governs each directory.
	jsConfigs map[string]*jsConfig
	goScopes  map[string]*goFileScope
	defs      map[string]Def // every definition by node id
	nodes     []Node
	edges     []Edge
}

// Resolve turns the references and imports of the extracted files into
// edges, now that every definition of the tree is known. It also returns the
// nodes that only exist across files (Go packages).
func Resolve(root string, files []*FileExtraction) ([]Node, []Edge) {
	r := &resolver{root: root, byPath: map[string]*FileExtraction{}, byBase: map[string][]string{}, byDir: map[string][]string{},
		fileIdx: map[string]*defIndex{}, famIdx: map[string]*defIndex{}, allIdx: newDefIndex(), goPkgs: map[string]*goPkg{}, jsConfigs: map[string]*jsConfig{}, goScopes: map[string]*goFileScope{}, defs: map[string]Def{}}
	for _, fe := range files {
		r.byPath[fe.Path] = fe
		r.byBase[path.Base(fe.Path)] = append(r.byBase[path.Base(fe.Path)], fe.Path)
		if fe.Family == "" {
			continue
		}
		dirKey := fe.Family + "\x00" + path.Dir(fe.Path)
		r.byDir[dirKey] = append(r.byDir[dirKey], fe.Path)
		own := newDefIndex()
		r.fileIdx[fe.Path] = own
		fam := r.famIdx[fe.Family]
		if fam == nil {
			fam = newDefIndex()
			r.famIdx[fe.Family] = fam
		}
		for _, d := range fe.Defs {
			own.add(d)
			fam.add(d)
			r.allIdx.add(d)
			r.defs[d.ID] = d
		}
	}
	r.indexGo(files)
	for _, fe := range files {
		switch {
		case fe.Lang == "go":
			r.resolveGo(fe)
		case fe.Family != "":
			r.resolveGeneric(fe)
		default:
			r.resolveDoc(fe)
		}
	}
	return r.nodes, r.edges
}

func (r *resolver) edge(fe *FileExtraction, ref Ref, target, relation, conf string, score float64) {
	src := ref.From
	if relation == "method" {
		src, target = target, src
	}
	// A definition that calls itself is recursion, worth an edge when the
	// source states it; any other self-reference is a resolution accident.
	if src == target && (relation != "calls" || conf != Extracted) {
		return
	}
	e := Edge{Source: src, Target: target, Relation: relation, Confidence: conf, SourceFile: fe.Path, SourceLocation: loc(ref.Line)}
	if conf == Inferred {
		e.ConfidenceScore = score
	}
	r.edges = append(r.edges, e)
}

// ---- Go ----

func (r *resolver) indexGo(files []*FileExtraction) {
	names := map[string]map[string]int{}
	var dirs []string
	for _, fe := range files {
		if fe.Lang != "go" {
			continue
		}
		dir := path.Dir(fe.Path)
		p := r.goPkgs[dir]
		if p == nil {
			p = &goPkg{dir: dir, funcs: map[string]string{}, types: map[string]string{}, idx: newDefIndex()}
			r.goPkgs[dir] = p
			names[dir] = map[string]int{}
			dirs = append(dirs, dir)
		}
		if fe.Package != "" {
			names[dir][strings.TrimSuffix(fe.Package, "_test")]++
		}
		for _, d := range fe.Defs {
			p.idx.add(d)
			switch d.Kind {
			case KindFunction:
				if _, dup := p.funcs[d.Name]; !dup {
					p.funcs[d.Name] = d.ID
				}
			case KindType:
				if _, dup := p.types[d.Name]; !dup {
					p.types[d.Name] = d.ID
				}
			}
		}
	}
	sort.Strings(dirs)
	seenMod := map[string]bool{}
	for _, dir := range dirs {
		p := r.goPkgs[dir]
		best := 0
		for n, c := range names[dir] {
			if c > best || (c == best && n < p.name) {
				p.name, best = n, c
			}
		}
		// The module that owns this directory: the nearest go.mod above it.
		for d := dir; ; d = path.Dir(d) {
			if !seenMod[d] {
				seenMod[d] = true
				if mod := readModulePath(filepath.Join(r.root, filepath.FromSlash(d), "go.mod")); mod != "" {
					r.goMods = append(r.goMods, goModule{modPath: mod, dir: d})
				}
			}
			if d == "." || d == "/" {
				break
			}
		}
	}
	// Longest module path first, so a nested module wins over its parent.
	sort.SliceStable(r.goMods, func(i, j int) bool { return len(r.goMods[i].modPath) > len(r.goMods[j].modPath) })
	for _, dir := range dirs {
		label := dir
		if dir == "." {
			label = r.goPkgs[dir].name
		}
		id := "pkg:" + dir
		r.nodes = append(r.nodes, Node{ID: id, Label: label, FileType: "code", SourceFile: dir, Kind: KindPackage})
	}
	for _, fe := range files {
		if fe.Lang == "go" {
			r.edges = append(r.edges, Edge{Source: "pkg:" + path.Dir(fe.Path), Target: fe.Path, Relation: "contains", Confidence: Extracted, SourceFile: fe.Path})
		}
	}
}

func readModulePath(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return strings.Trim(f[0], `"`)
			}
		}
	}
	return ""
}

// goImportDir maps an import path to the directory of a scanned package.
func (r *resolver) goImportDir(imp string) (string, bool) {
	for _, m := range r.goMods {
		rest, ok := "", false
		if imp == m.modPath {
			ok = true
		} else if rest, ok = strings.CutPrefix(imp, m.modPath+"/"); !ok {
			continue
		}
		dir := path.Join(m.dir, rest)
		if _, known := r.goPkgs[dir]; known {
			return dir, true
		}
	}
	if len(r.goMods) == 0 {
		// No go.mod in the tree: accept the longest directory the import
		// path ends with.
		for p := imp; strings.Contains(p, "/"); {
			p = p[strings.Index(p, "/")+1:]
			if _, known := r.goPkgs[p]; known {
				return p, true
			}
		}
	}
	return "", false
}

// goImportName is the name an import is used under when it has no alias.
func goImportName(imp string) string {
	parts := strings.Split(imp, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	return name
}

// goFileScope is what a Go file's imports bring into scope.
type goFileScope struct {
	local    map[string]string // name in the file -> directory of a scanned package
	external map[string]bool   // names of packages outside the tree
	dirs     []string          // scanned packages imported, in import order
	targets  []string          // per import: its directory, "" when external
}

func (r *resolver) goScope(fe *FileExtraction) *goFileScope {
	if sc := r.goScopes[fe.Path]; sc != nil {
		return sc
	}
	sc := &goFileScope{local: map[string]string{}, external: map[string]bool{}}
	for _, imp := range fe.Imports {
		dir, ok := r.goImportDir(imp.Path)
		name := imp.Alias
		if !ok {
			if name == "" {
				name = goImportName(imp.Path)
			}
			sc.external[name] = true
			sc.targets = append(sc.targets, "")
			continue
		}
		if name == "" {
			name = r.goPkgs[dir].name
		}
		if name != "_" && name != "." {
			sc.local[name] = dir
		}
		sc.dirs = append(sc.dirs, dir)
		sc.targets = append(sc.targets, dir)
	}
	r.goScopes[fe.Path] = sc
	return sc
}

// goNamed finds the package that declares the type or function written as
// ref ("T" or "pkg.T") in the file fe.
func (r *resolver) goNamed(fe *FileExtraction, ref string) (*goPkg, string, bool) {
	qual, name, qualified := strings.Cut(ref, ".")
	if !qualified {
		return r.goPkgs[path.Dir(fe.Path)], ref, true
	}
	dir, ok := r.goScope(fe).local[qual]
	if !ok {
		return nil, "", false
	}
	return r.goPkgs[dir], name, true
}

// defFile is the extraction of the file that holds a definition.
func (r *resolver) defFile(id string) *FileExtraction {
	file, _, _ := strings.Cut(id, "#")
	return r.byPath[file]
}

// goMethod finds method name of type typ in pkg, also through the types typ
// embeds (their methods are promoted).
func (r *resolver) goMethod(pkg *goPkg, typ, name string, depth int) (string, bool) {
	if pkg == nil {
		return "", false
	}
	if ds := pkg.idx.qualified[typ+"."+name]; len(ds) == 1 {
		return ds[0].ID, true
	}
	if depth >= 3 {
		return "", false
	}
	tid, ok := pkg.types[typ]
	if !ok {
		return "", false
	}
	for _, emb := range r.defs[tid].Embeds {
		if ep, en, ok := r.goNamed(r.defFile(tid), emb); ok {
			if id, ok := r.goMethod(ep, en, name, depth+1); ok {
				return id, true
			}
		}
	}
	return "", false
}

// goTypedMethod resolves a method call from what the file says about the
// type of the value it is called on.
func (r *resolver) goTypedMethod(fe *FileExtraction, ref Ref) (string, bool) {
	written := ref.TName
	if ref.TQual != "" {
		written = ref.TQual + "." + ref.TName
	}
	pkg, typ, ok := r.goNamed(fe, written)
	if !ok || pkg == nil {
		return "", false
	}
	if ref.TCall {
		// The value is what a function returns; a conversion T(x) is a T.
		if fid, isFunc := pkg.funcs[typ]; isFunc {
			result := r.defs[fid].Result
			if result == "" {
				return "", false
			}
			if pkg, typ, ok = r.goNamed(r.defFile(fid), result); !ok {
				return "", false
			}
		}
	}
	if ref.TField != "" {
		tid, ok := pkg.types[typ]
		if !ok {
			return "", false
		}
		ft, ok := r.defs[tid].Fields[ref.TField]
		if !ok {
			return "", false
		}
		if pkg, typ, ok = r.goNamed(r.defFile(tid), ft); !ok {
			return "", false
		}
	}
	return r.goMethod(pkg, typ, ref.Name, 0)
}

func (r *resolver) resolveGo(fe *FileExtraction) {
	pkg := r.goPkgs[path.Dir(fe.Path)]
	sc := r.goScope(fe)
	for i, imp := range fe.Imports {
		if dir := sc.targets[i]; dir != "" {
			r.edges = append(r.edges, Edge{Source: fe.Path, Target: "pkg:" + dir, Relation: "imports", Confidence: Extracted, SourceFile: fe.Path, SourceLocation: loc(imp.Line)})
		}
	}

	for _, ref := range fe.Refs {
		if ref.Relation == "method" {
			if id, ok := pkg.types[ref.Name]; ok {
				r.edge(fe, ref, id, "method", Extracted, 0)
			}
			continue
		}
		call := ref.Relation == "calls"
		// A value whose type the file states: the method is known exactly.
		if call && ref.TName != "" {
			if id, ok := r.goTypedMethod(fe, ref); ok {
				r.edge(fe, ref, id, "calls", Extracted, 0)
				continue
			}
		}
		in := pkg
		switch {
		case ref.Qual == "":
		case ref.TName == "" && ref.Recv == "" && sc.local[ref.Qual] != "":
			in = r.goPkgs[sc.local[ref.Qual]]
		case (sc.external[ref.Qual] && ref.TName == "") || !call:
			continue
		default:
			r.resolveGoMethod(fe, ref, pkg, sc.dirs)
			continue
		}
		if call {
			if id, ok := in.funcs[ref.Name]; ok {
				r.edge(fe, ref, id, "calls", Extracted, 0)
				continue
			}
		}
		if id, ok := in.types[ref.Name]; ok {
			rel := ref.Relation
			if call {
				rel = "uses" // a conversion T(x)
			}
			r.edge(fe, ref, id, rel, Extracted, 0)
		}
	}
}

// resolveGoMethod resolves x.Name(...) when the type of x is not known:
// exactly when x is the receiver, otherwise by a method name that is unique
// in the package or, failing that, in the packages the file imports.
func (r *resolver) resolveGoMethod(fe *FileExtraction, ref Ref, pkg *goPkg, imported []string) {
	if ref.Recv != "" {
		if id, ok := r.goMethod(pkg, ref.Recv, ref.Name, 0); ok {
			r.edge(fe, ref, id, "calls", Extracted, 0)
		}
		// Otherwise the receiver's type has no such method: this calls a
		// function-typed field, or a method of an embedded outside type.
		return
	}
	if ref.TName != "" && !ref.TCall {
		// The type is stated and is not one of the tree's (*os.File,
		// io.Writer, ...): a same-named method here would be a wrong guess.
		// A tree type without that method (an interface) is still guessed.
		p, typ, ok := r.goNamed(fe, strings.TrimPrefix(ref.TQual+"."+ref.TName, "."))
		if !ok || p == nil || p.types[typ] == "" {
			return
		}
		// A concrete tree type without the method: the same. Only a call
		// through an interface is left to be matched by name.
		if ref.TField == "" && !r.defs[p.types[typ]].Iface {
			return
		}
	}
	cands, score := pkg.idx.methods[ref.Name], 0.65
	if len(cands) == 0 {
		score = 0.55
		seen := map[string]bool{}
		for _, dir := range imported {
			if !seen[dir] {
				seen[dir] = true
				cands = append(cands, r.goPkgs[dir].idx.methods[ref.Name]...)
			}
		}
	}
	if !strings.HasSuffix(fe.Path, "_test.go") {
		kept := cands[:0:0]
		for _, c := range cands {
			if file, _, _ := strings.Cut(c.ID, "#"); !strings.HasSuffix(file, "_test.go") {
				kept = append(kept, c)
			}
		}
		cands = kept // code under test does not call into its tests
	}
	if len(cands) == 1 {
		r.edge(fe, ref, cands[0].ID, "calls", Inferred, score)
	}
}

// ---- other languages ----

func (r *resolver) has(p string) bool { _, ok := r.byPath[p]; return ok }

func (r *resolver) firstExisting(cands ...string) (string, bool) {
	for _, c := range cands {
		if c = path.Clean(c); r.has(c) {
			return c, true
		}
	}
	return "", false
}

// uniqueSuffix returns the one scanned file whose path ends in suffix.
func (r *resolver) uniqueSuffix(suffix string) (string, bool) {
	found := ""
	for _, p := range r.byBase[path.Base(suffix)] {
		if p == suffix || strings.HasSuffix(p, "/"+suffix) {
			if found != "" {
				return "", false
			}
			found = p
		}
	}
	return found, found != ""
}

var shellSourceRe = regexp.MustCompile(`[\w./-]*[\w-]\.(?:sh|bash|zsh|bats)\b`)

var jsExts = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}

// importTarget maps an import of a file to the scanned file it names.
func (r *resolver) importTarget(fe *FileExtraction, imp string) (string, bool) {
	dir := path.Dir(fe.Path)
	switch fe.Family {
	case "python":
		rest := strings.TrimLeft(imp, ".")
		dots := len(imp) - len(rest)
		sub := strings.ReplaceAll(rest, ".", "/")
		var bases []string
		if dots > 0 {
			base := dir
			for i := 1; i < dots; i++ {
				base = path.Dir(base)
			}
			bases = []string{base}
		} else {
			for d := dir; ; d = path.Dir(d) {
				bases = append(bases, d)
				if d == "." || d == "/" {
					break
				}
			}
			bases = append(bases, "src")
		}
		for _, b := range bases {
			if sub == "" {
				if p, ok := r.firstExisting(path.Join(b, "__init__.py")); ok {
					return p, true
				}
				continue
			}
			if p, ok := r.firstExisting(path.Join(b, sub+".py"), path.Join(b, sub, "__init__.py")); ok {
				return p, true
			}
		}
	case "js":
		if strings.HasPrefix(imp, ".") {
			return r.firstExisting(jsCandidates(path.Join(dir, imp))...)
		}
		// A path alias or baseUrl from the nearest tsconfig.json / jsconfig.json.
		for _, base := range r.jsConfigFor(dir).targets(imp) {
			if p, ok := r.firstExisting(jsCandidates(base)...); ok {
				return p, true
			}
		}
		return "", false
	case "scala":
		// import a.b.C, a.b.{C, D}, a.b._: the longest leading part that is a file.
		for parts := strings.Split(strings.TrimRight(imp, "._"), "."); len(parts) > 0; parts = parts[:len(parts)-1] {
			if p, ok := r.uniqueSuffix(strings.Join(parts, "/") + ".scala"); ok {
				return p, true
			}
		}
	case "dart":
		if rest, ok := strings.CutPrefix(imp, "package:"); ok {
			// package:<name>/x.dart is lib/x.dart of that package.
			if _, file, found := strings.Cut(rest, "/"); found {
				if p, ok := r.uniqueSuffix("lib/" + file); ok {
					return p, true
				}
				return r.uniqueSuffix(file)
			}
			return "", false
		}
		if strings.Contains(imp, ":") {
			return "", false // dart:core and the like
		}
		return r.firstExisting(path.Join(dir, imp))
	case "lua":
		sub := strings.ReplaceAll(imp, ".", "/")
		for d := dir; ; d = path.Dir(d) {
			if p, ok := r.firstExisting(path.Join(d, sub+".lua"), path.Join(d, sub+".luau"), path.Join(d, sub, "init.lua")); ok {
				return p, true
			}
			if d == "." || d == "/" {
				break
			}
		}
		return r.uniqueSuffix(sub + ".lua")
	case "shell":
		// source "$(dirname "$0")/lib.sh": the directory is computed, so
		// only the file name at the end is reliable.
		m := shellSourceRe.FindAllString(imp, -1)
		if len(m) == 0 {
			return "", false
		}
		file := m[len(m)-1]
		if p, ok := r.firstExisting(path.Join(dir, file), file); ok {
			return p, true
		}
		return r.uniqueSuffix(path.Base(file))
	case "java", "kotlin":
		sub := strings.ReplaceAll(imp, ".", "/")
		for _, e := range []string{".java", ".kt"} {
			if p, ok := r.uniqueSuffix(sub + e); ok {
				return p, true
			}
		}
	case "rust":
		if name, ok := strings.CutPrefix(imp, "mod "); ok {
			name = strings.TrimSpace(name)
			base := dir
			if b := path.Base(fe.Path); b != "mod.rs" && b != "lib.rs" && b != "main.rs" {
				base = path.Join(dir, strings.TrimSuffix(b, ".rs"))
			}
			return r.firstExisting(path.Join(base, name+".rs"), path.Join(base, name, "mod.rs"), path.Join(dir, name+".rs"), path.Join(dir, name, "mod.rs"))
		}
		segs := strings.Split(imp, "::")
		base := dir
		switch segs[0] {
		case "crate":
			for d := dir; ; d = path.Dir(d) {
				if r.has(path.Join(d, "lib.rs")) || r.has(path.Join(d, "main.rs")) {
					base = d
					break
				}
				if d == "." || d == "/" {
					break
				}
			}
		case "super":
			if b := path.Base(fe.Path); b == "mod.rs" {
				base = path.Dir(dir)
			}
		}
		for n := len(segs) - 1; n >= 1; n-- {
			sub := path.Join(segs[1 : n+1]...)
			if p, ok := r.firstExisting(path.Join(base, sub+".rs"), path.Join(base, sub, "mod.rs")); ok && p != fe.Path {
				return p, true
			}
		}
	case "c":
		if p, ok := r.firstExisting(path.Join(dir, imp), imp); ok {
			return p, true
		}
		return r.uniqueSuffix(path.Clean(imp))
	case "ruby":
		return r.firstExisting(path.Join(dir, imp+".rb"), path.Join(dir, imp))
	case "php":
		return r.firstExisting(path.Join(dir, imp), strings.TrimPrefix(imp, "/"))
	}
	return "", false
}

// jsCandidates are the files an extension-less module path can be.
func jsCandidates(p string) []string {
	cands := []string{p}
	stem := p
	if ext := path.Ext(p); ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs" {
		stem = strings.TrimSuffix(p, ext) // "./x.js" in TypeScript means x.ts
	}
	for _, e := range jsExts {
		cands = append(cands, p+e, stem+e)
	}
	for _, e := range jsExts {
		cands = append(cands, path.Join(p, "index"+e))
	}
	return cands
}

// jsConfig is the module resolution part of a tsconfig.json / jsconfig.json.
type jsConfig struct {
	dir     string // directory of the config file, relative to the root
	baseURL string // relative to the root; "" when not set
	paths   []jsPathAlias
}

type jsPathAlias struct {
	prefix, suffix string // the pattern around its "*"
	exact          bool   // the pattern has no "*"
	targets        []string
}

// targets returns the root-relative paths a non-relative import may mean.
func (c *jsConfig) targets(imp string) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, a := range c.paths {
		var star string
		switch {
		case a.exact && imp == a.prefix:
		case !a.exact && len(imp) >= len(a.prefix)+len(a.suffix) && strings.HasPrefix(imp, a.prefix) && strings.HasSuffix(imp, a.suffix):
			star = imp[len(a.prefix) : len(imp)-len(a.suffix)]
		default:
			continue
		}
		for _, t := range a.targets {
			out = append(out, path.Join(c.pathBase(), strings.Replace(t, "*", star, 1)))
		}
		break // patterns are ordered longest prefix first
	}
	if c.baseURL != "" {
		out = append(out, path.Join(c.baseURL, imp))
	}
	return out
}

func (c *jsConfig) pathBase() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return c.dir
}

// jsConfigFor returns the config that governs files in dir: the nearest
// tsconfig.json or jsconfig.json at or above it (nil when there is none).
func (r *resolver) jsConfigFor(dir string) *jsConfig {
	if c, ok := r.jsConfigs[dir]; ok {
		return c
	}
	var c *jsConfig
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		if c = readJSConfig(r.root, dir, name); c != nil {
			break
		}
	}
	if c == nil && dir != "." && dir != "/" {
		c = r.jsConfigFor(path.Dir(dir))
	}
	r.jsConfigs[dir] = c
	return c
}

var (
	jsoncCommentRe = regexp.MustCompile(`(?s)("(?:[^"\\]|\\.)*")|//[^\n]*|/\*.*?\*/`)
	jsoncCommaRe   = regexp.MustCompile(`,(\s*[}\]])`)
)

func readJSConfig(root, dir, name string) *jsConfig {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name))
	if err != nil || len(data) > maxFileBytes {
		return nil
	}
	// The file is JSON with comments and trailing commas.
	data = jsoncCommentRe.ReplaceAll(data, []byte("$1"))
	data = jsoncCommaRe.ReplaceAll(data, []byte("$1"))
	var raw struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	c := &jsConfig{dir: dir}
	rel := func(p string) string {
		p = strings.ReplaceAll(p, "${configDir}", ".")
		return path.Join(dir, filepath.ToSlash(p))
	}
	if raw.CompilerOptions.BaseURL != "" {
		c.baseURL = rel(raw.CompilerOptions.BaseURL)
	}
	for pattern, targets := range raw.CompilerOptions.Paths {
		a := jsPathAlias{prefix: pattern, exact: true}
		if before, after, ok := strings.Cut(pattern, "*"); ok {
			a = jsPathAlias{prefix: before, suffix: after}
		}
		for _, t := range targets {
			a.targets = append(a.targets, strings.ReplaceAll(filepath.ToSlash(t), "${configDir}", "."))
		}
		c.paths = append(c.paths, a)
	}
	sort.Slice(c.paths, func(i, j int) bool {
		if len(c.paths[i].prefix) != len(c.paths[j].prefix) {
			return len(c.paths[i].prefix) > len(c.paths[j].prefix)
		}
		return c.paths[i].prefix+c.paths[i].suffix < c.paths[j].prefix+c.paths[j].suffix
	})
	if c.baseURL == "" && len(c.paths) == 0 {
		return nil
	}
	return c
}

func onlyTypes(ds []Def) []Def {
	var out []Def
	for _, d := range ds {
		if d.Kind == KindType {
			out = append(out, d)
		}
	}
	return out
}

func (r *resolver) resolveGeneric(fe *FileExtraction) {
	spec := specForExt(strings.ToLower(path.Ext(fe.Path)))
	own := r.fileIdx[fe.Path]
	fam := r.famIdx[fe.Family]
	noGlobal := spec != nil && spec.noGlobal

	// The files whose definitions this one can name: its imports and, in
	// languages with directory-wide visibility, its directory.
	var near []*defIndex
	seen := map[string]bool{fe.Path: true}
	addNear := func(p string) {
		if !seen[p] {
			seen[p] = true
			near = append(near, r.fileIdx[p])
		}
	}
	// What each imported name stands for: a definition (orig) of a scanned
	// file, that file as a whole (orig ""), or something outside the tree.
	type source struct{ path, orig string }
	imported := map[string]source{}
	external := map[string]bool{}
	importEdge := func(imp Import, p string) {
		if p != fe.Path {
			r.edges = append(r.edges, Edge{Source: fe.Path, Target: p, Relation: "imports", Confidence: Extracted,
				SourceFile: fe.Path, SourceLocation: loc(imp.Line), TypeOnly: imp.TypeOnly})
			addNear(p)
		}
	}
	for _, imp := range fe.Imports {
		target, ok := r.importTarget(fe, imp.Path)
		if ok {
			importEdge(imp, target)
		}
		for i, local := range imp.Names {
			orig := imp.Origs[i]
			// Python "from pkg import mod" names a module of the package.
			if fe.Family == "python" && orig != "" {
				if sub, found := r.importTarget(fe, strings.TrimSuffix(imp.Path, ".")+"."+orig); found {
					importEdge(imp, sub)
					imported[local] = source{path: sub}
					continue
				}
			}
			if ok {
				imported[local] = source{path: target, orig: orig}
			} else {
				external[local] = true
			}
		}
	}
	// fromImport finds the definition an imported name (or a member of an
	// imported module / class) refers to.
	fromImport := func(ref Ref, typesOnly bool) (Def, bool) {
		key, member := ref.Name, ""
		if ref.Qual != "" {
			key, member = ref.Qual, ref.Name
		}
		src, ok := imported[key]
		idx := r.fileIdx[src.path]
		if !ok || idx == nil || ref.Recv != "" {
			return Def{}, false
		}
		var ds []Def
		switch {
		case member == "" && src.orig != "":
			ds = idx.byName[src.orig]
		case member != "" && src.orig != "":
			ds = idx.qualified[src.orig+"."+member] // a method of an imported class
		case member != "":
			ds = idx.byName[member] // a definition of an imported module
		}
		if typesOnly {
			ds = onlyTypes(ds)
		}
		if len(ds) == 1 {
			return ds[0], true
		}
		return Def{}, false
	}
	if spec != nil && spec.dirVisible {
		for _, p := range r.byDir[fe.Family+"\x00"+path.Dir(fe.Path)] {
			addNear(p)
		}
	}
	gather := func(get func(*defIndex) []Def) []Def {
		var out []Def
		for _, x := range near {
			out = append(out, get(x)...)
		}
		return out
	}

	for _, ref := range fe.Refs {
		name := ref.Name
		if ref.Relation != "calls" {
			// method owner, inherits, implements: a type by name.
			byName := func(x *defIndex) []Def { return onlyTypes(x.byName[name]) }
			if d, ok := fromImport(ref, true); ok {
				r.edge(fe, ref, d.ID, ref.Relation, Extracted, 0)
			} else if external[name] {
				// a type from outside the tree: no local type is meant
			} else if ds := byName(own); len(ds) == 1 {
				r.edge(fe, ref, ds[0].ID, ref.Relation, Extracted, 0)
			} else if ds := gather(byName); len(ds) == 1 {
				r.edge(fe, ref, ds[0].ID, ref.Relation, Inferred, 0.85)
			} else if ds := byName(fam); len(ds) == 1 && len(gather(byName)) == 0 && !noGlobal {
				score := 0.65
				if fe.Family == "sql" {
					score = 0.9 // one table of that name in the schema
				}
				r.edge(fe, ref, ds[0].ID, ref.Relation, Inferred, score)
			}
			continue
		}

		qualified := ref.Qual != "" && ref.Qual != "?" && ref.Recv == ""
		hit := func(d Def, conf string, score float64) {
			rel := "calls"
			if d.Kind == KindType {
				rel = "uses" // construction
			}
			r.edge(fe, ref, d.ID, rel, conf, score)
		}
		// 0. Names the file imports: exact when the import resolved, and
		// never a local definition when it came from outside the tree.
		if d, ok := fromImport(ref, false); ok {
			hit(d, Extracted, 0)
			continue
		}
		if ref.Recv == "" && (external[ref.Qual] || (ref.Qual == "" && external[name])) {
			continue
		}
		// The variable's stated type names the method's owner.
		if ref.TName != "" {
			key := ref.TName + "." + name
			var found *Def
			conf, score := Extracted, 0.0
			if src, ok := imported[ref.TName]; ok && r.fileIdx[src.path] != nil {
				owner := src.orig
				if owner == "" {
					owner = ref.TName
				}
				if ds := r.fileIdx[src.path].qualified[owner+"."+name]; len(ds) == 1 {
					found = &ds[0]
				}
			}
			if found == nil {
				if ds := own.qualified[key]; len(ds) == 1 {
					found = &ds[0]
				} else if ds := gather(func(x *defIndex) []Def { return x.qualified[key] }); len(ds) == 1 {
					found, conf, score = &ds[0], Inferred, 0.85
				} else if ds := fam.qualified[key]; len(ds) == 1 {
					found, conf, score = &ds[0], Inferred, 0.75
				}
			}
			if found != nil {
				hit(*found, conf, score)
				continue
			}
			// A type from outside the tree: no method here is meant. A tree
			// type without the method may inherit it, so that is still tried.
			if len(onlyTypes(own.byName[ref.TName]))+len(onlyTypes(fam.byName[ref.TName])) == 0 {
				continue
			}
		}
		// 1. This file.
		if ref.Recv != "" {
			if ds := own.qualified[ref.Recv+"."+name]; len(ds) == 1 {
				hit(ds[0], Extracted, 0)
				continue
			}
		}
		if qualified {
			if ds := own.qualified[ref.Qual+"."+name]; len(ds) == 1 {
				hit(ds[0], Extracted, 0)
				continue
			}
		}
		if ref.Qual == "" {
			if ds := own.byName[name]; len(ds) == 1 {
				hit(ds[0], Extracted, 0)
				continue
			}
		} else if ds := own.methods[name]; len(ds) == 1 {
			hit(ds[0], Inferred, 0.75)
			continue
		}
		// 2. Imported / same-directory files, then 3. the language family.
		tiers := []struct {
			get    func(func(*defIndex) []Def) []Def
			strong float64
			weak   float64
		}{
			{gather, 0.85, 0.75},
			{func(get func(*defIndex) []Def) []Def { return get(fam) }, 0.65, 0.55},
		}
		for ti, t := range tiers {
			if ti == 1 && (len(name) < 3 || noGlobal) {
				break
			}
			if qualified {
				q := ref.Qual + "." + name
				if ds := t.get(func(x *defIndex) []Def { return x.qualified[q] }); len(ds) == 1 {
					hit(ds[0], Inferred, t.strong)
					break
				}
			}
			if ref.Recv != "" {
				q := ref.Recv + "." + name
				if ds := t.get(func(x *defIndex) []Def { return x.qualified[q] }); len(ds) == 1 {
					hit(ds[0], Inferred, t.strong)
					break
				}
			}
			if ti == 1 && ref.Qual != "" && ref.Recv == "" {
				// obj.name() with nothing in scope to bind it to: a method
				// of that name somewhere in the tree would be a guess.
				break
			}
			ds := t.get(func(x *defIndex) []Def { return x.byName[name] })
			score := t.strong
			if ref.Qual != "" || ref.Recv != "" {
				// obj.name(), or a bare name() inside a type (possibly an
				// inherited method): a method anywhere in scope is as
				// likely as a module-level function.
				ds = append(ds, t.get(func(x *defIndex) []Def { return x.methods[name] })...)
				score = t.weak
			}
			if len(ds) == 1 {
				hit(ds[0], Inferred, score)
				break
			}
			if len(ds) > 1 {
				break // ambiguous here; a wider search would only guess
			}
		}
	}
}

// ---- documents ----

func (r *resolver) resolveDoc(fe *FileExtraction) {
	for _, ref := range fe.Refs {
		name := ref.Name
		if r.has(name) {
			r.edge(fe, ref, name, "references", Extracted, 0)
			continue
		}
		if ref.Relation == "references" {
			continue
		}
		if ps := r.byBase[name]; len(ps) == 1 {
			r.edge(fe, ref, ps[0], "references", Inferred, 0.85)
			continue
		}
		if ds := r.allIdx.qualified[name]; len(ds) == 1 {
			r.edge(fe, ref, ds[0].ID, "mentions", Inferred, 0.75)
			continue
		}
		short := lastSegment(name)
		if len(short) < 4 {
			continue
		}
		ds := r.allIdx.byName[short]
		if short != name {
			ds = append(append([]Def(nil), ds...), r.allIdx.methods[short]...)
		}
		if len(ds) == 1 {
			r.edge(fe, ref, ds[0].ID, "mentions", Inferred, 0.65)
		}
	}
}
