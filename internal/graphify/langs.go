package graphify

import (
	"regexp"
	"strings"
)

// Where a definition rule applies, relative to the enclosing scope.
const (
	anywhere  = iota
	inType    // directly inside a class/impl body
	notInFunc // at file, namespace or class level
)

// defRule recognizes one kind of definition on a line of scrubbed code. The
// pattern names the groups it fills: name (required), bases and impls (the
// types a type inherits from / implements) and trait (Rust "impl T for").
type defRule struct {
	re    *regexp.Regexp
	kind  string // KindFunction, KindType, or "impl" (a body for a type declared elsewhere)
	where int
	// need is text every matching line contains; lines without it skip the
	// (much slower) pattern.
	need string
}

// langSpec describes a language to the line-oriented extractor.
type langSpec struct {
	name   string
	family string // languages whose definitions can refer to each other
	exts   []string

	hashComments  bool // # to end of line
	dashComments  bool // -- to end of line, and --[[ ]]
	rawDQuotes    bool // "..." is an identifier, not a string (SQL)
	bareCalls     bool // a command is called by its bare name (shell)
	noGlobal      bool // names are never resolved outside the file and what it sources
	slashComments bool // // and /* */
	quoteStrings  bool // '...' is a string (otherwise a short char literal)
	tripleQuotes  bool // """...""" and '''...'''
	backticks     bool // `...` template strings
	indentScoped  bool // bodies end by indentation, not braces
	declIsNotDef  bool // "f(...);" is a declaration: drop it
	dirVisible    bool // files in one directory see each other's definitions
	baseSep       string

	defs    []defRule
	imports []*regexp.Regexp // group 1 is the import target; matched with strings intact
	selfs   []string         // names for "this object" in a call qualifier
	// keywords are this language's words that can precede "(" without being
	// a call, besides the ones every language shares (commonKeywords).
	keywords string
	// importNames returns the names an import statement binds; nil when the
	// language's imports are not read for names.
	importNames func(stmt string) (names, origs []string, typeOnly bool)

	// typed are patterns for declarations that state a variable's type
	// (groups var and type); they let obj.method() bind to that type's method.
	typed []*regexp.Regexp

	notCallable map[string]bool
	// importNeed are words one of which every import statement contains;
	// lines without any skip the import patterns.
	importNeed []string
}

func rule(kind string, where int, pattern string) defRule {
	return defRule{re: regexp.MustCompile(pattern), kind: kind, where: where}
}

