package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/ui"
)

// /permissions (/allowed-tools): view the allow, ask and deny permission
// rules by the source they come from, add a rule to the user, project or
// local project settings, delete a rule from one of those files, and view the
// workspace directories. A change is written to the settings file and applied
// to the live session at once. Rules from managed settings, the --settings
// flag, the command line or this session only are shown read-only.

// permRule is one permission rule and where it comes from.
type permRule struct {
	behavior toolreg.RuleBehavior
	text     string // the rule as written ("Bash(npm test:*)")
	source   string // a settings source, "cliArg" or "session"
}

// Values of the /permissions menus.
const (
	permAddValue       = "__add"
	permWorkspaceValue = "workspace"
)

// permBehaviors are the rule lists in menu order.
var permBehaviors = []toolreg.RuleBehavior{toolreg.RuleAllow, toolreg.RuleAsk, toolreg.RuleDeny}

// permBehaviorTitle is a behavior's menu label.
func permBehaviorTitle(b toolreg.RuleBehavior) string {
	switch b {
	case toolreg.RuleAsk:
		return "Ask"
	case toolreg.RuleDeny:
		return "Deny"
	}
	return "Allow"
}

// permBehaviorHelp explains what a behavior's rules do.
func permBehaviorHelp(b toolreg.RuleBehavior) string {
	switch b {
	case toolreg.RuleAsk:
		return "gocode will always ask for confirmation before using these tools."
	case toolreg.RuleDeny:
		return "gocode will always reject requests to use denied tools."
	}
	return "gocode won't ask before using allowed tools."
}

// permSourceLabel names a rule source for display.
func permSourceLabel(source string) string {
	switch settingspolicy.Source(source) {
	case settingspolicy.SourceUser:
		return "user settings"
	case settingspolicy.SourceProject:
		return "project settings"
	case settingspolicy.SourceLocal:
		return "project local settings"
	case settingspolicy.SourceFlag:
		return "the --settings flag"
	case settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
		return "managed settings"
	}
	switch source {
	case "cliArg":
		return "the command line (--allowedTools / --disallowedTools)"
	case "session":
		return "this session"
	}
	return source
}

// permEditableSource reports whether /permissions may change rules of source.
func permEditableSource(source string) bool {
	switch settingspolicy.Source(source) {
	case settingspolicy.SourceUser, settingspolicy.SourceProject, settingspolicy.SourceLocal:
		return true
	}
	return false
}

// ruleText renders a rule the way it is written in settings.
func ruleText(r toolreg.Rule) string {
	if r.Content == "" {
		return r.ToolName
	}
	return r.ToolName + "(" + r.Content + ")"
}

// permissionRuleList collects the rules of every settings source in
// precedence order (low to high), followed by the live session's rules that
// come from no settings file (command line, this session only).
func (d *interactiveDialogs) permissionRuleList() []permRule {
	var out []permRule
	ctx := settingsContext(d.env)
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		perms, ok := or.Result.Settings["permissions"].(map[string]any)
		if !ok {
			continue
		}
		for _, b := range permBehaviors {
			list, _ := perms[string(b)].([]any)
			for _, v := range list {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, permRule{behavior: b, text: strings.TrimSpace(s), source: string(or.Source)})
				}
			}
		}
	}
	if d.rules != nil {
		for _, r := range d.rules.Snapshot() {
			if r.Source == "cliArg" || r.Source == "session" {
				out = append(out, permRule{behavior: r.Behavior, text: ruleText(r), source: r.Source})
			}
		}
	}
	return out
}

// permissions runs the /permissions dialog.
func (d *interactiveDialogs) permissions() {
	changed := false
	for {
		rules := d.permissionRuleList()
		count := map[toolreg.RuleBehavior]int{}
		for _, r := range rules {
			count[r.behavior]++
		}
		var items []ui.ListItem
		for _, b := range permBehaviors {
			items = append(items, ui.ListItem{
				Label:  permBehaviorTitle(b),
				Value:  string(b),
				Detail: countNoun(count[b], "rule", "rules") + " · " + permBehaviorHelp(b),
			})
		}
		dirs := d.workspaceDirs()
		items = append(items, ui.ListItem{Label: "Workspace", Value: permWorkspaceValue, Detail: countNoun(len(dirs), "directory", "directories")})
		var body []string
		if managedPermissionRulesOnly(d.env) {
			body = append(body, "Managed settings allow only managed permission rules: other rules are ignored and cannot be added or removed.")
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog("Permissions", items, 0, body...))
		if !ok || r.cancelled {
			if !changed {
				d.sendMsg(ui.AppendSystemMsg{Text: "Permissions dialog dismissed"})
			}
			return
		}
		switch v := firstChosen(r.values); v {
		case permWorkspaceValue:
			d.workspaceView(dirs)
		case "":
			return
		default:
			if d.permissionBehavior(toolreg.RuleBehavior(v)) {
				changed = true
			}
		}
	}
}

