package skills

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseSkillMDRichYAML covers YAML the shared parser now understands:
// folded descriptions, nested metadata maps, block lists, CRLF and booleans.
func TestParseSkillMDRichYAML(t *testing.T) {
	src := "---\r\nname: rich\r\ndescription: >-\r\n  Multi line\r\n  summary\r\nallowed-tools:\r\n  - Read\r\n  - Grep\r\nmetadata:\r\n  owner: team\r\n  tier: 2\r\nuser-invocable: off\r\n---\r\nBody\r\n"
	pr := ParseSkillMD([]byte(src), "SKILL.md")
	if pr.ParseError != "" {
		t.Fatal(pr.ParseError)
	}
	fm := pr.Frontmatter
	if fm.Description != "Multi line summary" || !reflect.DeepEqual(fm.AllowedTools, []string{"Read", "Grep"}) || fm.UserInvocable {
		t.Fatalf("fm = %+v", fm)
	}
	if fm.Metadata["owner"] != "team" || fm.Metadata["tier"] != int64(2) {
		t.Fatalf("metadata = %#v", fm.Metadata)
	}
	bad := ParseSkillMD([]byte("---\nname: x\nlist: \"unterminated\n---\nBody"), "SKILL.md")
	if !strings.Contains(bad.ParseError, "line 3") || bad.Content != "Body" {
		t.Fatalf("bad = %+v", bad)
	}
}