func res(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

const (
	jsID      = `[A-Za-z_$][\w$]*`
	javaMods  = `(?:(?:public|private|protected|static|final|abstract|synchronized|native|default|sealed|strictfp)\s+)`
	javaAnnot = `(?:@\w+(?:\([^)]*\))?\s+)*`
	csMods    = `(?:(?:public|private|protected|internal|static|virtual|override|abstract|async|sealed|extern|unsafe|new|partial|readonly|file)\s+)`
	ktMods    = `(?:(?:public|private|protected|internal|open|override|abstract|final|suspend|inline|operator|infix|tailrec|external|actual|expect|data|sealed|enum|annotation|inner|value)\s+)*`
	swiftMods = `(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|private|fileprivate|internal|open|static|final|override|mutating|nonmutating|convenience|required|nonisolated|dynamic|indirect)\s+)*`
	rustVis   = `(?:pub(?:\([^)]*\))?\s+)?`
	typeTok   = `[\w.<>\[\],?]+`
)

var langSpecs = []*langSpec{
	{
		keywords: "print exec del global nonlocal pass raise",
		name:     "python", family: "python", exts: []string{".py"},
		hashComments: true, quoteStrings: true, tripleQuotes: true, indentScoped: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*(?:async\s+)?def\s+(?P<name>\w+)`),
			rule(KindType, anywhere, `^\s*class\s+(?P<name>\w+)\s*(?:\((?P<bases>[^)]*)\))?`),
		},
		imports:     res(`^\s*from\s+(\.*[\w.]*)\s+import\b`, `^\s*import\s+([\w.]+)`),
		selfs:       []string{"self", "cls"},
		importNames: pythonImportNames,
	},
	{
		keywords: "import require export from let var const static get set async constructor void debugger",
		name:     "javascript", family: "js", exts: []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"},
		slashComments: true, quoteStrings: true, backticks: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*(?P<name>`+jsID+`)`),
			rule(KindType, anywhere, `^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?class\s+(?P<name>`+jsID+`)(?:\s*<[^>{]*>)?(?:\s+extends\s+(?P<bases>[\w$.]+)(?:<[^{]*>)?)?(?:\s+implements\s+(?P<impls>[^{]+))?`),
			rule(KindType, anywhere, `^\s*(?:export\s+)?(?:declare\s+)?interface\s+(?P<name>`+jsID+`)(?:\s*<[^>{]*>)?(?:\s+extends\s+(?P<bases>[^{]+))?`),
			rule(KindFunction, notInFunc, `^\s*(?:export\s+)?(?:const|let|var)\s+(?P<name>`+jsID+`)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|(?:\([^)]*\)|`+jsID+`)\s*(?::\s*[^=]+?)?\s*=>)`),
			rule(KindFunction, inType, `^\s*(?:(?:public|private|protected|static|async|readonly|override|abstract|get|set)\s+)*\*?\s*(?P<name>#?`+jsID+`)\s*(?:<[^>(]*>)?\s*\([^)]*\)\s*(?::\s*[^{;]+)?\s*\{\s*$`),
			rule(KindFunction, inType, `^\s*(?:(?:public|private|protected|static|readonly|override)\s+)*(?P<name>#?`+jsID+`)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:\([^)]*\)|`+jsID+`)\s*(?::\s*[^=]+?)?\s*=>`),
			// An object literal that holds functions ("const api = {", "module.exports = {",
			// "export default {") is a body like a class's: its members become api.name().
			rule("impl", notInFunc, `^\s*(?:export\s+)?(?:const|let|var)\s+(?P<name>`+jsID+`)\s*(?::[^=]+)?=\s*\{\s*$`),
			rule("impl", notInFunc, `^\s*(?:module\.)?(?P<name>exports|export\s+default)\s*=?\s*\{\s*$`),
			rule(KindFunction, inType, `^\s*(?P<name>`+jsID+`)\s*:\s*(?:async\s+)?(?:function\b|(?:\([^)]*\)|`+jsID+`)\s*(?::\s*[^=]+?)?\s*=>)`),
		},
		imports:     res(`\bfrom\s+['"]([^'"]+)['"]`, `\brequire\(\s*['"]([^'"]+)['"]\s*\)`, `^\s*import\s+['"]([^'"]+)['"]`, `\bimport\(\s*['"]([^'"]+)['"]\s*\)`),
		selfs:       []string{"this"},
		importNames: jsImportNames,
	},
	{
		keywords: "synchronized default instanceof throws assert",
		name:     "java", family: "java", exts: []string{".java"},
		slashComments: true, dirVisible: true,
		defs: []defRule{
			rule(KindType, anywhere, `^\s*`+javaAnnot+javaMods+`*(?:class|interface|enum|record)\s+(?P<name>\w+)(?:\s*<[^{]*?>)?(?:\s*\([^)]*\))?(?:\s+extends\s+(?P<bases>[\w.<>, ]+?))?(?:\s+implements\s+(?P<impls>[\w.<>, ]+?))?\s*(?:\{|$)`),
			rule(KindFunction, inType, `^\s*`+javaAnnot+javaMods+`*(?:<[^>]+>\s+)?`+typeTok+`\s+(?P<name>\w+)\s*\(`),
			rule(KindFunction, inType, `^\s*`+javaAnnot+javaMods+`+(?P<name>\w+)\s*\(`),
		},
		imports: res(`^\s*import\s+(?:static\s+)?([\w.]+?)(?:\.\*)?\s*;`),
		selfs:   []string{"this"},
	},
	{
		keywords: "using lock when fixed checked unchecked default nameof foreach stackalloc",
		name:     "csharp", family: "csharp", exts: []string{".cs"},
		slashComments: true, dirVisible: true,
		defs: []defRule{
			rule(KindType, anywhere, `^\s*`+csMods+`*(?:class|interface|struct|enum|record)\s+(?P<name>\w+)(?:\s*<[^>{]*>)?(?:\s*\([^)]*\))?(?:\s*:\s*(?P<bases>[^{]+?))?\s*(?:where\b.*)?(?:\{|$)`),
			rule(KindFunction, inType, `^\s*`+csMods+`*`+typeTok+`\s+(?P<name>\w+)\s*(?:<[^>(]*>)?\s*\(`),
			rule(KindFunction, inType, `^\s*`+csMods+`+(?P<name>\w+)\s*\(`),
		},
		selfs: []string{"this"},
	},
	{
		keywords: "when fun init constructor object",
		name:     "kotlin", family: "kotlin", exts: []string{".kt", ".kts"},
		slashComments: true, dirVisible: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*`+ktMods+`fun\s+(?:<[^>]+>\s+)?(?:[\w.<>?]+\.)?(?P<name>\w+)\s*\(`),
			rule(KindType, anywhere, `^\s*`+ktMods+`(?:class|interface|object)\s+(?P<name>\w+)(?:\s*<[^>]*>)?(?:\s*(?:(?:private|internal|protected)\s+)?(?:constructor\s*)?\([^)]*\))?(?:\s*:\s*(?P<bases>[^{]+))?`),
		},
		imports: res(`^\s*import\s+([\w.]+)`),
		selfs:   []string{"this"},
	},
	{
		keywords: "fn impl match loop move mut ref unsafe async pub use mod where",
		name:     "rust", family: "rust", exts: []string{".rs"},
		slashComments: true, baseSep: "+",
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*`+rustVis+`(?:(?:default|const|async|unsafe|extern)\s+)*fn\s+(?P<name>\w+)`),
			rule(KindType, anywhere, `^\s*`+rustVis+`(?:unsafe\s+)?trait\s+(?P<name>\w+)(?:\s*<[^>]*>)?(?:\s*:\s*(?P<bases>[^{;(]+?))?\s*(?:where\b.*)?(?:\{|$)`),
			rule(KindType, anywhere, `^\s*`+rustVis+`(?:struct|enum|union)\s+(?P<name>\w+)`),
			rule("impl", anywhere, `^\s*(?:unsafe\s+)?impl(?:\s*<[^>]*>)?\s+(?:(?P<trait>[\w:]+)(?:<[^>]*>)?\s+for\s+)?(?P<name>[\w:]+)`),
		},
		imports: res(`^\s*`+rustVis+`(mod\s+\w+)\s*;`, `^\s*`+rustVis+`use\s+((?:crate|super|self)(?:::\w+)+)`),
		selfs:   []string{"self", "Self"},
	},
	{
		keywords: "defined alignof alignas decltype static_cast dynamic_cast const_cast reinterpret_cast typedef operator template typename noexcept",
		name:     "c", family: "c", exts: []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx"},
		slashComments: true, declIsNotDef: true,
		defs: []defRule{
			rule(KindType, anywhere, `^\s*(?:template\s*<[^>]*>\s*)?(?:typedef\s+)?(?:class|struct)\s+(?:[A-Z][A-Z0-9_]*\s+)?(?P<name>\w+)\s*(?:final\s*)?(?::\s*(?P<bases>[^{;]+?))?\s*(?:\{|$)`),
			rule(KindFunction, notInFunc, `^\s*(?:[\w:<>,\*&~\[\]]+[\s\*&]+)+?(?P<name>(?:\w+::)*~?\w+)\s*\([^;]*$`),
			rule(KindFunction, notInFunc, `^\s*(?P<name>(?:\w+::)+~?\w+)\s*\([^;]*$`),
		},
		imports: res(`^\s*#\s*include\s*"([^"]+)"`),
		selfs:   []string{"this"},
	},
	{
		keywords: "unless until elsif begin end then puts require require_relative raise attr_accessor attr_reader attr_writer",
		name:     "ruby", family: "ruby", exts: []string{".rb"},
		hashComments: true, quoteStrings: true, indentScoped: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*def\s+(?:self\.)?(?P<name>\w+[?!=]?)`),
			rule(KindType, anywhere, `^\s*(?:class|module)\s+(?P<name>[\w:]+)(?:\s*<\s*(?P<bases>[\w:]+))?`),
		},
		imports: res(`^\s*require_relative\s+['"]([^'"]+)['"]`),
		selfs:   []string{"self"},
	},
	{
		keywords: "echo isset unset empty array list print exit die require include require_once include_once fn use foreach",
		name:     "php", family: "php", exts: []string{".php"},
		hashComments: true, slashComments: true, quoteStrings: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*(?:(?:public|private|protected|static|final|abstract)\s+)*function\s+&?\s*(?P<name>\w+)\s*\(`),
			rule(KindType, anywhere, `^\s*(?:(?:final|abstract|readonly)\s+)*(?:class|interface|trait|enum)\s+(?P<name>\w+)(?:\s+extends\s+(?P<bases>[\w\\, ]+?))?(?:\s+implements\s+(?P<impls>[\w\\, ]+?))?\s*(?:\{|$)`),
		},
		imports: res(`\b(?:require|include)(?:_once)?\s*\(?\s*(?:__DIR__\s*\.\s*)?['"]([^'"]+)['"]`),
		selfs:   []string{"$this", "self", "static"},
	},
	{
		keywords: "guard func init switch where",
		name:     "swift", family: "swift", exts: []string{".swift"},
		slashComments: true, dirVisible: true,
		defs: []defRule{
			rule(KindFunction, anywhere, `^\s*`+swiftMods+`(?:class\s+)?func\s+(?P<name>\w+)`),
			rule(KindFunction, inType, `^\s*`+swiftMods+`(?P<name>init)[?!]?\s*\(`),
			rule(KindType, anywhere, `^\s*`+swiftMods+`(?:class|struct|enum|protocol|actor)\s+(?P<name>\w+)(?:\s*<[^>]*>)?(?:\s*:\s*(?P<bases>[^{]+?))?\s*(?:where\b.*)?(?:\{|$)`),
			rule("impl", anywhere, `^\s*`+swiftMods+`extension\s+(?P<name>[\w.]+)(?:\s*:\s*(?P<impls>[^{]+?))?\s*(?:where\b.*)?(?:\{|$)`),
		},
		selfs: []string{"self", "Self"},
	},
}