// permissionBehavior shows one behavior's rules with "Add a new rule…". It
// returns to the menu on Esc and reports whether anything changed.
func (d *interactiveDialogs) permissionBehavior(b toolreg.RuleBehavior) bool {
	changed := false
	for {
		var rules []permRule
		for _, r := range d.permissionRuleList() {
			if r.behavior == b {
				rules = append(rules, r)
			}
		}
		items := []ui.ListItem{{Label: "Add a new rule…", Value: permAddValue}}
		for i, r := range rules {
			items = append(items, ui.ListItem{Label: r.text, Value: strconv.Itoa(i), Detail: "from " + permSourceLabel(r.source)})
		}
		body := []string{permBehaviorHelp(b)}
		if len(rules) == 0 {
			body = append(body, "No "+strings.ToLower(permBehaviorTitle(b))+" rules")
		}
		r, ok := d.openAndWait(context.Background(), ui.ListDialog(permBehaviorTitle(b)+" rules", items, 12, body...))
		if !ok || r.cancelled {
			return changed
		}
		v := firstChosen(r.values)
		if v == permAddValue {
			if d.addPermissionRule(b) {
				changed = true
			}
			continue
		}
		i, err := strconv.Atoi(v)
		if err != nil || i < 0 || i >= len(rules) {
			return changed
		}
		if d.permissionRuleDetail(rules[i]) {
			changed = true
		}
	}
}

// permissionRuleDetail shows one rule: a deletable rule asks to confirm the
// delete; a read-only one says where it is configured.
func (d *interactiveDialogs) permissionRuleDetail(r permRule) bool {
	if !permEditableSource(r.source) {
		why := "This rule comes from a read-only source (" + permSourceLabel(r.source) + ") and cannot be modified here."
		switch settingspolicy.Source(r.source) {
		case settingspolicy.SourcePolicy, settingspolicy.SourceManaged:
			why = "This rule is configured by managed settings and cannot be modified."
		}
		dlg := &ui.Dialog{Kind: ui.DialogAsk, Title: "Rule details",
			Body:    []string{r.text, "From " + permSourceLabel(r.source), why},
			Options: []ui.DialogOption{{Label: "Back", Value: "back"}}}
		d.openAndWait(context.Background(), dlg)
		return false
	}
	dlg := &ui.Dialog{Kind: ui.DialogAsk,
		Title: "Delete " + string(r.behavior) + " rule?",
		Body:  []string{r.text, "From " + permSourceLabel(r.source), permBehaviorHelp(r.behavior), "Are you sure you want to delete this permission rule?"},
		Options: []ui.DialogOption{
			{Label: "Yes, delete it", Value: "delete"},
			{Label: "No", Value: "cancel"},
		}}
	reply, ok := d.openAndWait(context.Background(), dlg)
	if !ok || reply.cancelled || firstChosen(reply.values) != "delete" {
		return false
	}
	if managedPermissionRulesOnly(d.env) {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not delete " + r.text + ": " + errManagedRulesOnly.Error()})
		return false
	}
	removed, err := settingsload.RemovePermissionRule(settingsContext(d.env), settingspolicy.Source(r.source), string(r.behavior), r.text)
	if err != nil || !removed {
		msg := "the rule is unchanged"
		if err != nil {
			msg = err.Error()
		}
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not delete " + r.text + " — " + msg})
		return false
	}
	if d.rules != nil {
		if parsed, ok := parsePermissionRule(r.text, r.behavior, r.source); ok {
			d.rules.Remove(func(x toolreg.Rule) bool {
				return x.Behavior == parsed.Behavior && x.ToolName == parsed.ToolName && x.Content == parsed.Content && x.Source == parsed.Source
			})
		}
	}
	d.sendMsg(ui.AppendSystemMsg{Text: "Deleted " + string(r.behavior) + " rule " + r.text})
	return true
}

