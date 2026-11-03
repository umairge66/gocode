// Neutralization of harness-control markup in model- or tool-authored text
// that is handed back to a model (a subagent's report, for example).

package textutil

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// HarnessControlTags are the tags gocode itself injects to speak to the model
// as the harness: reminders, notifications, peer envelopes, command and shell
// transcripts, hook context, diagnostics and the result wrappers. Text that
// did not come from the harness has no business opening or closing them.
var HarnessControlTags = []string{
	"system-reminder",
	"task-notification",
	"cross-session-message",
	"agent-message",
	"command-name",
	"command-message",
	"command-args",
	"local-command-caveat",
	"local-command-stdout",
	"local-command-stderr",
	"bash-input",
	"bash-stdout",
	"bash-stderr",
	"user-prompt-submit-hook",
	"new-diagnostics",
	"persisted-output",
	"untrusted-web-content",
}

// harnessTagRE matches the opener of a harness tag — "<tag", "</tag", with
// optional spaces around the slash, any letter case, ASCII or fullwidth "<" —
// unless it is already escaped ("<\").
var harnessTagRE = func() *regexp.Regexp {
	names := make([]string, len(HarnessControlTags))
	for i, t := range HarnessControlTags {
		names[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)[<\x{FF1C}](\s*/?\s*)(` + strings.Join(names, "|") + `)\b`)
}()

// turnMarkerRE matches a fake conversation turn marker at the start of a line.
var turnMarkerRE = regexp.MustCompile(`(?m)^([ \t]*)(Human|Assistant):`)

// NeutralizeHarnessText defuses harness-only markup in text that a model will
// read as data: every harness control tag (HarnessControlTags) has its "<"
// escaped to `<\` (so "<system-reminder>" reads "<\system-reminder>" and
// cannot open or close a real one), and a "Human:" / "Assistant:" turn marker
// at the start of a line gets a leading backslash. It returns the text and
// the sorted, de-duplicated list of what matched ("system-reminder tag",
// "Human: turn marker" …); nil when nothing did, and then text is unchanged.
func NeutralizeHarnessText(text string) (string, []string) {
	seen := map[string]bool{}
	out := harnessTagRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := harnessTagRE.FindStringSubmatch(m)
		seen[strings.ToLower(sub[2])+" tag"] = true
		_, size := utf8.DecodeRuneInString(m)
		return `<\` + m[size:]
	})
	out = turnMarkerRE.ReplaceAllStringFunc(out, func(m string) string {
		sub := turnMarkerRE.FindStringSubmatch(m)
		seen[sub[2]+": turn marker"] = true
		return sub[1] + `\` + sub[2] + ":"
	})
	if len(seen) == 0 {
		return text, nil
	}
	hits := make([]string, 0, len(seen))
	for k := range seen {
		hits = append(hits, k)
	}
	sort.Strings(hits)
	return out, hits
}
