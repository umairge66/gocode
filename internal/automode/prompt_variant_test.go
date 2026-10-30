package automode

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ruleNames lists the "- Name:" / "- Name [" rule names inside a template.
func ruleNames(text string) []string {
	re := regexp.MustCompile(`(?m)^- ([A-Za-z][^:\[\n*]*?) *(?:\[|:)`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	sort.Strings(out)
	return out
}

func slotBody(text, slot string) string {
	open, closing := "<"+slot+">", "</"+slot+">"
	i := strings.Index(text, open)
	j := strings.Index(text, closing)
	if i < 0 || j < i {
		return ""
	}
	return text[i+len(open) : j]
}

// The compact variant keeps every rule category of every section, under the
// same names (the <category>/<reason> the parser and the UI show).
func TestCompactKeepsEveryRule(t *testing.T) {
	envKeys := regexp.MustCompile(`(?m)^- \*\*([^*]+)\*\*`)
	keys := func(s string) (out []string) {
		for _, m := range envKeys.FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		return out
	}
	fe := keys(slotBody(permissionsTemplate, "user_environment_to_replace"))
	ce := keys(slotBody(compactPermissionsTemplate, "user_environment_to_replace"))
	if len(fe) == 0 || strings.Join(fe, "|") != strings.Join(ce, "|") {
		t.Errorf("environment entries differ\nfull:    %q\ncompact: %q", fe, ce)
	}
	for _, slot := range ruleSlots[:3] {
		full := ruleNames(slotBody(permissionsTemplate, slot))
		compact := ruleNames(slotBody(compactPermissionsTemplate, slot))
		if len(full) == 0 {
			t.Fatalf("%s: no rules found in the full template", slot)
		}
		if strings.Join(full, "\n") != strings.Join(compact, "\n") {
			t.Errorf("%s: rule names differ\nfull:    %q\ncompact: %q", slot, full, compact)
		}
	}
	// Every soft rule keeps its must-name item.
	for _, line := range strings.Split(slotBody(compactPermissionsTemplate, "user_soft_deny_rules_to_replace"), "\n") {
		if strings.HasPrefix(line, "- ") && !strings.Contains(line, "**must name:**") {
			t.Errorf("soft rule without a must-name slot: %.80s", line)
		}
	}
}

// Same placeholders, same output contract, same intent-rule anchors.
func TestCompactStructure(t *testing.T) {
	for _, s := range []string{"<permissions_template>", "## Output Format",
		"<block>yes</block><category>Exact BLOCK Rule Name</category><reason>[Exact BLOCK Rule Name] one short sentence</reason>",
		"<block>no</block>", "Do NOT include a <category> or <reason> tag when the action is allowed.",
		"## User Intent Rule", "[named+specifics]", "Path A", "Path B", "## Classification Process", "## Threat Model",
		"PREEMPTIVE BLOCK ON CLEAR INTENT", "Questions are not consent"} {
		if !strings.Contains(compactSystemPromptTemplate, s) {
			t.Errorf("compact system prompt lacks %q", s)
		}
	}
	for _, s := range append(append([]string{}, ruleSlots...), "settings_deny_rules") {
		if strings.Count(compactPermissionsTemplate, "<"+s+">") != 1 {
			t.Errorf("compact template: placeholder <%s> must appear once", s)
		}
	}
	for _, s := range []string{"## Environment", "## Definitions", "## HARD BLOCK", "## SOFT BLOCK", "## ALLOW"} {
		if !strings.Contains(compactPermissionsTemplate, s) {
			t.Errorf("compact template lacks section %q", s)
		}
	}
}

// Size budget: the compact prompt is about a quarter of the full one
// (≈ chars/4 tokens: full ≈ 35k, compact ≤ 9k).
func TestCompactSize(t *testing.T) {
	full := len(SystemPrompt(&Rules{Prompt: PromptFull}, nil))
	compact := len(SystemPrompt(&Rules{Prompt: PromptCompact}, nil))
	if compact/4 > 9000 {
		t.Errorf("compact prompt ≈ %d tokens, want ≤ 9000", compact/4)
	}
	if compact*3 > full {
		t.Errorf("compact (%d chars) is not much smaller than full (%d chars)", compact, full)
	}
}

func TestPromptSelection(t *testing.T) {
	if DefaultPrompt != PromptCompact {
		t.Fatalf("DefaultPrompt = %q", DefaultPrompt)
	}
	def := SystemPrompt(nil, nil)
	if def != SystemPrompt(&Rules{}, nil) || def != SystemPrompt(&Rules{Prompt: "compact"}, nil) || def != SystemPrompt(&Rules{Prompt: "nonsense"}, nil) {
		t.Fatal("empty, compact and unknown variants must all be the default (compact) prompt")
	}
	full := SystemPrompt(&Rules{Prompt: " FULL "}, nil)
	if full == def || !strings.Contains(full, "Three kinds of slot.") {
		t.Fatal("full variant is not the shipped template")
	}
	c := SystemPrompt(&Rules{Prompt: " COMPACT "}, []string{"Bash(rm:*)"})
	if !strings.Contains(c, "Trust slots default to") {
		t.Fatal("compact variant not selected")
	}
	for _, s := range append(append([]string{}, ruleSlots...), "settings_deny_rules", "permissions_template") {
		if strings.Contains(c, "<"+s+">") {
			t.Errorf("unsubstituted placeholder <%s> in the compact prompt", s)
		}
	}
	if !strings.Contains(c, "<cc_automode_permissions>\n## Environment") || !strings.Contains(c, "`Bash(rm:*)`") {
		t.Fatal("compact prompt misses the wrapped template or the deny-rule line")
	}
	for _, in := range []string{"full", "Full", " compact"} {
		if _, ok := NormalizePrompt(in); !ok {
			t.Errorf("NormalizePrompt(%q) not ok", in)
		}
	}
	if _, ok := NormalizePrompt(""); ok {
		t.Error(`NormalizePrompt("") ok`)
	}
}

// User rules and "$defaults" splice into the compact template exactly as into
// the full one — with the compact defaults.
func TestCompactUserRulesSplice(t *testing.T) {
	r := &Rules{Prompt: PromptCompact, SoftDeny: []string{"$defaults", "Never touch the billing DB"}, Allow: []string{"Pushing to the docs repo"}}
	p := SystemPrompt(r, nil)
	if !strings.Contains(p, "- Never touch the billing DB") || !strings.Contains(p, "- Pushing to the docs repo") {
		t.Fatal("custom rules missing")
	}
	if !strings.Contains(p, "- Git Destructive [named+specifics — **must name:**") {
		t.Fatal("$defaults did not splice the compact soft-deny defaults")
	}
	if strings.Contains(p, "- Declared Dependencies:") {
		t.Fatal("custom allow list without $defaults must replace the defaults")
	}
	if strings.Contains(p, "Reshaping the agent's own work — `--amend`") {
		t.Fatal("compact prompt spliced the full defaults")
	}
}
