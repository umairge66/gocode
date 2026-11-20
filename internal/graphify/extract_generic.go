package graphify

import (
	"regexp"
	"strings"
)

var (
	jsImportStartRe = regexp.MustCompile(`^\s*(?:import|export)\s*(?:type\s+)?(?:\{|\*|[\w$]+\s*,)[^'"]*$`)
	typeCheckingRe  = regexp.MustCompile(`^\s*if\s+(?:typing\.)?TYPE_CHECKING\s*:`)
	identRe         = regexp.MustCompile(`^[A-Za-z_]\w*$`)
)

// scrub blanks out comments (in both results) and string contents (in code
// only), keeping every byte's position and every newline, so the two results
// line up with the source line by line.
func scrub(src []byte, s *langSpec) (code, withStrings []byte) {
	code = append([]byte(nil), src...)
	withStrings = append([]byte(nil), src...)
	blank := func(b []byte, from, to int) {
		for i := from; i < to && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case s.dashComments && c == '-' && i+3 < n && src[i+1] == '-' && src[i+2] == '[' && src[i+3] == '[':
			j := i + 4
			for j < n && !(src[j] == ']' && j+1 < n && src[j+1] == ']') {
				j++
			}
			j = min(j+2, n)
			blank(code, i, j)
			blank(withStrings, i, j)
			i = j
		case s.slashComments && c == '/' && i+1 < n && src[i+1] == '/' && !s.rawDQuotes,
			s.dashComments && c == '-' && i+1 < n && src[i+1] == '-',
			// "#" opens a comment at the start of a word; "$#", "${#x}" and
			// "a#b" are not comments.
			s.hashComments && c == '#' && (i == 0 || src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n' || src[i-1] == ';'):
			j := i
			for j < n && src[j] != '\n' {
				j++
			}
			blank(code, i, j)
			blank(withStrings, i, j)
			i = j
		case s.slashComments && c == '/' && i+1 < n && src[i+1] == '*':
			j := i + 2
			for j < n && !(src[j] == '*' && j+1 < n && src[j+1] == '/') {
				j++
			}
			j = min(j+2, n)
			blank(code, i, j)
			blank(withStrings, i, j)
			i = j
		case s.tripleQuotes && (c == '"' || c == '\'') && i+2 < n && src[i+1] == c && src[i+2] == c:
			j := i + 3
			for j < n && !(src[j] == c && j+2 < n && src[j+1] == c && src[j+2] == c) {
				j++
			}
			j = min(j+3, n)
			blank(code, i, j)
			i = j
		case (c == '"' && !s.rawDQuotes) || (c == '\'' && s.quoteStrings) || (c == '`' && s.backticks):
			j := i + 1
			for j < n && src[j] != c && (c == '`' || src[j] != '\n') {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, n)
			blank(code, i, j)
			i = j
		case c == '\'':
			// A character literal ('a', '\n', 'é'); anything longer is a
			// lifetime or a label and stays.
			j := i + 1
			for j < n && j < i+10 && src[j] != '\'' && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j < n && src[j] == '\'' && j > i+1 {
				blank(code, i, j+1)
				i = j + 1
			} else {
				i++
			}
		default:
			i++
		}
	}
	return code, withStrings
}

// commandWords appends the [start, end) of every word in command position
// on a shell line: the first word of the line and of each part after "|",
// ";", "&", "(", "{", "then", "do" or "else". Only words that turn out to
// name a function of the script become edges.
func commandWords(line string, out [][2]int) [][2]int {
	atCommand := true
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '|' || c == ';' || c == '&' || c == '(' || c == '{' || c == '!':
			atCommand = true
			i++
		case isIdentByte(c):
			j := i
			for j < len(line) && (isIdentByte(line[j]) || line[j] == '-') {
				j++
			}
			word := line[i:j]
			assignment := j < len(line) && line[j] == '='
			switch {
			case word == "then" || word == "do" || word == "else" || word == "if" || word == "elif" || word == "while" || word == "until" || word == "time" || word == "exec" || word == "eval":
				atCommand = true
			case atCommand && !assignment && !(c >= '0' && c <= '9'):
				out = append(out, [2]int{i, j})
				atCommand = false
			case !assignment:
				atCommand = false
			}
			i = j
		default:
			atCommand = false
			i++
		}
	}
	return out
}

