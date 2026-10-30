// Package plugineval runs a plugin's eval suite: cases (case.yaml, or
// prompt.md + graders/*.md) under the plugin's eval directory are run as
// headless gocode sessions in a throwaway sandbox, graded, and reported as an
// aggregate result (aggregate-result.json, --json) and a summary table.
//
// It implements `gocode plugin eval`: the runner, the case schema, prose
// cases, case discovery, graders, the trace parser, the child argv, the run
// sandbox, scaffolds, tool grants, advisories, the eval-dir resolution,
// `eval init --bare`, scoring, the report, the summary table and the
// camelCase result document.
//
// Limitations: reports stay local; artifact_publish and growthbook_overrides
// in a case are accepted but not applied; shell tool grants are refused (no
// sandbox confines a granted shell); MCP mocks are not served (in record mode
// the plugin's real servers are withheld like an unmocked server); and
// context.history_file is not replayed (resume is by session id).
package plugineval

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
)

const (
	// DefaultEvalDir is the eval directory below a plugin.
	DefaultEvalDir = "evals"
	// maxSchemaMajor is the newest case.yaml major version understood.
	maxSchemaMajor = 1
	// proseSchemaVersion is the schema_version of a prose-only case.
	proseSchemaVersion = "1.1"
	// blankPrompt / blankCriteria are the `eval init --bare` placeholders
	//.
	blankPrompt   = "TODO: describe what the agent should do"
	blankCriteria = "TODO: describe what a successful response looks like"
	// maxCaseYAMLBytes caps case.yaml; maxProseBytes caps prose .md
	// files; maxGraderFiles caps graders/*.md.
	maxCaseYAMLBytes = 1 << 20
	maxProseBytes    = 1 << 20
	maxGraderFiles   = 256
)

// BarePromptTemplate / BareGraderTemplate are the files `plugin eval init
// --bare <name>` writes.
const (
	BarePromptTemplate = "---\nmax_turns: 10\nallowed_tools: [Read, Glob, Grep, Skill]\n---\n\n" + blankPrompt + "\n"
	BareGraderTemplate = "---\ntype: llm\nweight: 1\n---\n\n" + blankCriteria + "\n"
)

// Target is what a regex grader matches or an llm grader shows the judge: one
// of trace | last_message | files | mock_calls, or a file the run wrote.
type Target struct {
	Kind string // "trace", "last_message", "files", "mock_calls", "file"
	Path string // for Kind == "file"
}

func (t Target) String() string {
	if t.Kind == "file" {
		return "file " + t.Path
	}
	return t.Kind
}

// ToolRef names a tool call in a tool_order grader.
type ToolRef struct {
	Tool       string
	InputMatch string
}

// Grader is one grader of a case.
type Grader struct {
	Type   string
	Name   string
	Weight float64
	// Arm is "", "with-only" or "both".
	Arm string

	// regex
	Target  Target
	Pattern string
	Flags   string
	Match   string // contains | not_contains | count:N
	// tool_order
	Before, After ToolRef
	// tool_used
	Tool       string
	InputMatch string
	Min, Max   *int
	// file_exists
	Path   string
	Exists bool
	// llm (+ baseline)
	Criteria string
	Focus    Target
	// baseline
	BaselineFile string

	// config is the grader as written minus name/type/weight (the result
	// document's `config`).
	config map[string]any
}

// Case is a parsed, validated eval case.
type Case struct {
	SchemaVersion   string
	Name            string
	Description     string
	Tags            []string
	Plugins         []string // nil when the case does not declare plugins
	ScaffoldScript  string
	HistoryFile     string
	AddDirs         []string
	Prompt          string
	MaxTurns        int
	TimeoutSeconds  int
	Model           string
	AllowedTools    []string
	DisallowedTools []string
	AppendSystem    string
	Env             map[string]string
	// ArtifactPublish / GrowthbookOverrides are accepted for compatibility;
	// both need gocode-hosted services and are never applied (see Advisories).
	ArtifactPublish     bool
	GrowthbookOverrides map[string]any
	Runs                int
	Graders             []Grader
	ExpectedOutcome     string

	// Resolved by discovery.
	CaseFile   string
	CaseDir    string
	CaseSource string // case_yaml | prose | mixed
	PluginDirs []string
}

// caseError is a validation failure of a case file.
type caseError struct{ msg string }

func (e *caseError) Error() string { return e.msg }