func init() {
	langSpecs = append(langSpecs,
		&langSpec{
			name: "scala", family: "scala", exts: []string{".scala", ".sc"},
			slashComments: true, tripleQuotes: true, dirVisible: true, baseSep: " with ",
			keywords: "match val var given using then",
			defs: []defRule{
				rule(KindFunction, anywhere, `^\s*`+scalaMods+`def\s+(?P<name>\w+)`),
				rule(KindType, anywhere, `^\s*`+scalaMods+`(?:class|trait|object|enum)\s+(?P<name>\w+)(?:\[[^\]]*\])?(?:\s*(?:private\s+)?\([^)]*\))?(?:\s+extends\s+(?P<bases>[^{:=]+?))?\s*(?:\{|:|$)`),
			},
			imports: res(`^\s*import\s+([\w.]+)`),
			selfs:   []string{"this"},
		},
		&langSpec{
			name: "dart", family: "dart", exts: []string{".dart"},
			slashComments: true, quoteStrings: true,
			keywords: "await async sync yield late final const var assert",
			defs: []defRule{
				rule(KindType, anywhere, `^\s*(?:(?:abstract|base|final|sealed|interface|mixin)\s+)*(?:class|mixin|enum|extension)\s+(?P<name>\w+)(?:<[^{]*?>)?(?:\s+extends\s+(?P<bases>[\w.<>, ]+?))?(?:\s+with\s+[\w.<>, ]+?)?(?:\s+implements\s+(?P<impls>[\w.<>, ]+?))?\s*(?:\{|$)`),
				rule(KindFunction, notInFunc, `^\s*(?:@\w+\s+)*(?:(?:static|final|const|late|external|abstract|override)\s+)*[\w.<>\[\],?]+\s+(?P<name>\w+)\s*(?:<[^>(]*>)?\s*\(`),
			},
			imports: res(`^\s*(?:import|export|part)\s+['"]([^'"]+)['"]`),
			selfs:   []string{"this"},
		},
		&langSpec{
			name: "lua", family: "lua", exts: []string{".lua", ".luau"},
			dashComments: true, quoteStrings: true, indentScoped: true,
			keywords: "function local end then elseif repeat until nil",
			defs: []defRule{
				rule(KindFunction, anywhere, `^\s*(?:local\s+)?function\s+(?P<name>[\w.:]+)\s*\(`),
				rule(KindFunction, anywhere, `^\s*(?:local\s+)?(?P<name>[\w.]+)\s*=\s*function\s*\(`),
			},
			imports: res(`\brequire\s*\(?\s*['"]([^'"]+)['"]`),
			selfs:   []string{"self"},
		},
		&langSpec{
			name: "shell", family: "shell", exts: []string{".sh", ".bash", ".zsh", ".bats"},
			hashComments: true, quoteStrings: true, bareCalls: true, noGlobal: true,
			keywords: "then fi do done elif esac function local export readonly declare typeset select until time",
			defs: []defRule{
				rule(KindFunction, anywhere, `^\s*(?:function\s+)?(?P<name>[A-Za-z_]\w*)\s*\(\s*\)`),
				rule(KindFunction, anywhere, `^\s*function\s+(?P<name>[A-Za-z_]\w*)\b`),
			},
			imports: res(`^\s*(?:source|\.)\s+(.+)$`),
		},
	)
}

