package agentsdef

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDefinitionRichYAML(t *testing.T) {
	src := "---\nname: reviewer\ndescription: |\n  Reviews code.\n  Use after edits.\ntools: [Read, Grep]\nbackground: yes\neffort: 3\n---\nYou review.\n"
	pr := ParseDefinition([]byte(src), "reviewer.md")
	if pr.ParseError != "" {
		t.Fatal(pr.ParseError)
	}
	fm := pr.Frontmatter
	if fm.Description != "Reviews code.\nUse after edits.\n" || !reflect.DeepEqual(fm.Tools, []string{"Read", "Grep"}) || !fm.Background || fm.Effort != "3" {
		t.Fatalf("fm = %+v", fm)
	}
	bad := ParseDefinition([]byte("---\nname: x\n\tdescription: y\n  - z\n---\nB"), "x.md")
	if bad.ParseError == "" || !strings.Contains(bad.ParseError, "line") || bad.Body != "B" {
		t.Fatalf("bad = %+v", bad)
	}
}