// issue collects validation issues ("path: message").
type issues []string

func (is *issues) add(path, format string, a ...any) {
	p := path
	if p == "" {
		p = "(root)"
	}
	*is = append(*is, fmt.Sprintf("%s: %s", p, fmt.Sprintf(format, a...)))
}

// ParseCase validates a case document.
func ParseCase(doc any) (*Case, error) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, &caseError{"case.yaml must be a YAML object"}
	}
	sv, ok := m["schema_version"].(string)
	if !ok {
		return nil, &caseError{`missing required field schema_version (e.g. "1.0")`}
	}
	major, err := strconv.Atoi(strings.SplitN(sv, ".", 2)[0])
	if err != nil {
		return nil, &caseError{fmt.Sprintf("schema_version %q is not a valid version string", sv)}
	}
	if major > maxSchemaMajor {
		return nil, &caseError{fmt.Sprintf("schema_version %q requires a newer gocode (this binary supports up to %d.x)", sv, maxSchemaMajor)}
	}
	var is issues
	c := &Case{SchemaVersion: sv, MaxTurns: 10, TimeoutSeconds: 300, Runs: 3, Env: map[string]string{}}
	c.Name = reqString(&is, m, "name", "")
	if c.Name == "" && len(is) == 0 {
		is.add("name", "String must contain at least 1 character(s)")
	}
	c.Description = optString(&is, m, "description", "")
	c.Tags = stringList(&is, m, "tags", "")
	if v, ok := m["plugins"]; ok && v != nil {
		c.Plugins = stringList(&is, m, "plugins", "")
		if c.Plugins == nil {
			c.Plugins = []string{}
		}
	}
	if ctx, ok := objectField(&is, m, "context", ""); ok {
		c.ScaffoldScript = optString(&is, ctx, "scaffold_script", "context.")
		c.HistoryFile = optString(&is, ctx, "history_file", "context.")
		c.AddDirs = stringList(&is, ctx, "add_dirs", "context.")
	}
	ex, hasEx := objectField(&is, m, "execution", "")
	if !hasEx {
		if _, present := m["execution"]; !present {
			is.add("execution", "Required")
		}
		ex = map[string]any{}
	}
	c.Prompt = optString(&is, ex, "prompt", "execution.")
	c.MaxTurns = intField(&is, ex, "max_turns", "execution.", 10, 1, 200)
	c.TimeoutSeconds = intField(&is, ex, "timeout_seconds", "execution.", 300, 1, 3600)
	c.Model = optString(&is, ex, "model", "execution.")
	c.AllowedTools = stringList(&is, ex, "allowed_tools", "execution.")
	c.DisallowedTools = stringList(&is, ex, "disallowed_tools", "execution.")
	if v, ok := ex["artifact_publish"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			is.add("execution.artifact_publish", "Expected boolean, received %s", typeName(v))
		}
		c.ArtifactPublish = b
	}
	if v, ok := ex["growthbook_overrides"]; ok && v != nil {
		if gm, isMap := v.(map[string]any); isMap {
			c.GrowthbookOverrides = gm
		} else {
			is.add("execution.growthbook_overrides", "Expected object, received %s", typeName(v))
		}
	}
	c.AppendSystem = optString(&is, ex, "append_system_prompt", "execution.")
	if v, ok := ex["env"]; ok && v != nil {
		em, isMap := v.(map[string]any)
		if !isMap {
			is.add("execution.env", "Expected object, received %s", typeName(v))
		}
		for k, val := range em {
			s, isStr := val.(string)
			if !isStr {
				is.add("execution.env."+k, "Expected string, received %s", typeName(val))
				continue
			}
			c.Env[k] = s
		}
	}
	c.Runs = intField(&is, m, "runs", "", 3, 1, 50)
	c.ExpectedOutcome = optString(&is, m, "expected_outcome", "")
	gl, hasGraders := m["graders"].([]any)
	switch {
	case m["graders"] == nil:
		is.add("graders", "Required")
	case !hasGraders:
		is.add("graders", "Expected array, received %s", typeName(m["graders"]))
	case len(gl) == 0:
		is.add("graders", "Array must contain at least 1 element(s)")
	}
	seen := map[string]bool{}
	for i, g := range gl {
		gr := parseGrader(&is, g, fmt.Sprintf("graders.%d", i))
		if gr == nil {
			continue
		}
		if seen[gr.Name] {
			is.add("graders", "duplicate grader name %q", gr.Name)
		}
		seen[gr.Name] = true
		c.Graders = append(c.Graders, *gr)
	}
	if len(is) > 0 {
		return nil, &caseError{"invalid case.yaml:\n  " + strings.Join(is, "\n  ")}
	}
	if strings.TrimSpace(c.Prompt) == "" {
		if c.HistoryFile != "" {
			return nil, &caseError{"context.history_file requires execution.prompt (the resumed session needs a next turn)"}
		}
		return nil, &caseError{"execution.prompt is required (a prompt.md body, or execution.prompt in case.yaml)"}
	}
	if strings.Contains(c.Prompt, blankPrompt) {
		return nil, &caseError{fmt.Sprintf("the prompt is still the blank `init` template — replace the %q line with the user prompt to test (in prompt.md, or in execution.prompt if the case is written in case.yaml)", blankPrompt)}
	}
	for _, g := range c.Graders {
		text := g.Criteria
		if g.Type == "regex" {
			text = g.Pattern
		}
		if strings.Contains(text, blankCriteria) {
			return nil, &caseError{fmt.Sprintf("grader %q is still the blank `init` template — replace the %q line with concrete pass criteria", g.Name, blankCriteria)}
		}
	}
	return c, nil
}