// qualifierBefore reads what a called name is qualified with, from the text
// before it: "" for a bare name, the identifier before ".", "?.", "->" or
// "::", or "?" when that is some other expression.
func qualifierBefore(before string) string {
	j := len(before)
	for j > 0 && (before[j-1] == ' ' || before[j-1] == '\t') {
		j--
	}
	switch {
	case j >= 2 && (before[j-2:j] == "->" || before[j-2:j] == "::" || before[j-2:j] == "?."):
		j -= 2
	case j >= 1 && before[j-1] == '.':
		j--
	default:
		return ""
	}
	for j > 0 && (before[j-1] == ' ' || before[j-1] == '\t') {
		j--
	}
	end := j
	for j > 0 && isIdentByte(before[j-1]) {
		j--
	}
	if j == end || (before[j] >= '0' && before[j] <= '9') {
		return "?"
	}
	return before[j:end]
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// findCalls appends the [start, end) of every name that is followed by "(",
// optionally with explicit type arguments in between: name(, name <T>(,
// name::<T>(.
func findCalls(line string, out [][2]int) [][2]int {
	for p := strings.IndexByte(line, '('); p >= 0; {
		j := p
		for j > 0 && (line[j-1] == ' ' || line[j-1] == '\t') {
			j--
		}
		if j > 0 && line[j-1] == '>' {
			// Walk back over <...>; give up on anything a type list cannot hold.
			depth, k := 0, j-1
			for ; k >= 0; k-- {
				c := line[k]
				if c == '>' {
					depth++
				} else if c == '<' {
					if depth--; depth == 0 {
						break
					}
				} else if !isIdentByte(c) && !strings.ContainsRune(" \t,.[]?:'", rune(c)) {
					k = -1
					break
				}
			}
			if k > 0 {
				j = k
				if j >= 2 && line[j-1] == ':' && line[j-2] == ':' {
					j -= 2
				}
				for j > 0 && (line[j-1] == ' ' || line[j-1] == '\t') {
					j--
				}
			}
		}
		end := j
		for j > 0 && isIdentByte(line[j-1]) {
			j--
		}
		if j < end && !(line[j] >= '0' && line[j] <= '9') {
			out = append(out, [2]int{j, end})
		}
		next := strings.IndexByte(line[p+1:], '(')
		if next < 0 {
			break
		}
		p += next + 1
	}
	return out
}

type scope struct {
	kind       string // KindFunction or KindType
	id         string // "" for an impl/extension body
	name       string
	indent     int
	startDepth int
	bodyDepth  int
	opened     bool
	// lengths to cut back to when the scope turns out to be a declaration
	nNodes, nEdges, nDefs, nRefs int
	// types are the variables declared in this scope with a stated type
	// ("" when two declarations disagree).
	types map[string]string
}

// declare records that name has the given type in a scope's (or the file's)
// table; a second, different type makes the name's type unknown.
func declare(table map[string]string, name, typ string) {
	if old, ok := table[name]; ok && old != typ {
		table[name] = ""
		return
	}
	table[name] = typ
}

type pendingImpl struct {
	typeName, target, relation string
	line                       int
}

// extractGeneric reads a file of one of the languages in langSpecs line by
// line: definitions by pattern, their extent by braces or indentation, and
// every "name(" inside as a call from the enclosing definition.
func extractGeneric(rel string, src []byte, s *langSpec) *FileExtraction {
	fe := newFileExtraction(rel, s.name, s.family, "code")
	code, withStr := scrub(src, s)
	codeLines := strings.Split(string(code), "\n")
	strLines := strings.Split(string(withStr), "\n")

	var stack []*scope
	var impls []pendingImpl
	depth := 0
	importBuf := ""       // an import statement still waiting for its module
	typeCheckIndent := -1 // indentation of an open "if TYPE_CHECKING:" block
	top := func() *scope {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	// enclosing returns the innermost type scope name when the innermost
	// scope chain is "type" or "function directly in a type".
	enclosingType := func() *scope {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == KindType {
				return stack[i]
			}
		}
		return nil
	}
	fileTypes := map[string]string{}
	// typeOf is the stated type of a variable, innermost scope first.
	typeOf := func(name string) string {
		for i := len(stack) - 1; i >= 0; i-- {
			if t, ok := stack[i].types[name]; ok {
				return t
			}
		}
		return fileTypes[name]
	}
	isSelf := func(q string) bool {
		for _, n := range s.selfs {
			if n == q {
				return true
			}
		}
		return false
	}

	for i, line := range codeLines {
		lineNo := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if s.indentScoped {
			for t := top(); t != nil && indent <= t.indent; t = top() {
				stack = stack[:len(stack)-1]
			}
		}

		// Imports. A JavaScript import may list its names over several lines
		// before the module; a Python one under "if TYPE_CHECKING:" is for
		// the type checker only.
		stmt := strLines[i]
		if importBuf != "" {
			importBuf += " " + stmt
			stmt = importBuf
			if strings.ContainsAny(strLines[i], `'";`) || len(importBuf) > 4000 {
				importBuf = ""
			}
		} else if s.family == "js" && jsImportStartRe.MatchString(stmt) && !strings.Contains(stmt, ";") {
			importBuf = stmt
		}
		if typeCheckIndent >= 0 && indent <= typeCheckIndent {
			typeCheckIndent = -1
		}
		mayImport := false
		for _, word := range s.importNeed {
			mayImport = mayImport || strings.Contains(stmt, word)
		}
		for _, re := range s.imports {
			if !mayImport {
				break
			}
			for _, m := range re.FindAllStringSubmatch(stmt, -1) {
				imp := Import{Path: m[1], Line: lineNo, TypeOnly: typeCheckIndent >= 0}
				if s.importNames != nil {
					var typeOnly bool
					imp.Names, imp.Origs, typeOnly = s.importNames(stmt)
					imp.TypeOnly = imp.TypeOnly || typeOnly
				}
				fe.Imports = append(fe.Imports, imp)
			}
		}
		if s.tripleQuotes && typeCheckingRe.MatchString(line) {
			typeCheckIndent = indent
		}

		// Definition on this line (first matching rule).
		defStart, defEnd := -1, -1
		for _, r := range s.defs {
			t := top()
			if r.where == inType && (t == nil || t.kind != KindType || (!s.indentScoped && !t.opened)) {
				continue
			}
			if r.where == notInFunc && t != nil && t.kind == KindFunction {
				continue
			}
			if r.need != "" && !strings.Contains(line, r.need) {
				continue
			}
			m := r.re.FindStringSubmatchIndex(line)
			if m == nil {
				continue
			}
			group := func(name string) (string, int) {
				gi := r.re.SubexpIndex(name)
				if gi < 0 || m[2*gi] < 0 {
					return "", -1
				}
				return line[m[2*gi]:m[2*gi+1]], m[2*gi]
			}
			full, at := group("name")
			name, recv := full, ""
			if k := strings.LastIndex(full, "::"); k >= 0 {
				name, recv = full[k+2:], lastSegment(full[:k])
			} else if k := strings.LastIndexAny(full, ".:"); k > 0 && r.kind == KindFunction {
				name, recv = full[k+1:], lastSegment(full[:k]) // Lua: M.name, M:name
			} else if r.kind != KindFunction {
				name = lastSegment(full)
				if f := strings.Fields(name); len(f) > 1 {
					name = f[len(f)-1] // "export default" -> default
				}
			}
			if name == "" || notDefinable[name] {
				continue
			}
			if r.kind == KindFunction && r.where != anywhere {
				if prefix := line[:at]; strings.Contains(prefix, "=") || (s.declIsNotDef && strings.Contains(prefix, "(")) {
					continue
				}
			}
			// A definition still waiting for its body had none.
			if t != nil && !s.indentScoped && !t.opened {
				stack = stack[:len(stack)-1]
				t = top()
			}
			sc := &scope{kind: KindFunction, name: name, indent: indent, startDepth: depth, types: map[string]string{},
				nNodes: len(fe.Nodes), nEdges: len(fe.Edges), nDefs: len(fe.Defs), nRefs: len(fe.Refs)}
			switch r.kind {
			case KindFunction:
				if recv == "" && t != nil && t.kind == KindType {
					recv = t.name
				}
				if recv != "" {
					sc.id = fe.addDef(KindMethod, name, recv, lineNo)
					fe.addRef(Ref{From: sc.id, Name: recv, Relation: "method", Line: lineNo})
				} else {
					sc.id = fe.addDef(KindFunction, name, "", lineNo)
				}
			case KindType:
				sc.kind = KindType
				sc.id = fe.addDef(KindType, name, "", lineNo)
				for _, g := range [][2]string{{"bases", "inherits"}, {"impls", "implements"}} {
					list, _ := group(g[0])
					for _, b := range cleanBases(list, s.baseSep) {
						fe.addRef(Ref{From: sc.id, Name: b, Relation: g[1], Line: lineNo})
					}
				}
			default: // impl / extension
				sc.kind = KindType
				for _, g := range []string{"trait", "impls"} {
					list, _ := group(g)
					for _, b := range cleanBases(list, s.baseSep) {
						impls = append(impls, pendingImpl{typeName: name, target: b, relation: "implements", line: lineNo})
					}
				}
			}
			stack = append(stack, sc)
			defStart, defEnd = at, at+len(full)
			break
		}

		// Declarations on this line that state a variable's type. One made
		// through the object itself (self.repo = Repo()) belongs to the type.
		if strings.IndexFunc(line, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0 {
			for _, re := range s.typed {
				vi, ti := re.SubexpIndex("var"), re.SubexpIndex("type")
				for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
					name, typ := line[m[2*vi]:m[2*vi+1]], line[m[2*ti]:m[2*ti+1]]
					table := fileTypes
					if t := top(); t != nil {
						table = t.types
					}
					if t := enclosingType(); t != nil && isSelf(qualifierBefore(line[:m[2*vi]])) {
						table = t.types
					}
					declare(table, name, typ)
				}
			}
		}

		// Calls on this line, from the innermost definition.
		from := fe.Path
		for k := len(stack) - 1; k >= 0; k-- {
			if stack[k].id != "" {
				from = stack[k].id
				break
			}
		}
		var calls [][2]int
		if t := top(); !s.declIsNotDef || (t != nil && t.kind == KindFunction) {
			// (in C, "int f(int);" outside a body declares, it does not call)
			calls = findCalls(line, calls)
			if s.bareCalls {
				calls = commandWords(line, calls)
			}
		}
		for _, m := range calls {
			start, end := m[0], m[1]
			name := line[start:end]
			if start >= defStart && end <= defEnd {
				continue
			}
			r := Ref{From: from, Name: name, Relation: "calls", Line: lineNo}
			r.Qual = qualifierBefore(line[:start])
			if r.Qual == "" && s.notCallable[name] {
				continue // a keyword; after "x." or "X::" the same word is a name
			}
			if t := enclosingType(); t != nil && (r.Qual == "" || isSelf(r.Qual)) {
				r.Recv = t.name
			} else if r.Qual != "" && r.Qual != "?" {
				r.TName = typeOf(r.Qual)
			}
			fe.addRef(r)
		}

		if s.indentScoped {
			continue
		}
		for _, c := range []byte(line) {
			t := top()
			switch c {
			case '{':
				depth++
				if t != nil && !t.opened {
					t.opened, t.bodyDepth = true, depth
				}
			case '}':
				depth--
				for t != nil && t.opened && depth < t.bodyDepth {
					stack = stack[:len(stack)-1]
					t = top()
				}
			case ';':
				if t != nil && !t.opened && depth == t.startDepth {
					if s.declIsNotDef && t.kind == KindFunction {
						for _, n := range fe.Nodes[t.nNodes:] {
							delete(fe.seenID, n.ID)
						}
						fe.Nodes, fe.Edges, fe.Defs, fe.Refs = fe.Nodes[:t.nNodes], fe.Edges[:t.nEdges], fe.Defs[:t.nDefs], fe.Refs[:t.nRefs]
					}
					stack = stack[:len(stack)-1]
				}
			}
		}
	}

	// "impl Trait for Type" / "extension Type: Proto" attach to the type when
	// it is declared in this file.
	for _, p := range impls {
		for _, d := range fe.Defs {
			if d.Kind == KindType && d.Name == p.typeName {
				fe.addRef(Ref{From: d.ID, Name: p.target, Relation: p.relation, Line: p.line})
				break
			}
		}
	}
	return fe
}

// lastSegment is the last part of a dotted / namespaced name.
func lastSegment(s string) string {
	if k := strings.LastIndexAny(s, ".:\\"); k >= 0 {
		return s[k+1:]
	}
	return s
}

var (
	bracketsRe = regexp.MustCompile(`<[^<>]*>|\([^()]*\)|\[[^\[\]]*\]`)
	accessRe   = regexp.MustCompile(`\b(?:public|private|protected|virtual)\b`)
)

// cleanBases turns a base-type list ("public A<T>, pkg.B(), metaclass=M")
// into plain type names.
func cleanBases(list, sep string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	if sep == "" {
		sep = ","
	}
	for range 3 {
		list = bracketsRe.ReplaceAllString(list, "")
	}
	list = accessRe.ReplaceAllString(list, "")
	var out []string
	for _, part := range strings.Split(list, sep) {
		part = strings.TrimSpace(part)
		if part == "" || strings.Contains(part, "=") {
			continue
		}
		name := lastSegment(part)
		if identRe.MatchString(name) && name != "object" {
			out = append(out, name)
		}
	}
	return out
}
