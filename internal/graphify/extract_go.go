package graphify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// extractGo reads a Go file with go/ast. Calls and type uses are recorded as
// references and resolved later against the package's (and its imports')
// definitions, because that needs the other files of the tree.
func extractGo(rel string, src []byte) *FileExtraction {
	fe := newFileExtraction(rel, "go", "go", "code")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if f == nil {
		if err != nil {
			fe.Err = err.Error()
		}
		return fe
	}
	fe.Package = f.Name.Name
	line := func(p token.Pos) int { return fset.Position(p).Line }

	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		fe.Imports = append(fe.Imports, Import{Path: p, Alias: alias, Line: line(imp.Pos())})
	}

	// Package-level variables of this file with a known type.
	fileEnv := goEnv{}
	for _, decl := range f.Decls {
		if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					fileEnv.declare(vs)
				}
			}
		}
	}
	lastDef := func(id string) *Def {
		if n := len(fe.Defs); n > 0 && fe.Defs[n-1].ID == id {
			return &fe.Defs[n-1]
		}
		return nil
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			recv, recvVar := "", ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv = goTypeName(d.Recv.List[0].Type)
				if names := d.Recv.List[0].Names; len(names) > 0 {
					recvVar = names[0].Name
				}
			}
			var id string
			if recv != "" {
				id = fe.addDef(KindMethod, d.Name.Name, recv, line(d.Pos()))
				fe.addRef(Ref{From: id, Name: recv, Relation: "method", Line: line(d.Pos())})
			} else {
				id = fe.addDef(KindFunction, d.Name.Name, "", line(d.Pos()))
			}
			if def := lastDef(id); def != nil && d.Type.Results != nil && len(d.Type.Results.List) > 0 {
				if t, ok := goExprType(d.Type.Results.List[0].Type); ok {
					def.Result = t.String()
				}
			}
			goTypeRefs(d.Type, func(qual, name string, p token.Pos) {
				fe.addRef(Ref{From: id, Name: name, Qual: qual, Relation: "uses", Line: line(p)})
			})
			if d.Body != nil {
				env := goEnv{}
				if recvVar != "" {
					env[recvVar] = goType{name: recv}
				}
				env.fields(d.Type.Params)
				env.fields(d.Type.Results)
				env.scan(d.Body)
				for k, v := range fileEnv {
					if _, local := env[k]; !local {
						env[k] = v
					}
				}
				goBodyRefs(fe, id, recv, recvVar, env, d.Body, line)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					id := fe.addDef(KindType, s.Name.Name, "", line(s.Pos()))
					goTypeSpecRefs(fe, id, s.Type, line)
					if _, ok := s.Type.(*ast.InterfaceType); ok {
						if def := lastDef(id); def != nil {
							def.Iface = true
						}
					}
					if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
						if def := lastDef(id); def != nil {
							for _, fld := range st.Fields.List {
								t, ok := goExprType(fld.Type)
								if !ok {
									continue
								}
								if len(fld.Names) == 0 {
									def.Embeds = append(def.Embeds, t.String())
								}
								for _, n := range fld.Names {
									if def.Fields == nil {
										def.Fields = map[string]string{}
									}
									def.Fields[n.Name] = t.String()
								}
							}
						}
					}
				case *ast.ValueSpec:
					// Package-level initializers run at load time: their calls
					// belong to the file.
					for _, v := range s.Values {
						goBodyRefs(fe, fe.Path, "", "", fileEnv, v, line)
					}
				}
			}
		}
	}
	return fe
}

// goType is what is known about a value's type: the named type qual.name,
// or (call) whatever the function qual.name returns.
type goType struct {
	qual, name string
	call       bool
}

func (t goType) String() string {
	if t.qual != "" {
		return t.qual + "." + t.name
	}
	return t.name
}

// goExprType reads a type expression: T, *T, pkg.T, T[K].
func goExprType(e ast.Expr) (goType, bool) {
	switch t := e.(type) {
	case *ast.Ident:
		return goType{name: t.Name}, true
	case *ast.StarExpr:
		return goExprType(t.X)
	case *ast.ParenExpr:
		return goExprType(t.X)
	case *ast.IndexExpr:
		return goExprType(t.X)
	case *ast.IndexListExpr:
		return goExprType(t.X)
	case *ast.SelectorExpr:
		if q, ok := t.X.(*ast.Ident); ok {
			return goType{qual: q.Name, name: t.Sel.Name}, true
		}
	}
	return goType{}, false
}

// goValueType reads the type of a value from how it is made: T{...},
// &T{...}, new(T), or a call f(...) / pkg.f(...) (the type f returns).
func goValueType(e ast.Expr) (goType, bool) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return goValueType(v.X)
	case *ast.CompositeLit:
		if v.Type != nil {
			return goExprType(v.Type)
		}
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return goValueType(v.X)
		}
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
			return goExprType(v.Args[0])
		}
		if t, ok := goExprType(v.Fun); ok {
			t.call = true
			return t, true
		}
	}
	return goType{}, false
}