var (
	jsFlagsRe = regexp.MustCompile(`^[dgimsuvy]*$`)
	countRe   = regexp.MustCompile(`^count:\d+$`)
)

// graderKeys are the keys each grader type accepts (the .strict() objects).
var graderKeys = map[string][]string{
	"regex":       {"type", "name", "target", "pattern", "flags", "match", "weight", "arm"},
	"tool_order":  {"type", "name", "before", "after", "weight", "arm"},
	"tool_used":   {"type", "name", "tool", "input_match", "min", "max", "weight", "arm"},
	"file_exists": {"type", "name", "path", "exists", "weight", "arm"},
	"llm":         {"type", "name", "criteria", "focus", "weight", "arm"},
	"baseline":    {"type", "name", "baseline_file", "criteria", "weight", "arm"},
}

// parseGrader validates one grader.
func parseGrader(is *issues, v any, path string) *Grader {
	m, ok := v.(map[string]any)
	if !ok {
		is.add(path, "Expected object, received %s", typeName(v))
		return nil
	}
	t, _ := m["type"].(string)
	keys, known := graderKeys[t]
	if !known {
		is.add(path+".type", "Invalid discriminator value. Expected 'regex' | 'tool_order' | 'tool_used' | 'file_exists' | 'llm' | 'baseline'")
		return nil
	}
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	var extra []string
	for k := range m {
		if !allowed[k] {
			extra = append(extra, "'"+k+"'")
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		is.add(path, "Unrecognized key(s) in object: %s", strings.Join(extra, ", "))
	}
	n := len(*is)
	g := &Grader{Type: t, Weight: 1, config: map[string]any{}}
	g.Name = reqString(is, m, "name", path+".")
	if w, ok := m["weight"]; ok && w != nil {
		f, isNum := toFloat(w)
		if !isNum {
			is.add(path+".weight", "Expected number, received %s", typeName(w))
		} else if f <= 0 {
			is.add(path+".weight", "Number must be greater than 0")
		} else {
			g.Weight = f
		}
	}
	if a, ok := m["arm"]; ok && a != nil {
		s, _ := a.(string)
		if s != "with-only" && s != "both" {
			is.add(path+".arm", "Invalid enum value. Expected 'with-only' | 'both'")
		}
		g.Arm = s
	}
	switch t {
	case "regex":
		g.Target = parseTarget(is, m, "target", path+".")
		g.Pattern = reqString(is, m, "pattern", path+".")
		g.Flags = optString(is, m, "flags", path+".")
		if !jsFlagsRe.MatchString(g.Flags) {
			is.add(path+".flags", "must be JS RegExp flags (d g i m s u v y)")
		}
		g.Match = optString(is, m, "match", path+".")
		if g.Match == "" {
			g.Match = "contains"
		}
		if g.Match != "contains" && g.Match != "not_contains" && !countRe.MatchString(g.Match) {
			is.add(path+".match", "must be contains | not_contains | count:N")
		}
	case "tool_order":
		g.Before = parseToolRef(is, m["before"], path+".before")
		g.After = parseToolRef(is, m["after"], path+".after")
	case "tool_used":
		g.Tool = reqString(is, m, "tool", path+".")
		g.InputMatch = optString(is, m, "input_match", path+".")
		g.Min = optNonNegInt(is, m, "min", path+".")
		g.Max = optNonNegInt(is, m, "max", path+".")
	case "file_exists":
		g.Path = reqString(is, m, "path", path+".")
		g.Exists = true
		if v, ok := m["exists"]; ok && v != nil {
			b, isBool := v.(bool)
			if !isBool {
				is.add(path+".exists", "Expected boolean, received %s", typeName(v))
			}
			g.Exists = b
		}
	case "llm":
		g.Criteria = reqString(is, m, "criteria", path+".")
		g.Focus = parseTarget(is, m, "focus", path+".")
	case "baseline":
		g.BaselineFile = reqString(is, m, "baseline_file", path+".")
		g.Criteria = reqString(is, m, "criteria", path+".")
	}
	if len(*is) > n {
		return nil
	}
	for k, v := range m {
		if k != "name" && k != "type" && k != "weight" {
			g.config[k] = v
		}
	}
	// The parsed defaults show in the result's config.
	switch t {
	case "regex":
		g.config["target"] = targetConfig(g.Target)
		g.config["flags"] = g.Flags
		g.config["match"] = g.Match
	case "file_exists":
		g.config["exists"] = g.Exists
	case "llm":
		g.config["focus"] = targetConfig(g.Focus)
	}
	return g
}

func targetConfig(t Target) any {
	if t.Kind == "file" {
		return map[string]any{"source": "file", "path": t.Path}
	}
	return t.Kind
}

// parseTarget: trace | last_message | files | mock_calls | {source:
// file, path}; default last_message.
func parseTarget(is *issues, m map[string]any, key, prefix string) Target {
	v, ok := m[key]
	if !ok || v == nil {
		return Target{Kind: "last_message"}
	}
	switch t := v.(type) {
	case string:
		switch t {
		case "trace", "last_message", "files", "mock_calls":
			return Target{Kind: t}
		}
	case map[string]any:
		src, _ := t["source"].(string)
		p, pok := t["path"].(string)
		if src == "file" && pok && len(t) == 2 {
			return Target{Kind: "file", Path: p}
		}
	}
	is.add(prefix+key, "Invalid input")
	return Target{}
}

// parseToolRef: a tool name or {tool, input_match?}.
func parseToolRef(is *issues, v any, path string) ToolRef {
	switch t := v.(type) {
	case string:
		return ToolRef{Tool: t}
	case map[string]any:
		tool, ok := t["tool"].(string)
		if !ok {
			is.add(path+".tool", "Required")
			return ToolRef{}
		}
		r := ToolRef{Tool: tool}
		if im, has := t["input_match"]; has && im != nil {
			s, ok := im.(string)
			if !ok {
				is.add(path+".input_match", "Expected string, received %s", typeName(im))
			}
			r.InputMatch = s
		}
		for k := range t {
			if k != "tool" && k != "input_match" {
				is.add(path, "Unrecognized key(s) in object: '%s'", k)
			}
		}
		return r
	case nil:
		is.add(path, "Required")
	default:
		is.add(path, "Invalid input")
	}
	return ToolRef{}
}

func reqString(is *issues, m map[string]any, key, prefix string) string {
	v, ok := m[key]
	if !ok || v == nil {
		is.add(prefix+key, "Required")
		return ""
	}
	s, ok := v.(string)
	if !ok {
		is.add(prefix+key, "Expected string, received %s", typeName(v))
	}
	return s
}

func optString(is *issues, m map[string]any, key, prefix string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		is.add(prefix+key, "Expected string, received %s", typeName(v))
	}
	return s
}

