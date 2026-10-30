// Package workflows implements the Workflow subsystem: the
// workflow descriptor / meta schema, the discovery+precedence registry over the
// user/project/plugin `workflows/` directories, and a deterministic executor
// that models the phase() / parallel() / pipeline() orchestration primitives
// over an injectable agent-step runner.
//
// # What a workflow is
//
// A workflow is a plain-JavaScript script that orchestrates many subagents
// deterministically. Every script begins with a PURE-LITERAL
// `export const meta = {...}` header (name, description, optional whenToUse and
// phases), followed by a body that drives the orchestration through the globals
// agent() / parallel() / pipeline() / phase() / log() / workflow() plus `args`
// and `budget`. Scripts run in a locked-down VM (no Date.now/Math.random, no
// filesystem), returning a JSON-able value that becomes the tool result.
//
// # What is implemented
//
// The package provides:
//
//   - the meta descriptor schema and its validation (workflow.go),
//   - the discovery directories and source precedence (registry.go),
//   - the execution model — phase / parallel (barrier) / pipeline (no barrier),
//     the concurrency cap and null-on-failure aggregation — as a pure executor
//     over an injectable StepRunner seam (run.go),
//   - execution of the JavaScript body under an embedded goja VM
//     (jsruntime.go RunScriptResult), whose injected globals drive
//     the run.go executor.
//
// Parsing reads the `meta` header statically (never executed) and hands
// back the remaining body as text; the model-facing Workflow tool
// (internal/agenttool/workflow.go) parses a script and then runs its body via
// RunScriptResult over a StepRunner that spawns real subagents. The nested
// workflow() global is not injected (see jsruntime.go).
//
// The executor caps concurrency at min(16, max(2, cpus-2)), has a 1000-agent
// backstop and a budget, and builds each workflow subagent via the same spawn
// path as the Agent tool.
package workflows

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxScriptBytes is the per-script byte cap; scripts larger than this are
// rejected at parse and skipped at discovery.
const MaxScriptBytes = 524288

// MaxFanoutItems is the most items a single parallel()/pipeline() call accepts;
// passing more is an explicit error, not a silent truncation.
const MaxFanoutItems = 4096

// MaxAgentCalls is the lifetime cap on agent() calls across one workflow run — a
// runaway-loop backstop set far above any real workflow.
const MaxAgentCalls = 1000

// Phase is one entry of meta.phases: a progress group title with optional detail
// and an optional per-phase model override. Titles are matched exactly against
// phase() calls in the body.
type Phase struct {
	Title  string
	Detail string
	Model  string
}

// Meta is the validated `export const meta = {...}` header.
type Meta struct {
	Name        string
	Description string
	Title       string
	WhenToUse   string
	Phases      []Phase
}

// ParseResult is the output of Parse: the validated meta plus the script body
// that follows the meta declaration (kept as opaque text — not executed).
type ParseResult struct {
	Meta Meta
	// ScriptBody is everything after `export const meta = {...}`, with a leading
	// `;`/whitespace+newline trimmed. It is JavaScript and is NOT executed here.
	ScriptBody string
}

// reservedKeys are object keys rejected in meta (prototype-pollution guard).
var reservedKeys = map[string]bool{"__proto__": true, "constructor": true, "prototype": true}

// metaHeader matches the fast path: optional leading trivia, then
// `export const meta =`, positioned just before the opening `{`.
var metaHeader = regexp.MustCompile(`^(?:` + jsSpaceClass +
	`|//[^\n\r\x{2028}\x{2029}]*(?:[\n\r\x{2028}\x{2029}]|$)|/\*[^*]*\*+(?:[^/*][^*]*\*+)*/)*` +
	`export` + jsSpaceClass + `+const` + jsSpaceClass + `+meta` + jsSpaceClass + `*=` + jsSpaceClass + `*`)

// jsSpaceClass is JavaScript's \s as an RE2 character class.
const jsSpaceClass = `[\s\v\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

// Parse validates a workflow script's meta header and returns the meta plus the
// (unexecuted) body. This is the discovery path, which never parses or runs
// the body.
//
// There is no full JS parse here: leading comments are accepted by the
// fast-path trivia above, and body syntax is checked when the goja runtime
// compiles the script at run time (jsruntime.go), where a syntax error is
// reported too.
func Parse(script string) (ParseResult, error) {
	if len(script) > MaxScriptBytes {
		return ParseResult{}, fmt.Errorf("script exceeds %d bytes", MaxScriptBytes)
	}
	loc := metaHeader.FindStringIndex(script)
	if loc == nil {
		return ParseResult{}, fmt.Errorf("`export const meta = { name, description, phases }` must be the FIRST statement in the script")
	}
	objStart := loc[1]
	if objStart >= len(script) || script[objStart] != '{' {
		return ParseResult{}, fmt.Errorf("meta must be an object literal")
	}
	val, end, err := parseValue(script, objStart)
	if err != nil {
		return ParseResult{}, fmt.Errorf("meta must be a pure literal: %w", err)
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return ParseResult{}, fmt.Errorf("meta must be an object literal")
	}
	bodyStart, ok := asiEnd(script, end)
	if !ok {
		return ParseResult{}, fmt.Errorf("meta declaration must end with a statement terminator (`;` or newline)")
	}
	meta, err := validateMeta(obj)
	if err != nil {
		return ParseResult{}, err
	}
	return ParseResult{Meta: meta, ScriptBody: trimBody(script[bodyStart:])}, nil
}

// validateMeta validates the meta fields and normalizes the phases.
func validateMeta(o map[string]any) (Meta, error) {
	name, _ := o["name"].(string)
	if name == "" {
		return Meta{}, fmt.Errorf("meta.name must be a non-empty string")
	}
	desc, _ := o["description"].(string)
	if desc == "" {
		return Meta{}, fmt.Errorf("meta.description must be a non-empty string")
	}
	m := Meta{Name: name, Description: desc}
	if t, ok := o["title"].(string); ok && t != "" {
		m.Title = t
	}
	if w, ok := o["whenToUse"].(string); ok {
		m.WhenToUse = w
	}
	m.Phases = normalizePhases(o["phases"])
	return m, nil
}

// normalizePhases keeps only object entries with a string `title`;
// returns nil when the input is not an array or yields no usable entries.
func normalizePhases(v any) []Phase {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []Phase
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		title, ok := m["title"].(string)
		if !ok {
			continue
		}
		p := Phase{Title: title}
		if d, ok := m["detail"].(string); ok {
			p.Detail = d
		}
		if md, ok := m["model"].(string); ok {
			p.Model = md
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// trimBody drops a leading `;`/whitespace line then trims leading
// whitespace.
func trimBody(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		if strings.TrimLeft(s[:i], ";\t\r\f\v ") == "" {
			s = s[i+1:]
		}
	}
	return strings.TrimLeft(s, " \t\r\n\f\v")
}
