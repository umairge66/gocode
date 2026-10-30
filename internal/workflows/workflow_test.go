package workflows

import (
	"reflect"
	"testing"
)

func TestParseMetaCanonical(t *testing.T) {
	// The canonical review-changes header from the authoring reference,
	// followed by a body that must come back verbatim as
	// opaque, unexecuted text.
	script := `export const meta = {
  name: 'review-changes',
  description: 'Review changed files across dimensions, verify each finding',
  phases: [{ title: 'Review' }, { title: 'Verify', detail: 'adversarial' }],
}
const DIMENSIONS = [{key: 'bugs'}]
return { confirmed: DIMENSIONS }`

	res, err := Parse(script)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Meta.Name != "review-changes" {
		t.Errorf("name = %q", res.Meta.Name)
	}
	if res.Meta.Description != "Review changed files across dimensions, verify each finding" {
		t.Errorf("description = %q", res.Meta.Description)
	}
	want := []Phase{{Title: "Review"}, {Title: "Verify", Detail: "adversarial"}}
	if !reflect.DeepEqual(res.Meta.Phases, want) {
		t.Errorf("phases = %+v, want %+v", res.Meta.Phases, want)
	}
	if res.ScriptBody == "" || res.ScriptBody[:5] != "const" {
		t.Errorf("body not preserved verbatim: %q", res.ScriptBody)
	}
}

func TestParseMetaOptionalFields(t *testing.T) {
	script := `export const meta = {
  name: "find-flaky-tests",
  description: "Find flaky tests",
  whenToUse: "when CI is red",
  title: "Flaky Finder",
  phases: [
    { title: "Scan", detail: "grep logs", model: "fast" },
  ],
};
phase('Scan')`
	res, err := Parse(script)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Meta.WhenToUse != "when CI is red" || res.Meta.Title != "Flaky Finder" {
		t.Errorf("optional fields: %+v", res.Meta)
	}
	if len(res.Meta.Phases) != 1 || res.Meta.Phases[0].Model != "fast" || res.Meta.Phases[0].Detail != "grep logs" {
		t.Errorf("phase model/detail: %+v", res.Meta.Phases)
	}
	// The `;` after the literal is consumed; the body starts at phase(...).
	if res.ScriptBody != "phase('Scan')" {
		t.Errorf("body = %q", res.ScriptBody)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not first statement": `const x = 1
export const meta = { name: 'a', description: 'b' }`,
		"missing name":        `export const meta = { description: 'b' }`,
		"empty name":          `export const meta = { name: '', description: 'b' }`,
		"missing description": `export const meta = { name: 'a' }`,
		"reserved key":        `export const meta = { name: 'a', description: 'b', __proto__: {} }`,
		"interpolation":       "export const meta = { name: `x${1}`, description: 'b' }",
		"non-literal value":   `export const meta = { name: SOME_CONST, description: 'b' }`,
		"not an object":       `export const meta = 'nope'`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(script); err == nil {
				t.Errorf("expected parse error for %q", name)
			}
		})
	}
}

func TestParseOversize(t *testing.T) {
	big := make([]byte, MaxScriptBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if _, err := Parse(string(big)); err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestParseNegativeNumberAndNested(t *testing.T) {
	// Negative numbers and nested arrays/objects are pure literals.
	script := `export const meta = {
  name: 'n',
  description: 'd',
  phases: [{ title: 'A' }],
  nums: [-1, 2.5, 3e2],
  nested: { a: { b: [true, false, null] } },
}
x()`
	if _, err := Parse(script); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestTrimBody(t *testing.T) {
	// Leading `;` line then whitespace is dropped.
	if got := trimBody(";\n  body"); got != "body" {
		t.Errorf("trimBody = %q", got)
	}
	if got := trimBody("\n\nx"); got != "x" {
		t.Errorf("trimBody = %q", got)
	}
}