func stringList(is *issues, m map[string]any, key, prefix string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		is.add(prefix+key, "Expected array, received %s", typeName(v))
		return nil
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			is.add(fmt.Sprintf("%s%s.%d", prefix, key, i), "Expected string, received %s", typeName(e))
			continue
		}
		out = append(out, s)
	}
	return out
}

func objectField(is *issues, m map[string]any, key, prefix string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false
	}
	o, ok := v.(map[string]any)
	if !ok {
		is.add(prefix+key, "Expected object, received %s", typeName(v))
		return nil, false
	}
	return o, true
}

func intField(is *issues, m map[string]any, key, prefix string, def, lo, hi int) int {
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	f, isNum := toFloat(v)
	switch {
	case !isNum:
		is.add(prefix+key, "Expected number, received %s", typeName(v))
	case f != math.Trunc(f):
		is.add(prefix+key, "Expected integer, received float")
	case f < float64(lo):
		is.add(prefix+key, "Number must be greater than 0")
	case f > float64(hi):
		is.add(prefix+key, "Number must be less than or equal to %d", hi)
	default:
		return int(f)
	}
	return def
}

func optNonNegInt(is *issues, m map[string]any, key, prefix string) *int {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	f, isNum := toFloat(v)
	if !isNum || f != math.Trunc(f) || f < 0 {
		is.add(prefix+key, "Expected a non-negative integer")
		return nil
	}
	n := int(f)
	return &n
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if _, ok := toFloat(v); ok {
		return "number"
	}
	return "unknown"
}