const scalaMods = `(?:(?:override|private|protected|final|implicit|inline|lazy|abstract|sealed|case|open|transparent)(?:\[[^\]]*\])?\s+)*`

// sqlSpec only tells scrub how SQL is written; SQL has its own extractor.
var sqlSpec = &langSpec{name: "sql", family: "sql", dashComments: true, slashComments: true, quoteStrings: true, rawDQuotes: true}

// embeddedJSExts are single-file components whose <script> blocks are
// JavaScript or TypeScript.
var embeddedJSExts = map[string]bool{".vue": true, ".svelte": true}

// isParsedExt reports whether an extractor reads files with this extension.
func isParsedExt(ext string) bool {
	return ext == ".go" || ext == ".sql" || embeddedJSExts[ext] || specForExt(ext) != nil
}

var specByExt = map[string]*langSpec{}

func specForExt(ext string) *langSpec { return specByExt[ext] }

// Languages lists the languages the extractors read, for help and reports.
func Languages() []string {
	out := []string{"go"}
	for _, s := range langSpecs {
		out = append(out, s.name)
	}
	return append(out, "sql", "vue", "svelte")
}

// notDefinable are words a definition pattern can capture as a name without
// the line being a definition.
var notDefinable = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`if else elif for foreach while do switch case return catch try throw function
		sizeof typeof synchronized`) {
		m[w] = true
	}
	return m
}()