// goEnv maps the variables of a function (parameters, results, locals) to
// what is known of their types. It ignores block scopes: the first
// declaration of a name wins.
type goEnv map[string]goType

func (env goEnv) set(name string, t goType) {
	if _, dup := env[name]; !dup && name != "_" {
		env[name] = t
	}
}

func (env goEnv) fields(list *ast.FieldList) {
	if list == nil {
		return
	}
	for _, f := range list.List {
		if t, ok := goExprType(f.Type); ok {
			for _, n := range f.Names {
				env.set(n.Name, t)
			}
		}
	}
}

func (env goEnv) declare(vs *ast.ValueSpec) {
	for i, n := range vs.Names {
		if vs.Type != nil {
			if t, ok := goExprType(vs.Type); ok {
				env.set(n.Name, t)
			}
		} else if i < len(vs.Values) {
			if t, ok := goValueType(vs.Values[i]); ok {
				env.set(n.Name, t)
			}
		}
	}
}

func (env goEnv) scan(body ast.Node) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			env.fields(x.Type.Params)
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						env.declare(vs)
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				var rhs ast.Expr
				switch {
				case len(x.Rhs) == len(x.Lhs):
					rhs = x.Rhs[i]
				case len(x.Rhs) == 1 && i == 0:
					rhs = x.Rhs[0] // v, err := f(): v is f's first result
				default:
					continue
				}
				if t, ok := goValueType(rhs); ok {
					env.set(id.Name, t)
				}
			}
		}
		return true
	})
}

// goTypeName is the named type of a receiver or embedded field: T, *T, T[K].
func goTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return goTypeName(t.X)
	case *ast.ParenExpr:
		return goTypeName(t.X)
	case *ast.IndexExpr:
		return goTypeName(t.X)
	case *ast.IndexListExpr:
		return goTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// goTypeSpecRefs records what a type declaration is built from: embedded
// types ("embeds") and the types of fields and method signatures ("uses").
func goTypeSpecRefs(fe *FileExtraction, id string, e ast.Expr, line func(token.Pos) int) {
	uses := func(qual, name string, p token.Pos) {
		fe.addRef(Ref{From: id, Name: name, Qual: qual, Relation: "uses", Line: line(p)})
	}
	embeds := func(qual, name string, p token.Pos) {
		fe.addRef(Ref{From: id, Name: name, Qual: qual, Relation: "embeds", Line: line(p)})
	}
	var fields *ast.FieldList
	switch t := e.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	default:
		goTypeRefs(e, uses)
		return
	}
	if fields == nil {
		return
	}
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			goTypeRefs(f.Type, embeds)
		} else {
			goTypeRefs(f.Type, uses)
		}
	}
}

// goTypeRefs calls add for every named type a type expression mentions.
func goTypeRefs(e ast.Expr, add func(qual, name string, p token.Pos)) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := t.X.(*ast.Ident); ok {
				add(x.Name, t.Sel.Name, t.Pos())
			}
			return false
		case *ast.Ident:
			add("", t.Name, t.Pos())
		case *ast.Field:
			// Parameter and field names are not types.
			if t.Type != nil {
				goTypeRefs(t.Type, add)
			}
			return false
		}
		return true
	})
}

// goBodyRefs records the calls and composite-literal types in a function
// body (or an initializer expression).
func goBodyRefs(fe *FileExtraction, from, recv, recvVar string, env goEnv, body ast.Node, line func(token.Pos) int) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			fun := x.Fun
			for {
				switch f := fun.(type) {
				case *ast.ParenExpr:
					fun = f.X
					continue
				case *ast.IndexExpr:
					fun = f.X
					continue
				case *ast.IndexListExpr:
					fun = f.X
					continue
				}
				break
			}
			switch f := fun.(type) {
			case *ast.Ident:
				fe.addRef(Ref{From: from, Name: f.Name, Relation: "calls", Line: line(f.Pos())})
			case *ast.SelectorExpr:
				r := Ref{From: from, Name: f.Sel.Name, Qual: "?", Relation: "calls", Line: line(f.Pos())}
				switch x := f.X.(type) {
				case *ast.Ident:
					r.Qual = x.Name
					if recvVar != "" && x.Name == recvVar {
						r.Recv = recv
					} else if t, ok := env[x.Name]; ok {
						r.TQual, r.TName, r.TCall = t.qual, t.name, t.call
					}
				case *ast.SelectorExpr:
					// v.field.Method(): the field's type, when v's is known.
					if v, ok := x.X.(*ast.Ident); ok {
						if t, ok := env[v.Name]; ok && !t.call {
							r.TQual, r.TName, r.TField = t.qual, t.name, x.Sel.Name
						}
					}
				}
				fe.addRef(r)
			}
		case *ast.CompositeLit:
			if x.Type != nil {
				goTypeRefs(x.Type, func(qual, name string, p token.Pos) {
					fe.addRef(Ref{From: from, Name: name, Qual: qual, Relation: "uses", Line: line(p)})
				})
			}
		}
		return true
	})
}
