package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/pluginmgr"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the interactive /plugin flow onto the same dialog host
// contract as /model and /login (openAndWait + the shared submit/cancel
// callbacks). It drives internal/pluginmgr.Manager.
//
// The top menu offers "Add marketplace", "Browse and install plugins", "Manage
// installed plugins", "List marketplaces". Every data-driven step uses the
// ListDialog picker, which is the right primitive for choosing a plugin or a
// marketplace.
//
// Text entry: the ui host only ships a MASKED text field (SecretDialog), which
// is wrong for a marketplace spec, and the ui package is not edited this round.
// So the marketplace spec is entered through the fast-path argument form
// (`/plugin add <owner/repo|url|path>` or `/plugin marketplace add <…>`), and
// the "Add marketplace" menu item points the user at it. Every other action is
// fully interactive.
//
// After install/enable the session cannot hot-swap the already-loaded skill /
// command / agent registries without re-running discovery and risking duplicate
// registrations, so ("restart required to apply") the flow tells the user to
// restart gocode to load the new plugin.

// plugin is the /plugin entrypoint. args is the raw argument string: empty opens
// the action menu, otherwise it is a fast-path (add / marketplace / install /
// list / enable / disable / uninstall / manage).
func (d *interactiveDialogs) plugin(args string) {
	if d.pluginMgr == nil {
		d.sendMsg(ui.AppendSystemMsg{Text: "/plugin is unavailable: no user config directory could be resolved."})
		return
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		d.pluginMenu()
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
	switch fields[0] {
	case "add":
		d.pluginAddMarketplace(rest)
	case "marketplace", "marketplaces":
		d.pluginMarketplaceFast(fields[1:])
	case "install":
		if len(fields) > 1 {
			d.pluginInstallRef(fields[1])
		} else {
			d.pluginBrowseInstall()
		}
	case "list", "ls":
		d.sendMsg(ui.AppendSystemMsg{Text: d.installedText("")})
	case "manage":
		d.pluginManage()
	case "enable":
		d.pluginSetEnabled(argAt(fields, 1), true)
	case "disable":
		d.pluginSetEnabled(argAt(fields, 1), false)
	case "uninstall", "remove":
		d.pluginUninstall(argAt(fields, 1))
	default:
		d.sendMsg(ui.AppendSystemMsg{Text: pluginSlashHelp})
		d.pluginMenu()
	}
}

const pluginSlashHelp = `/plugin commands:
  /plugin                         Open the plugin menu
  /plugin add <source>            Add a marketplace (owner/repo, URL, or local path)
  /plugin marketplace list        List configured marketplaces
  /plugin install                 Browse and install plugins
  /plugin install <plugin>[@mkt]  Install a specific plugin
  /plugin list                    List installed plugins
  /plugin enable|disable <plugin> Toggle a plugin
  /plugin uninstall <plugin>      Uninstall a plugin
  /plugin manage                  Manage installed plugins`

// pluginMenu opens the top-level action picker.
func (d *interactiveDialogs) pluginMenu() {
	items := []ui.ListItem{
		{Label: "Add marketplace", Value: "add", Detail: "owner/repo, URL, or local path"},
		{Label: "Browse and install plugins", Value: "install"},
		{Label: "Manage installed plugins", Value: "manage", Detail: "enable, disable, uninstall"},
		{Label: "List marketplaces", Value: "mkt-list"},
	}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog("Plugins", items, 0))
	if !ok || r.cancelled {
		return
	}
	switch firstChosen(r.values) {
	case "add":
		// No plain-text dialog in the ui host: direct the user to the fast path.
		d.sendMsg(ui.AppendSystemMsg{Text: "Add a marketplace with: /plugin add <owner/repo | git-url | local-path>"})
	case "install":
		d.pluginBrowseInstall()
	case "manage":
		d.pluginManage()
	case "mkt-list":
		d.sendMsg(ui.ShowInfoPanelMsg{Text: marketplaceListText(d.marketplaces())})
	}
}