// commonKeywords precede "(" without being a call in every supported
// language. Each language adds its own (langSpec.keywords): a word that is a
// keyword in one language is an ordinary function name in another.
const commonKeywords = `if else elif for while do switch case return catch try finally throw function def class new
	sizeof typeof await yield delete lambda with assert except in of not and or is super this self`

// ruleNeeds gives, per language and in the order of its rules, the text
// every line a rule matches must contain ("" when there is none). It only
// saves time: a line without the text skips the rule's pattern.
var ruleNeeds = map[string][]string{
	"python":     {"def", "class"},
	"javascript": {"function", "class", "interface", "=", "(", "=>", "{", "export", ":"},
	"java":       {"", "(", "("},
	"csharp":     {"", "(", "("},
	"kotlin":     {"fun", ""},
	"rust":       {"fn", "trait", "", "impl"},
	"c":          {"", "(", "("},
	"ruby":       {"def", ""},
	"php":        {"function", ""},
	"swift":      {"func", "init", "", "extension"},
	"scala":      {"def", ""},
	"dart":       {"", "("},
	"lua":        {"function", "function"},
	"shell":      {"(", "function"},
}

// Declarations that state a variable's type. Types are taken to start with
// a capital letter, which keeps ordinary assignments and literals out.
var (
	// name: Type            (parameters, fields, locals)
	typedColon = regexp.MustCompile(`(?P<var>[A-Za-z_$][\w$]*)\s*\??\s*:\s*(?:&\s*(?:mut\s+)?|\*\s*)?(?:[\w.]+\.|(?:\w+::)+)?(?P<type>[A-Z][\w$]*)`)
	// Type name             (parameters, fields, locals)
	typedPrefix = regexp.MustCompile(`\b(?P<type>[A-Z][\w$]*)(?:<[^<>;(){}]*>)?(?:\[\])?[\s*&]+(?P<var>[a-z_$][\w$]*)\s*(?:[=;,)]|$)`)
	// name = new Type(
	typedNew = regexp.MustCompile(`(?P<var>[A-Za-z_$][\w$]*)\s*(?::[^=]*)?=\s*(?:await\s+)?new\s+(?:[\w.\\]+[.\\])?(?P<type>[A-Z][\w$]*)`)
	// name = Type(
	typedCtor = regexp.MustCompile(`(?P<var>[A-Za-z_$][\w$]*)\s*(?::[^=]*)?=\s*(?:[\w.]+\.)?(?P<type>[A-Z][\w$]*)\s*\(`)
	// name = Type.new
	typedRubyNew = regexp.MustCompile(`(?P<var>@?[A-Za-z_]\w*)\s*=\s*(?:\w+::)*(?P<type>[A-Z]\w*)\.new\b`)
	// name = Type::new(  /  name = Type {
	typedRustNew = regexp.MustCompile(`(?P<var>[A-Za-z_]\w*)\s*(?::[^=]*)?=\s*(?:\w+::)*(?P<type>[A-Z]\w*)\s*(?:::\s*\w+\s*\(|\{)`)
)

