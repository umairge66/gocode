package cli

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// `gocode auto-mode <defaults|config|reset|critique>`: inspect or reset the
// auto mode rule configuration (the settings "autoMode" section:
// allow / soft_deny / hard_deny / environment).
//
// These rules feed the auto mode safety classifier (internal/automode, wired in
// automodeclassifier.go). defaults / config / reset are purely local (they
// read the shipped rule text and the settings files, and reset edits the user
// settings file). critique would send the rules to a model for review; it is
// not available (see runAutoModeCritique).

// autoModeDefaultsText holds the four shipped default rule sections: the
// <user_*_to_replace> blocks of the permissions template that autoModeDefaults
// reads.
//
//go:embed automode_defaults.txt
var autoModeDefaultsText string

// autoModeRules is the rule set, in its display key order.
type autoModeRules struct {
	Allow       []string `json:"allow"`
	SoftDeny    []string `json:"soft_deny"`
	HardDeny    []string `json:"hard_deny"`
	Environment []string `json:"environment"`
}

// autoModeDefaultsSentinel splices the defaults into a custom list.
const autoModeDefaultsSentinel = "$defaults"

// autoModeRuleSection returns the "- "-bulleted entries inside one
// <tag>…</tag> block; continuation lines are appended to the previous entry.
func autoModeRuleSection(text, tag string) []string {
	re := regexp.MustCompile(`<` + tag + `>([\s\S]*?)</` + tag + `>`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return []string{}
	}
	out := []string{}
	for _, line := range strings.Split(m[1], "\n") {
		h := strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t")
		if strings.HasPrefix(h, "- ") {
			out = append(out, h[2:])
		} else if len(out) > 0 && strings.TrimSpace(h) != "" {
			out[len(out)-1] += "\n" + h
		}
	}
	return out
}

// autoModeDefaults returns the shipped default rules.
func autoModeDefaults() autoModeRules {
	return autoModeRules{
		Allow:       autoModeRuleSection(autoModeDefaultsText, "user_allow_rules_to_replace"),
		SoftDeny:    autoModeRuleSection(autoModeDefaultsText, "user_soft_deny_rules_to_replace"),
		HardDeny:    autoModeRuleSection(autoModeDefaultsText, "user_hard_deny_rules_to_replace"),
		Environment: autoModeRuleSection(autoModeDefaultsText, "user_environment_to_replace"),
	}
}

// autoModeLabel normalizes a rule to its label: strip "*" and
// "[...]", lower-case, then keep the text before the first ":" or "[".
var autoModeBracketRe = regexp.MustCompile(`\[[^\]]*\]`)

func autoModeLabel(rule string) string {
	s := strings.ToLower(autoModeBracketRe.ReplaceAllString(strings.ReplaceAll(rule, "*", ""), ""))
	if i := strings.IndexAny(s, ":["); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t\r\n")
}

// filterAutoModeRules keeps rules whose label starts
// with the (normalized) prefix.
func filterAutoModeRules(r autoModeRules, label string) autoModeRules {
	want := autoModeLabel(label)
	if want == "" {
		want = strings.ToLower(label)
	}
	keep := func(list []string) []string {
		out := []string{}
		for _, x := range list {
			if strings.HasPrefix(autoModeLabel(x), want) {
				out = append(out, x)
			}
		}
		return out
	}
	return autoModeRules{Allow: keep(r.Allow), SoftDeny: keep(r.SoftDeny), HardDeny: keep(r.HardDeny), Environment: keep(r.Environment)}
}