// addPermissionRule asks for a rule and where to save it, then saves it and
// applies it to the live session.
func (d *interactiveDialogs) addPermissionRule(b toolreg.RuleBehavior) bool {
	in, ok := d.openAndWait(context.Background(), ui.TextInputDialog(
		"Add "+string(b)+" permission rule", "",
		"Permission rules are a tool name, optionally followed by a specifier in parentheses.",
		"e.g., WebFetch(domain:example.com) or Bash(npm run test:*)"))
	if !ok || in.cancelled {
		return false
	}
	text := strings.TrimSpace(firstChosen(in.values))
	if text == "" {
		return false
	}
	var warning string
	if p := validatePermissionRule(text, string(b)); p != nil {
		if !p.Warning {
			d.sendMsg(ui.AppendErrorMsg{Text: "Invalid permission rule " + text + ": " + p.message()})
			return false
		}
		warning = p.message()
	}
	if managedPermissionRulesOnly(d.env) {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not add " + text + ": " + errManagedRulesOnly.Error()})
		return false
	}
	ctx := settingsContext(d.env)
	var items []ui.ListItem
	for _, s := range []struct {
		src   settingspolicy.Source
		label string
		verb  string
	}{
		{settingspolicy.SourceLocal, "Project settings (local)", "Saved in "},
		{settingspolicy.SourceProject, "Project settings", "Checked in at "},
		{settingspolicy.SourceUser, "User settings", "Saved in "},
	} {
		if p, ok := ctx.PathForSource(s.src); ok {
			items = append(items, ui.ListItem{Label: s.label, Value: string(s.src), Detail: s.verb + displayPath(d.env, p)})
		}
	}
	where, ok := d.openAndWait(context.Background(), ui.ListDialog("Where should this rule be saved?", items, 0, text))
	if !ok || where.cancelled {
		return false
	}
	src := settingspolicy.Source(firstChosen(where.values))
	if !permEditableSource(string(src)) {
		return false
	}
	added, err := settingsload.AddPermissionRule(ctx, src, string(b), text)
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not add " + text + ": " + err.Error()})
		return false
	}
	if !added {
		d.sendMsg(ui.AppendSystemMsg{Text: text + " is already a " + string(b) + " rule in " + permSourceLabel(string(src))})
		return false
	}
	if d.rules != nil {
		if r, ok := parsePermissionRule(text, b, string(src)); ok {
			d.rules.Add(r)
		}
	}
	msg := "Added " + string(b) + " rule " + text
	if warning != "" {
		msg += "\nWarning: " + warning
	}
	d.sendMsg(ui.AppendSystemMsg{Text: msg})
	return true
}

// workspaceDir is one directory gocode works in.
type workspaceDir struct{ path, detail string }

// workspaceDirs lists the working directory and the additional directories
// (--add-dir, permissions.additionalDirectories).
func (d *interactiveDialogs) workspaceDirs() []workspaceDir {
	ctx := settingsContext(d.env)
	var out []workspaceDir
	if ctx.Cwd != "" {
		out = append(out, workspaceDir{ctx.Cwd, "Working directory"})
	}
	flagDirs, _ := resolveAddDirs(ctx.Cwd, ctx.HomeDir, d.addDirs)
	isFlag := map[string]bool{}
	for _, p := range flagDirs {
		isFlag[p] = true
	}
	for _, p := range additionalWorkingDirs(d.env, flagDirs) {
		detail := "From settings (permissions.additionalDirectories)"
		if isFlag[p] {
			detail = "From --add-dir"
		}
		out = append(out, workspaceDir{p, detail})
	}
	return out
}

// workspaceView lists the workspace directories (read-only).
func (d *interactiveDialogs) workspaceView(dirs []workspaceDir) {
	items := make([]ui.ListItem, len(dirs))
	for i, w := range dirs {
		items[i] = ui.ListItem{Label: displayPath(d.env, w.path), Value: w.path, Detail: w.detail}
	}
	d.openAndWait(context.Background(), ui.ListDialog("Workspace", items, 12,
		"gocode can read files in the workspace, and make edits when auto-accept edits is on.",
		"Add directories with --add-dir or permissions.additionalDirectories in settings."))
}