var typedByLang = map[string][]*regexp.Regexp{
	"python": {typedColon, typedCtor}, "javascript": {typedColon, typedNew}, "java": {typedPrefix, typedNew},
	"csharp": {typedPrefix, typedNew}, "kotlin": {typedColon, typedCtor}, "rust": {typedColon, typedRustNew},
	"c": {typedPrefix, typedNew}, "ruby": {typedRubyNew}, "php": {typedPrefix, typedNew}, "swift": {typedColon, typedCtor},
	"scala": {typedColon, typedNew, typedCtor}, "dart": {typedPrefix, typedCtor},
}

// The second init of this file (they run in order): finish every language,
// including the ones appended above.
func init() {
	for _, s := range langSpecs {
		for _, e := range s.exts {
			specByExt[e] = s
		}
		s.typed = typedByLang[s.name]
		needs := ruleNeeds[s.name]
		if len(needs) != len(s.defs) {
			panic("graphify: ruleNeeds out of step with the rules of " + s.name)
		}
		for i := range s.defs {
			s.defs[i].need = needs[i]
		}
		s.importNeed = map[string][]string{
			"python": {"import"}, "javascript": {"import", "from", "require"}, "java": {"import"}, "kotlin": {"import"},
			"rust": {"mod", "use"}, "c": {"include"}, "ruby": {"require"}, "php": {"require", "include"},
			"scala": {"import"}, "dart": {"import", "export", "part"}, "lua": {"require"}, "shell": {"source", "."},
		}[s.name]
		s.notCallable = map[string]bool{}
		for _, w := range strings.Fields(commonKeywords + " " + s.keywords) {
			s.notCallable[w] = true
		}
	}
}