// pluginMarketplaceFast handles `/plugin marketplace <sub> …`.
func (d *interactiveDialogs) pluginMarketplaceFast(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		d.pluginAddMarketplace(strings.Join(args[1:], " "))
	case "list", "":
		d.sendMsg(ui.ShowInfoPanelMsg{Text: marketplaceListText(d.marketplaces())})
	case "remove":
		name := argAt(args, 1)
		if name == "" {
			d.sendMsg(ui.AppendSystemMsg{Text: "Usage: /plugin marketplace remove <name>"})
			return
		}
		if err := d.pluginMgr.RemoveMarketplace(name, false); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
			return
		}
		d.sendMsg(ui.AppendSystemMsg{Text: "Removed marketplace " + name})
	case "update":
		name := argAt(args, 1)
		if name == "" {
			d.sendMsg(ui.AppendSystemMsg{Text: "Usage: /plugin marketplace update <name>"})
			return
		}
		if _, err := d.pluginMgr.UpdateMarketplace(context.Background(), name); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
			return
		}
		d.sendMsg(ui.AppendSystemMsg{Text: "Updated marketplace " + name})
	default:
		d.sendMsg(ui.AppendSystemMsg{Text: "Unknown marketplace command. Use add, list, remove, or update."})
	}
}

// pluginAddMarketplace adds a marketplace from a source spec.
func (d *interactiveDialogs) pluginAddMarketplace(spec string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		d.sendMsg(ui.AppendSystemMsg{Text: "Usage: /plugin add <owner/repo | git-url | local-path>"})
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: "Adding marketplace " + spec + "…"})
	mkt, err := d.pluginMgr.AddMarketplace(context.Background(), spec)
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not add marketplace: " + err.Error()})
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Added marketplace %q (%d plugin(s)). Use /plugin install to browse them.", mkt.Name, len(mkt.Plugins))})
}

// pluginBrowseInstall lists every plugin the configured marketplaces offer and
// installs the chosen one.
func (d *interactiveDialogs) pluginBrowseInstall() {
	var items []ui.ListItem
	for _, info := range d.marketplaces() {
		if info.Index == nil {
			continue
		}
		for _, e := range info.Index.Plugins {
			label := e.Name
			if e.DisplayName != "" {
				label = e.DisplayName
			}
			items = append(items, ui.ListItem{
				Label:  label + " @" + info.Name,
				Value:  e.Name + "@" + info.Name,
				Detail: e.Description,
			})
		}
	}
	if len(items) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No plugins available. Add a marketplace first with /plugin add <source>."})
		return
	}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog("Select a plugin to install", items, 12))
	if !ok || r.cancelled {
		return
	}
	ref := firstChosen(r.values)
	if ref == "" {
		return
	}
	d.pluginInstallRef(ref)
}

// pluginInstallRef plans an install, surfaces its executable contributions for
// confirmation, then installs it.
func (d *interactiveDialogs) pluginInstallRef(ref string) {
	plan, err := d.pluginMgr.PlanInstall(context.Background(), ref)
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Could not resolve " + ref + ": " + err.Error()})
		return
	}
	body := []string{"Install " + plan.ID + versionSuffixText(plan.Version) + "?"}
	if txt := planContributionsText(plan); txt != "" {
		body = append(body, strings.Split(txt, "\n")...)
	}
	if !d.confirm("Install plugin", body...) {
		d.sendMsg(ui.AppendSystemMsg{Text: "Install cancelled."})
		return
	}
	if err := d.pluginMgr.Install(context.Background(), plan); err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: "Install failed: " + err.Error()})
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: "Installed and enabled " + plan.ID + ". " + pluginRestartHint})
}