// spliceDefaults merges a custom list with the defaults: an empty custom list
// means the defaults; the "$defaults" sentinel expands (once) to the defaults in place.
func spliceDefaults(custom, defaults []string) []string {
	if len(custom) == 0 {
		return append([]string{}, defaults...)
	}
	out := []string{}
	spliced := false
	for _, r := range custom {
		if r == autoModeDefaultsSentinel {
			if !spliced {
				out = append(out, defaults...)
				spliced = true
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// autoModeTrustedSources are the only tiers allowed to set classifier rules;
// project and local settings are repo-controllable and ignored.
var autoModeTrustedSources = []settingspolicy.Source{settingspolicy.SourceUser, settingspolicy.SourceFlag, settingspolicy.SourcePolicy}

// autoModeSection parses a settings "autoMode" value: an object whose four keys
// are optional string arrays. ok=false when the value does not have that shape
// .
func autoModeSection(v any) (r autoModeRules, ok bool) {
	m, isObj := v.(map[string]any)
	if !isObj {
		return r, false
	}
	get := func(key string) ([]string, bool) {
		raw, present := m[key]
		if !present || raw == nil {
			return nil, true
		}
		arr, isArr := raw.([]any)
		if !isArr {
			return nil, false
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			s, isStr := x.(string)
			if !isStr {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	var good bool
	if r.Allow, good = get("allow"); !good {
		return r, false
	}
	if r.SoftDeny, good = get("soft_deny"); !good {
		return r, false
	}
	if r.HardDeny, good = get("hard_deny"); !good {
		return r, false
	}
	if r.Environment, good = get("environment"); !good {
		return r, false
	}
	return r, true
}

// userAutoModeRules concatenates the autoMode lists from the
// trusted sources (user, flag, policy). The result is nil when none sets any
// rule.
func userAutoModeRules(env *Env) *autoModeRules {
	ctx := settingsContext(env)
	fsys := fsfacade.OSFS{}
	var out autoModeRules
	for _, src := range autoModeTrustedSources {
		res := settingsload.LoadSource(fsys, ctx, src)
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		sec, ok := autoModeSection(res.Settings["autoMode"])
		if !ok {
			continue
		}
		out.Allow = append(out.Allow, sec.Allow...)
		out.SoftDeny = append(out.SoftDeny, sec.SoftDeny...)
		out.HardDeny = append(out.HardDeny, sec.HardDeny...)
		out.Environment = append(out.Environment, sec.Environment...)
	}
	if len(out.Allow)+len(out.SoftDeny)+len(out.HardDeny)+len(out.Environment) == 0 {
		return nil
	}
	return &out
}

// effectiveAutoModeRules returns the custom rules spliced into the defaults.
func effectiveAutoModeRules(custom *autoModeRules) autoModeRules {
	d := autoModeDefaults()
	var c autoModeRules
	if custom != nil {
		c = *custom
	}
	return autoModeRules{
		Allow:       spliceDefaults(c.Allow, d.Allow),
		SoftDeny:    spliceDefaults(c.SoftDeny, d.SoftDeny),
		HardDeny:    spliceDefaults(c.HardDeny, d.HardDeny),
		Environment: spliceDefaults(c.Environment, d.Environment),
	}
}

// printAutoModeJSON prints the rules as 2-space-indented JSON.
func printAutoModeJSON(env *Env, r autoModeRules) {
	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintf(env.Stdout, "%s\n", data)
}

// runAutoModeCmd routes `gocode auto-mode <verb>`.
func runAutoModeCmd(env *Env, t []string) int {
	args := argsAfter(t, "auto-mode")
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	switch verb {
	case "defaults":
		o, err := scanOpts(args, nil, map[string]string{"--label": "label"})
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 1
		}
		rules := autoModeDefaults()
		if label, ok := o.value("label"); ok {
			rules = filterAutoModeRules(rules, label)
		}
		printAutoModeJSON(env, rules)
		return 0
	case "config":
		printAutoModeJSON(env, effectiveAutoModeRules(userAutoModeRules(env)))
		return 0
	case "reset":
		o, err := scanOpts(args, map[string]string{"-y": "yes", "--yes": "yes"}, nil)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 1
		}
		return runAutoModeReset(env, o.has("yes"))
	case "critique":
		return runAutoModeCritique(env)
	default:
		writeCommandUsage(env, "auto-mode")
		return 1
	}
}

// runAutoModeReset removes the autoMode section from the user
// settings file after showing what goes and asking for confirmation.
//
// The settings writer (settingsload.UpdateFile) rewrites the raw JSON object
// with every other key preserved, so nothing else is lost.
func runAutoModeReset(env *Env, yes bool) int {
	path, ok := settingsContext(env).PathForSource(settingspolicy.SourceUser)
	if !ok {
		fmt.Fprintln(env.Stderr, "Could not resolve the user settings file path.")
		return 1
	}
	var content string
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		content = string(data)
	case errors.Is(err, fs.ErrNotExist):
	default:
		fmt.Fprintf(env.Stderr, "Could not read %s: %v\n", path, err)
		return 1
	}
	var obj map[string]any
	if strings.TrimSpace(content) != "" {
		parsed := settingsload.ParseContent(path, content)
		if parsed.LoadState != settingspolicy.LoadLoaded {
			fmt.Fprintf(env.Stderr, "The settings file at %s contains invalid JSON — fix or remove it, then re-run reset.\n", path)
			return 1
		}
		obj = parsed.Settings
	}
	section, present := obj["autoMode"]
	if !present {
		fmt.Fprintf(env.Stdout, "Auto mode configuration is already at defaults — %s has no autoMode section.\n", path)
		autoModeOtherSourcesNote(env)
		return 0
	}
	fmt.Fprintf(env.Stdout, "This resets auto mode to the shipped defaults by removing the autoMode section from %s:\n", path)
	for _, line := range autoModeSectionSummary(section) {
		fmt.Fprintf(env.Stdout, "  - %s\n", line)
	}
	if !yes && !confirmYesNo(env, "Reset auto mode configuration to defaults?") {
		fmt.Fprintln(env.Stderr, "Aborted.")
		return 1
	}
	werr := settingsload.UpdateFile(path, func(m map[string]any) (bool, error) {
		if _, has := m["autoMode"]; !has {
			return false, nil
		}
		delete(m, "autoMode")
		return true, nil
	})
	if werr != nil {
		if strings.Contains(werr.Error(), "JSON") {
			fmt.Fprintf(env.Stderr, "The settings file at %s contains invalid JSON — fix or remove it, then re-run reset.\n", path)
		} else {
			fmt.Fprintf(env.Stderr, "Could not write %s — check file permissions and disk space (run with --debug for the underlying error).\n", path)
		}
		return 1
	}
	fmt.Fprintf(env.Stdout, "Auto mode configuration reset to defaults — autoMode section removed from %s.\nRun `gocode auto-mode config` to see the effective rules.\n", path)
	autoModeOtherSourcesNote(env)
	return 0
}

// autoModeSectionSummary returns one line per autoMode key, with the
// entry count for arrays.
func autoModeSectionSummary(v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return []string{"autoMode (unrecognized value)"}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if arr, isArr := m[k].([]any); isArr {
			out = append(out, fmt.Sprintf("%s (%d %s)", k, len(arr), pluralize(len(arr), "entry", "entries")))
		} else {
			out = append(out, k)
		}
	}
	return out
}

// autoModeOtherSourcesNote prints a note when a non-user trusted
// source (flag / managed) still sets autoMode.
func autoModeOtherSourcesNote(env *Env) {
	ctx := settingsContext(env)
	for _, src := range autoModeTrustedSources {
		if src == settingspolicy.SourceUser {
			continue
		}
		res := settingsload.LoadSource(fsfacade.OSFS{}, ctx, src)
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if _, ok := res.Settings["autoMode"]; ok {
			fmt.Fprintln(env.Stdout, "Note: auto mode rules from managed or --settings flag sources still apply — reset only changes your user settings file.")
			return
		}
	}
}

// runAutoModeCritique runs the local half of `auto-mode critique`. With no
// custom rules it prints guidance on adding some. The review itself would send
// the rules and the classifier prompt to a model; that half is not available.
func runAutoModeCritique(env *Env) int {
	custom := userAutoModeRules(env)
	if custom == nil || !hasCustomAutoModeRule(*custom) {
		fmt.Fprintln(env.Stdout, "No custom auto mode rules found.\n\nAdd rules to your settings file under autoMode.{allow, soft_deny, hard_deny, environment}.\nRun `gocode auto-mode defaults` to see the default rules for reference.")
		return 0
	}
	fmt.Fprintln(env.Stderr, "gocode auto-mode critique is not available in this build: the model review of your rules is not included. Your rules are still applied by the auto mode classifier; run `gocode auto-mode config` to see them.")
	return 1
}

// hasCustomAutoModeRule reports any entry other than the sentinel.
func hasCustomAutoModeRule(r autoModeRules) bool {
	for _, list := range [][]string{r.Allow, r.SoftDeny, r.HardDeny, r.Environment} {
		for _, x := range list {
			if x != autoModeDefaultsSentinel {
				return true
			}
		}
	}
	return false
}
