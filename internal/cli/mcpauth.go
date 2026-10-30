package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/mcphost"
	"github.com/umairge66/gocode/internal/ui"
)

// mcpAuthStore is the MCP OAuth credential store (<configDir>/mcp-auth.json,
// 0600). Nil when no user config directory is known, which disables OAuth
// sign-in (a 401 still reports "needs authentication").
func mcpAuthStore(env *Env) *mcp.TokenStore {
	dir := resolveSessionPaths(env).configDir
	if dir == "" {
		return nil
	}
	return mcp.NewTokenStore(filepath.Join(dir, mcp.AuthFileName))
}

// mcpNeedsAuthHint is appended to a needs-auth server line outside the
// interactive REPL, where no browser is ever opened.
const mcpNeedsAuthHint = "run /mcp in an interactive session to authenticate"

// /mcp server-menu actions.
const (
	mcpActionAuth      = "auth"
	mcpActionReauth    = "reauth"
	mcpActionClearAuth = "clear-auth"
	mcpActionReconnect = "reconnectMcpServer"
	mcpActionDetails   = "details"
	mcpActionCancel    = "cancel"
)

// mcpServerMenu opens the per-server action menu for a live server row of
// /mcp: authentication actions for OAuth-capable servers, Reconnect, and the
// existing detail view.
func (d *interactiveDialogs) mcpServerMenu(name string, statuses []mcphost.ServerStatus) {
	var st mcphost.ServerStatus
	found := false
	for _, s := range statuses {
		if s.Name == name {
			st, found = s, true
			break
		}
	}
	// A healthy server without OAuth has nothing to act on: show its detail
	// directly (the long-standing behaviour of selecting a /mcp row).
	if !found || d.mcpHost == nil || (!st.AuthCapable && st.State == mcphost.StateConnected) {
		d.showServerDetail(name, statuses)
		return
	}
	var items []ui.ListItem
	if st.AuthCapable {
		if st.HasAuth {
			items = append(items,
				ui.ListItem{Label: "Re-authenticate", Value: mcpActionReauth, Detail: "sign in again in the browser"},
				ui.ListItem{Label: "Clear authentication", Value: mcpActionClearAuth, Detail: "delete the stored tokens"})
		} else {
			items = append(items, ui.ListItem{Label: "Authenticate", Value: mcpActionAuth, Detail: "sign in in the browser"})
		}
	}
	if st.State != mcphost.StateNeedsAuth {
		items = append(items, ui.ListItem{Label: "Reconnect", Value: mcpActionReconnect})
	}
	items = append(items, ui.ListItem{Label: "Show details", Value: mcpActionDetails, Detail: mcpStatusDetail(st)})

	r, ok := d.openAndWait(context.Background(), ui.ListDialog(name+" MCP Server", items, 8))
	if !ok || r.cancelled {
		return
	}
	switch firstChosen(r.values) {
	case mcpActionAuth, mcpActionReauth:
		d.mcpAuthenticate(name, st.HasAuth)
	case mcpActionClearAuth:
		if err := d.mcpHost.ClearAuth(name); err != nil {
			d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Could not clear authentication for %s: %v", name, err)})
			return
		}
		if d.refreshTools != nil {
			d.refreshTools()
		}
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Authentication cleared for %s.", name)})
	case mcpActionReconnect:
		err := d.mcpHost.Reconnect(context.Background(), name)
		if d.refreshTools != nil {
			d.refreshTools()
		}
		switch {
		case err == nil:
			d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Reconnected to %s.", name)})
		case errors.Is(err, mcp.ErrNeedsAuth):
			d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("%s needs authentication — choose Authenticate in /mcp.", name)})
		default:
			d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Failed to reconnect to %s: %v", name, err)})
		}
	case mcpActionDetails:
		d.showServerDetail(name, statuses)
	}
}

// mcpAuthenticate runs the OAuth sign-in for one server. The authorization
// URL is always printed (so it works over SSH); the browser is opened best
// effort. A small dialog lets the user cancel while the loopback listener
// waits for the redirect.
func (d *interactiveDialogs) mcpAuthenticate(name string, wasAuthenticated bool) {
	d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Authenticating with %s… A browser window will open for authentication", name)})

	authCtx, cancelAuth := context.WithCancel(context.Background())
	defer cancelAuth()
	dlgCtx, closeDlg := context.WithCancel(context.Background())
	defer closeDlg()

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		err := d.mcpHost.Authenticate(authCtx, name, mcp.AuthorizeOptions{
			OnAuthorizationURL: func(u string) {
				d.sendMsg(ui.AppendSystemMsg{Text: "If your browser doesn't open automatically, copy this URL manually:\n" + u})
			},
		})
		done <- outcome{err}
		closeDlg()
	}()

	wait := ui.ListDialog(fmt.Sprintf("Authenticating with %s…", name), []ui.ListItem{
		{Label: "Cancel", Value: mcpActionCancel, Detail: "stop waiting for the browser sign-in"},
	}, 3)
	if r, ok := d.openAndWait(dlgCtx, wait); ok && (r.cancelled || firstChosen(r.values) == mcpActionCancel) {
		cancelAuth()
	}
	res := <-done
	if d.refreshTools != nil {
		d.refreshTools()
	}
	switch {
	case res.err == nil:
		verb := "Connected"
		if wasAuthenticated {
			verb = "Reconnected"
		}
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Authentication successful. %s to %s.", verb, name)})
	case errors.Is(res.err, context.Canceled):
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Authentication for %s cancelled.", name)})
	case errors.Is(res.err, mcp.ErrNeedsAuth):
		d.sendMsg(ui.AppendSystemMsg{Text: fmt.Sprintf("Tried reconnecting, but %s is still unauthorized. Make sure the browser sign-in completed, then try again from /mcp.", name)})
	default:
		d.sendMsg(ui.AppendErrorMsg{Text: fmt.Sprintf("Authentication failed for %s: %s", name, strings.TrimSpace(res.err.Error()))})
	}
}