var (
	jsBracesRe   = regexp.MustCompile(`\{([^}]*)\}`)
	jsStarRe     = regexp.MustCompile(`\*\s+as\s+(` + jsID + `)`)
	jsDefaultRe  = regexp.MustCompile(`^\s*import\s+(?:type\s+)?(` + jsID + `)\s*(?:,|from\b)`)
	jsRequireRe  = regexp.MustCompile(`(?:const|let|var)\s+(` + jsID + `)\s*=\s*(?:await\s+)?(?:require|import)\s*\(`)
	jsTypeOnlyRe = regexp.MustCompile(`^\s*import\s+type\b`)
	jsReexportRe = regexp.MustCompile(`^\s*export\b`)
	pyFromRe     = regexp.MustCompile(`^\s*from\s+\S+\s+import\s+(.*)$`)
	pyImportRe   = regexp.MustCompile(`^\s*import\s+(.*)$`)
)

// jsImportNames reads the names bound by an import or require statement
// (which may span lines; stmt is the text up to and including the module).
func jsImportNames(stmt string) (names, origs []string, typeOnly bool) {
	if jsReexportRe.MatchString(stmt) {
		return nil, nil, false // "export { a } from": binds nothing here
	}
	typeOnly = jsTypeOnlyRe.MatchString(stmt)
	add := func(name, orig string) {
		if identLike(name) {
			names, origs = append(names, name), append(origs, orig)
		}
	}
	if m := jsBracesRe.FindStringSubmatch(stmt); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "type "))
			orig, local := part, part
			if a, b, ok := strings.Cut(part, " as "); ok { // import { a as b }
				orig, local = strings.TrimSpace(a), strings.TrimSpace(b)
			} else if a, b, ok := strings.Cut(part, ":"); ok { // const { a: b } = require()
				orig, local = strings.TrimSpace(a), strings.TrimSpace(b)
			}
			add(local, orig)
		}
	}
	if m := jsStarRe.FindStringSubmatch(stmt); m != nil {
		add(m[1], "")
	}
	if m := jsDefaultRe.FindStringSubmatch(stmt); m != nil {
		add(m[1], "")
	}
	if m := jsRequireRe.FindStringSubmatch(stmt); m != nil {
		add(m[1], "")
	}
	return names, origs, typeOnly
}

// pythonImportNames reads "from m import a, b as c" and "import m.n as k, o".
func pythonImportNames(stmt string) (names, origs []string, _ bool) {
	if m := pyFromRe.FindStringSubmatch(stmt); m != nil {
		for _, part := range strings.Split(strings.Trim(strings.TrimSpace(m[1]), "()\\"), ",") {
			orig, local, _ := strings.Cut(strings.TrimSpace(part), " as ")
			orig = strings.TrimSpace(orig)
			if local = strings.TrimSpace(local); local == "" {
				local = orig
			}
			if identLike(local) && identLike(orig) {
				names, origs = append(names, local), append(origs, orig)
			}
		}
		return names, origs, false
	}
	if m := pyImportRe.FindStringSubmatch(stmt); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			mod, local, aliased := strings.Cut(strings.TrimSpace(part), " as ")
			mod, local = strings.TrimSpace(mod), strings.TrimSpace(local)
			if aliased {
				if identLike(local) {
					names, origs = append(names, local), append(origs, "")
				}
				continue
			}
			// "import a.b" binds a; a call a.b.f() is seen as b.f().
			for _, seg := range []string{mod[:strings.IndexAny(mod+".", ".")], lastSegment(mod)} {
				if identLike(seg) && (len(names) == 0 || names[len(names)-1] != seg) {
					names, origs = append(names, seg), append(origs, "")
				}
			}
		}
	}
	return names, origs, false
}

func identLike(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
