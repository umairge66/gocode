package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/ui"
)

// newPluginDialogs builds an interactiveDialogs wired to mgr with a channel-
// backed send, enough for the /plugin flow (it touches only send + pluginMgr).
func newPluginDialogs(t *testing.T) (*interactiveDialogs, chan tea.Msg) {
	t.Helper()
	ch := make(chan tea.Msg, 64)
	d := &interactiveDialogs{
		send:      func(m tea.Msg) { ch <- m },
		pluginMgr: managerFor(t),
	}
	return d, ch
}

// recvUntil drains ch until pred matches or it times out.
func recvUntil(t *testing.T, ch <-chan tea.Msg, pred func(tea.Msg) bool) tea.Msg {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-ch:
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for message")
			return nil
		}
	}
}

func sysTextContains(sub string) func(tea.Msg) bool {
	return func(m tea.Msg) bool {
		switch s := m.(type) {
		case ui.AppendSystemMsg:
			return strings.Contains(s.Text, sub)
		case ui.ShowInfoPanelMsg: // listings open in the info panel
			return strings.Contains(s.Text, sub)
		}
		return false
	}
}

// TestPluginDialog_FastPaths covers the argument fast-paths that need no dialog:
// `/plugin add <local-path>` and `/plugin list`.
func TestPluginDialog_FastPaths(t *testing.T) {
	mkt := buildCLILocalMarketplace(t, "mymkt")
	d, ch := newPluginDialogs(t)

	// /plugin list with nothing installed.
	d.plugin("list")
	recvUntil(t, ch, sysTextContains("No plugins installed"))

	// /plugin add <path>
	d.plugin("add " + mkt)
	recvUntil(t, ch, sysTextContains(`Added marketplace "mymkt"`))

	// /plugin marketplace list reflects it.
	d.plugin("marketplace list")
	recvUntil(t, ch, sysTextContains("mymkt"))
}

// TestPluginDialog_MenuOpens checks the top menu opens and routing to "List
// marketplaces" works.
func TestPluginDialog_MenuOpens(t *testing.T) {
	mkt := buildCLILocalMarketplace(t, "mymkt")
	d, ch := newPluginDialogs(t)
	if _, err := d.pluginMgr.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}

	go d.plugin("")
	// The menu is a list dialog titled "Plugins".
	m := recvUntil(t, ch, func(m tea.Msg) bool {
		od, ok := m.(ui.OpenDialogMsg)
		return ok && od.Dialog != nil && od.Dialog.Title == "Plugins"
	})
	od := m.(ui.OpenDialogMsg)
	if len(od.Dialog.Items) != 4 {
		t.Fatalf("menu has %d items, want 4", len(od.Dialog.Items))
	}
	// Choose "List marketplaces".
	d.submit([]string{"mkt-list"})
	recvUntil(t, ch, sysTextContains("mymkt"))
}

// TestPluginDialog_BrowseInstall drives the browse → confirm → install flow.
func TestPluginDialog_BrowseInstall(t *testing.T) {
	mkt := buildCLILocalMarketplace(t, "mymkt")
	d, ch := newPluginDialogs(t)
	if _, err := d.pluginMgr.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatal(err)
	}

	go d.plugin("install")
	// First dialog: the plugin picker.
	m := recvUntil(t, ch, func(m tea.Msg) bool {
		od, ok := m.(ui.OpenDialogMsg)
		return ok && od.Dialog != nil && strings.Contains(od.Dialog.Title, "Select a plugin")
	})
	od := m.(ui.OpenDialogMsg)
	if len(od.Dialog.Items) == 0 || od.Dialog.Items[0].Value != "formatter@mymkt" {
		t.Fatalf("plugin picker items = %+v", od.Dialog.Items)
	}
	d.submit([]string{"formatter@mymkt"})

	// Second dialog: the install confirmation (Yes/No).
	recvUntil(t, ch, func(m tea.Msg) bool {
		od, ok := m.(ui.OpenDialogMsg)
		return ok && od.Dialog != nil && od.Dialog.Title == "Install plugin"
	})
	d.submit([]string{"yes"})

	recvUntil(t, ch, sysTextContains("Installed and enabled formatter@mymkt"))

	// The install reached the Manager.
	installed, err := d.pluginMgr.ListInstalled()
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].ID != "formatter@mymkt" {
		t.Fatalf("installed = %+v", installed)
	}
}

// TestPluginDialog_Unavailable: a nil Manager reports unavailability.
func TestPluginDialog_Unavailable(t *testing.T) {
	ch := make(chan tea.Msg, 8)
	d := &interactiveDialogs{send: func(m tea.Msg) { ch <- m }}
	d.plugin("")
	recvUntil(t, ch, sysTextContains("unavailable"))
}