// proseCase is a parsed prose case: the prompt.md frontmatter partitioned into top
// and execution keys, the prompt body and the graders/*.md graders.
type proseCase struct {
	top       map[string]any
	execution map[string]any
	prompt    *string
	graders   []any
}

// topKeys / executionKeys are Cs / As.
var (
	topKeys       = []string{"schema_version", "name", "description", "tags", "plugins", "runs", "expected_outcome"}
	executionKeys = []string{"model", "max_turns", "timeout_seconds", "allowed_tools", "disallowed_tools", "artifact_publish", "growthbook_overrides", "append_system_prompt", "env"}
)

func inList(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// loadProseCase: nil when neither prompt.md nor graders/*.md exist.
func loadProseCase(dir string) (*proseCase, error) {
	promptPath := filepath.Join(dir, "prompt.md")
	promptText, err := readCapped(promptPath, maxProseBytes, "prose .md")
	if err != nil {
		return nil, err
	}
	graderFiles, err := listGraderFiles(filepath.Join(dir, "graders"))
	if err != nil {
		return nil, err
	}
	if promptText == nil && len(graderFiles) == 0 {
		return nil, nil
	}
	pc := &proseCase{top: map[string]any{}, execution: map[string]any{}}
	if promptText != nil {
		fm, body, _, err := splitProse(*promptText, promptPath)
		if err != nil {
			return nil, err
		}
		if b := strings.TrimSpace(body); b != "" {
			pc.prompt = &b
		}
		for _, k := range sortedKeys(fm) {
			switch {
			case inList(topKeys, k):
				pc.top[k] = fm[k]
			case inList(executionKeys, k):
				pc.execution[k] = fm[k]
			default:
				return nil, fmt.Errorf("%s: unknown frontmatter key %q (expected one of: %s)", "prompt.md", k, strings.Join(append(append([]string{}, topKeys...), executionKeys...), ", "))
			}
		}
	}
	for _, gf := range graderFiles {
		text, err := readCapped(gf, maxProseBytes, "prose .md")
		if err != nil {
			return nil, err
		}
		if text == nil {
			return nil, fmt.Errorf("%s vanished from %s/ while being read; try again", filepath.Base(gf), filepath.Base(filepath.Dir(gf)))
		}
		fm, body, has, err := splitProse(*text, gf)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		t, ok := fm["type"].(string)
		if !ok {
			rel, _ := filepath.Rel(dir, gf)
			return nil, fmt.Errorf("%s: frontmatter must include \"type:\" (regex | tool_order | tool_used | file_exists | llm | baseline)", rel)
		}
		g := map[string]any{"name": strings.TrimSuffix(filepath.Base(gf), ".md")}
		for k, v := range fm {
			g[k] = v
		}
		bodyKey := map[string]string{"llm": "criteria", "baseline": "criteria", "regex": "pattern"}[t]
		if b := strings.TrimSpace(body); bodyKey != "" && g[bodyKey] == nil && b != "" {
			g[bodyKey] = b
		}
		pc.graders = append(pc.graders, g)
	}
	if pc.prompt == nil && len(pc.graders) == 0 && len(pc.top) == 0 && len(pc.execution) == 0 {
		return nil, nil
	}
	return pc, nil
}

// mergeProse: case.yaml (or a fresh prose skeleton) overlaid with the
// prose case.
func mergeProse(base map[string]any, pc *proseCase, dir string) map[string]any {
	if base == nil {
		base = map[string]any{"schema_version": proseSchemaVersion, "name": filepath.Base(dir)}
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range pc.top {
		out[k] = v
	}
	ex := map[string]any{}
	if be, ok := base["execution"].(map[string]any); ok {
		for k, v := range be {
			ex[k] = v
		}
	}
	baseDis, hasBase := ex["disallowed_tools"]
	for k, v := range pc.execution {
		ex[k] = v
	}
	proseDis, hasProse := pc.execution["disallowed_tools"]
	if hasBase || hasProse {
		ex["disallowed_tools"] = mergeDisallowed(baseDis, hasBase, proseDis, hasProse)
	}
	if pc.prompt != nil {
		ex["prompt"] = *pc.prompt
	}
	out["execution"] = ex
	if len(pc.graders) > 0 {
		var gs []any
		if bg, ok := base["graders"].([]any); ok {
			gs = append(gs, bg...)
		}
		out["graders"] = append(gs, pc.graders...)
	}
	return out
}

// mergeDisallowed: the two lists are concatenated.
func mergeDisallowed(a any, hasA bool, b any, hasB bool) any {
	if !hasA {
		return b
	}
	if !hasB {
		return a
	}
	al, aok := a.([]any)
	if !aok {
		return a
	}
	bl, bok := b.([]any)
	if !bok {
		return b
	}
	return append(append([]any{}, al...), bl...)
}

// splitProse: an optional `---` YAML frontmatter block.
func splitProse(text, path string) (fm map[string]any, body string, has bool, err error) {
	block, rest, ok := frontmatter.Split(text)
	if !ok {
		t := strings.TrimPrefix(text, "\ufeff")
		if strings.HasPrefix(t, "---\n") || strings.HasPrefix(t, "---\r\n") || strings.HasPrefix(t, "--- \n") {
			return nil, "", false, fmt.Errorf("%s: unterminated frontmatter (opening \"---\" with no closing \"---\")", filepath.Base(path))
		}
		return map[string]any{}, t, false, nil
	}
	v, perr := frontmatter.ParseValue(block)
	if perr != nil {
		return nil, "", false, fmt.Errorf("%s: invalid YAML frontmatter: %v", filepath.Base(path), perr)
	}
	if v == nil {
		return map[string]any{}, rest, true, nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, "", false, fmt.Errorf("%s: frontmatter must be a YAML object", filepath.Base(path))
	}
	return m, rest, true, nil
}

// readCapped reads a regular file of at most max bytes; nil when absent.
func readCapped(path string, max int64, label string) (*string, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if lst, lerr := os.Lstat(path); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s is a symbolic link whose target does not exist — not read", filepath.Base(path))
			}
			return nil, nil
		}
		return nil, fmt.Errorf("%s is unreadable (%v)", filepath.Base(path), err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes (cap: %d)", filepath.Base(path), st.Size(), max)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable (%v)", filepath.Base(path), err)
	}
	s := string(b)
	_ = label
	return &s, nil
}

