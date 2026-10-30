package workflows

import "testing"

func TestParseAcceptsLeadingComments(t *testing.T) {
	script := "// build pipeline\n/* multi\n * line */\n\nexport const meta = { name: \"w\", description: \"d\" }\nrun()\n"
	res, err := Parse(script)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Meta.Name != "w" || res.ScriptBody != "run()\n" {
		t.Fatalf("got %+v", res)
	}
	// Code before the meta export is still rejected.
	if _, err := Parse("run()\nexport const meta = { name: \"w\", description: \"d\" }\n"); err == nil {
		t.Fatal("non-comment statement before meta must be rejected")
	}
	// An unterminated block comment is not trivia.
	if _, err := Parse("/* open\nexport const meta = { name: \"w\", description: \"d\" }\n"); err == nil {
		t.Fatal("unterminated comment must be rejected")
	}
}