// pluginManage lists installed plugins and lets the user toggle or uninstall
// each one.
func (d *interactiveDialogs) pluginManage() {
	installed, err := d.pluginMgr.ListInstalled()
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
		return
	}
	if len(installed) == 0 {
		d.sendMsg(ui.AppendSystemMsg{Text: "No plugins installed. Use /plugin install to add one."})
		return
	}
	enabled, _ := d.pluginMgr.EnabledPlugins()
	items := make([]ui.ListItem, 0, len(installed))
	for _, p := range installed {
		state := "disabled"
		if enabled[p.ID] {
			state = "enabled"
		}
		items = append(items, ui.ListItem{Label: p.ID, Value: p.ID, Detail: state})
	}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog("Manage installed plugins", items, 12))
	if !ok || r.cancelled {
		return
	}
	id := firstChosen(r.values)
	if id == "" {
		return
	}
	d.pluginActions(id, enabled[id])
}

// pluginActions opens the per-plugin action picker (toggle enable/disable or
// uninstall).
func (d *interactiveDialogs) pluginActions(id string, on bool) {
	toggle := ui.ListItem{Label: "Enable", Value: "enable"}
	if on {
		toggle = ui.ListItem{Label: "Disable", Value: "disable"}
	}
	items := []ui.ListItem{toggle, {Label: "Uninstall", Value: "uninstall"}, {Label: "Back", Value: "back"}}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog(id, items, 0))
	if !ok || r.cancelled {
		return
	}
	switch firstChosen(r.values) {
	case "enable":
		d.pluginSetEnabled(id, true)
	case "disable":
		d.pluginSetEnabled(id, false)
	case "uninstall":
		d.pluginUninstall(id)
	}
}

// pluginSetEnabled enables or disables an installed plugin.
func (d *interactiveDialogs) pluginSetEnabled(token string, on bool) {
	id := d.resolveID(token)
	if id == "" {
		d.sendMsg(ui.AppendSystemMsg{Text: "Usage: /plugin enable|disable <plugin>"})
		return
	}
	var err error
	if on {
		err = d.pluginMgr.Enable(id)
	} else {
		err = d.pluginMgr.Disable(id)
	}
	if err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
		return
	}
	verb := "Disabled"
	hint := " " + pluginRestartHint
	if on {
		verb = "Enabled"
	} else {
		hint = " Restart gocode (or start a new session) for the change to take effect."
	}
	d.sendMsg(ui.AppendSystemMsg{Text: verb + " " + id + "." + hint})
}

// pluginUninstall removes an installed plugin.
func (d *interactiveDialogs) pluginUninstall(token string) {
	id := d.resolveID(token)
	if id == "" {
		d.sendMsg(ui.AppendSystemMsg{Text: "Usage: /plugin uninstall <plugin>"})
		return
	}
	if err := d.pluginMgr.Uninstall(id); err != nil {
		d.sendMsg(ui.AppendErrorMsg{Text: err.Error()})
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: "Uninstalled " + id + "."})
}

// confirm opens a Yes/No picker and reports whether the user chose Yes.
func (d *interactiveDialogs) confirm(title string, body ...string) bool {
	items := []ui.ListItem{{Label: "Yes", Value: "yes"}, {Label: "No", Value: "no"}}
	r, ok := d.openAndWait(context.Background(), ui.ListDialog(title, items, 0, body...))
	if !ok || r.cancelled {
		return false
	}
	return firstChosen(r.values) == "yes"
}

// marketplaces returns the configured marketplaces (empty on error).
func (d *interactiveDialogs) marketplaces() []pluginmgr.MarketplaceInfo {
	list, err := d.pluginMgr.ListMarketplaces()
	if err != nil {
		return nil
	}
	return list
}

// installedText renders the installed-plugin list for a system message.
func (d *interactiveDialogs) installedText(only string) string {
	installed, err := d.pluginMgr.ListInstalled()
	if err != nil {
		return err.Error()
	}
	enabled, _ := d.pluginMgr.EnabledPlugins()
	return installedListText(installed, enabled, only)
}

// resolveID maps a bare name to a unique installed id (same rule as the CLI
// subcommand); a name@marketplace token is used verbatim.
func (d *interactiveDialogs) resolveID(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return resolveInstalledID(d.pluginMgr, token)
}

func argAt(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}