// listGraderFiles: graders/*.md, sorted, at most 256.
func listGraderFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if lst, lerr := os.Lstat(dir); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s/ is a symbolic link whose target does not exist", filepath.Base(dir))
			}
			return nil, nil
		}
		var pe *os.PathError
		if errors.As(err, &pe) && strings.Contains(err.Error(), "not a directory") {
			return nil, fmt.Errorf("%s is not a directory", filepath.Base(dir))
		}
		return nil, fmt.Errorf("%s/ is unreadable (%v)", filepath.Base(dir), err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			continue
		}
		out = append(out, p)
	}
	if len(out) > maxGraderFiles {
		return nil, fmt.Errorf("%s/ has %d .md entries (cap: %d)", filepath.Base(dir), len(out), maxGraderFiles)
	}
	sort.Strings(out)
	return out, nil
}

// loadCaseYAML: nil when there is no case.yaml.
func loadCaseYAML(path string) (map[string]any, error) {
	text, err := readCapped(path, maxCaseYAMLBytes, "case.yaml")
	if err != nil || text == nil {
		return nil, err
	}
	v, err := frontmatter.ParseValue(strings.TrimPrefix(*text, "\ufeff"))
	if err != nil {
		return nil, fmt.Errorf("YAML parse failed: %v", err)
	}
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("case.yaml must be a YAML object")
	}
	return m, nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
