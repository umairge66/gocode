package mods

import (
	"fmt"
	"reflect"
	"slices"
	"sort"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// Scan is what a hooks module hooks and calls, read from its source before it
// loads. The host serves a module
// only the calls its scan lists: a `$` call spelled nowhere in the module's
// source is refused, and $.env.get / $.env.set serve only the variable names
// the source passes them as string literals.
type Scan struct {
	Hooks  []string `json:"hooks"`
	Calls  []string `json:"calls"`
	NextTo []string `json:"nextTo,omitempty"`
	Env    *EnvScan `json:"env,omitempty"`
	// State lists the { plugin, key } references of its $.state calls
	//.
	State  *StateScan `json:"state,omitempty"`
	Notes  []string   `json:"-"`
	parsed *ast.Program
}

// EnvScan lists the variable names a module reads and writes.
type EnvScan struct {
	Reads  []string `json:"reads"`
	Writes []string `json:"writes"`
}

// eventCalls are the events a module may raise through `$` beside the op
// events.
var eventCalls = []string{
	"tool.call", "tool.check", "agent.spawn", "prompt.submit", "prompt.fill",
	"prompt.suggest", "prompt.compose", "prompt.media", "command.run", "config.set",
	"telemetry.log", "telemetry.mark", "ui.ask",
}

// IsCallable reports whether "noun.method" is a call `$` offers.
func IsCallable(name string) bool {
	return IsOpEvent(name) || slices.Contains(eventCalls, name)
}

// ScanSource reads a compiled module's source (Compile's output).
func ScanSource(code string) (*Scan, error) {
	prog, err := parser.ParseFile(nil, "module.js", code, 0)
	if err != nil {
		return nil, fmt.Errorf("does not parse: %v", err)
	}
	s := &Scan{parsed: prog}
	hooks := map[string]bool{}
	calls := map[string]bool{}
	nextTo := map[string]bool{}
	reads := map[string]bool{}
	writes := map[string]bool{}
	states := newStateScanner(prog)

	walk(prog, func(n ast.Node) {
		switch x := n.(type) {
		case *ast.DotExpression:
			if noun, ok := memberName(x.Left); ok {
				name := noun + "." + string(x.Identifier.Name)
				if IsCallable(name) {
					calls[name] = true
				}
			}
		case *ast.BracketExpression:
			if lit, ok := x.Member.(*ast.StringLiteral); ok {
				if noun, ok := memberName(x.Left); ok {
					name := noun + "." + string(lit.Value)
					if IsCallable(name) {
						calls[name] = true
					}
				}
			}
		case *ast.CallExpression:
			// $.env.get("NAME") / $.env.set("NAME", v): the names it serves.
			// $.state.get(ref) / $.state.set(ref, v): the values it reads
			// and writes.
			if name, ok := calleeName(x.Callee); ok && (name == "state.get" || name == "state.set") {
				states.call(name, x.ArgumentList)
			}
			if name, ok := calleeName(x.Callee); ok && (name == "env.get" || name == "env.set") && len(x.ArgumentList) > 0 {
				if lit, ok := x.ArgumentList[0].(*ast.StringLiteral); ok {
					if name == "env.get" {
						reads[string(lit.Value)] = true
					} else {
						writes[string(lit.Value)] = true
					}
				}
			}
			// on("event", hook) / on("event", matcher, hook).
			if _, isIdent := x.Callee.(*ast.Identifier); isIdent && (len(x.ArgumentList) == 2 || len(x.ArgumentList) == 3) {
				if lit, ok := x.ArgumentList[0].(*ast.StringLiteral); ok {
					if p, err := parsePattern(string(lit.Value)); err == nil && (p.kind != "name" || IsEvent(p.prefixOrN)) {
						hooks[string(lit.Value)] = true
					}
				}
			}
			// next.to(e, "append"): the tiers a module may continue at.
			if d, ok := x.Callee.(*ast.DotExpression); ok && d.Identifier.Name == "to" && len(x.ArgumentList) == 2 {
				if lit, ok := x.ArgumentList[1].(*ast.StringLiteral); ok && slices.Contains(nextToTargets(TierPrepend), Tier(lit.Value)) {
					nextTo[string(lit.Value)] = true
				}
			}
		}
	})
	s.Hooks = sortedKeys(hooks)
	s.Calls = sortedKeys(calls)
	if len(nextTo) > 0 {
		for _, t := range nextToTargets(TierPrepend) {
			if nextTo[string(t)] {
				s.NextTo = append(s.NextTo, string(t))
			}
		}
	}
	if len(reads) > 0 || len(writes) > 0 {
		s.Env = &EnvScan{Reads: sortedKeys(reads), Writes: sortedKeys(writes)}
	}
	s.State = states.result()
	for _, op := range states.bad {
		s.Notes = append(s.Notes, fmt.Sprintf("a $.%s call whose reference the scan cannot read (plugin and key string literals, at the call or in a const of this file); it is refused when made", op))
	}
	return s, nil
}

// allowsCall: "" when the scan lists the call, else the refusal.
func (s *Scan) allowsCall(name string) string {
	if s == nil {
		return "no hooks module of that name is loaded, so there is no scan to allow it (host rule)"
	}
	if slices.Contains(s.Calls, name) {
		return ""
	}
	listed := "no calls"
	if len(s.Calls) > 0 {
		listed = ""
		for i, c := range s.Calls {
			if i > 0 {
				listed += ", "
			}
			listed += "$." + c
		}
	}
	return "its hooks module does not call it (host rule; the scan lists " + listed + ")"
}

// allowsEnv: env.get / env.set serve only the names the scan lists.
func (s *Scan) allowsEnv(op, name string) string {
	if op != "env.get" && op != "env.set" {
		return ""
	}
	if name == "" {
		return fmt.Sprintf("it carries no variable name a $.%s(\"<name>\") call passes", op)
	}
	var list []string
	kind := "writes"
	if s != nil && s.Env != nil {
		if op == "env.get" {
			list = s.Env.Reads
		} else {
			list = s.Env.Writes
		}
	}
	if op == "env.get" {
		kind = "reads"
	}
	if slices.Contains(list, name) {
		return ""
	}
	listed := "nothing"
	if len(list) > 0 {
		listed = ""
		for i, n := range list {
			if i > 0 {
				listed += ", "
			}
			listed += n
		}
	}
	return fmt.Sprintf("its hooks module does not name %s in a $.%s(\"<name>\") call (host rule; the scan lists %s of %s)", name, op, kind, listed)
}

// allowsNextTo: next.to(e, tier) only to a tier spelled in a literal.
func (s *Scan) allowsNextTo(plugin string, tier Tier) error {
	if s != nil && slices.Contains(s.NextTo, string(tier)) {
		return nil
	}
	return fmt.Errorf("%s: next.to(%q) refused: its hooks module does not spell next.to:%s in a literal the scan reads (host rule)", plugin, tier, tier)
}

// memberName is the property name of e when e is `<something>.<name>` or
// `<something>["<name>"]` (or a bare identifier).
func memberName(e ast.Expression) (string, bool) {
	switch x := e.(type) {
	case *ast.DotExpression:
		return string(x.Identifier.Name), true
	case *ast.BracketExpression:
		if lit, ok := x.Member.(*ast.StringLiteral); ok {
			return string(lit.Value), true
		}
	}
	return "", false
}

// calleeName is "noun.method" for a callee `<x>.noun.method`.
func calleeName(e ast.Expression) (string, bool) {
	var method string
	var left ast.Expression
	switch x := e.(type) {
	case *ast.DotExpression:
		method, left = string(x.Identifier.Name), x.Left
	case *ast.BracketExpression:
		lit, ok := x.Member.(*ast.StringLiteral)
		if !ok {
			return "", false
		}
		method, left = string(lit.Value), x.Left
	default:
		return "", false
	}
	noun, ok := memberName(left)
	if !ok {
		return "", false
	}
	return noun + "." + method, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var nodeType = reflect.TypeOf((*ast.Node)(nil)).Elem()

// walk visits every AST node under n (goja's ast has no walker; this one
// follows every field that holds a node or a list of them).
func walk(n any, visit func(ast.Node)) {
	var rec func(v reflect.Value)
	rec = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return
			}
			rec(v.Elem())
		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			if v.Type().Implements(nodeType) {
				visit(v.Interface().(ast.Node))
			}
			if v.Elem().Kind() == reflect.Struct {
				rec(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				f := v.Type().Field(i)
				if !f.IsExported() {
					continue
				}
				rec(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				rec(v.Index(i))
			}
		}
	}
	rec(reflect.ValueOf(n))
}
