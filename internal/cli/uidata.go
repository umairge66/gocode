package cli

import (
	"encoding/json"
	"strings"

	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/ui"
)

// Data the full-screen UI shows beside the conversation: the todo list for
// the sidebar (from the TodoWrite tool's input) and the recent sessions for
// the welcome screen (from the local session store). Both are read locally.

// todosFromInput decodes a TodoWrite call's todo list. ok is false for any
// other tool or an input without a todo list.
func todosFromInput(tool string, input json.RawMessage) ([]ui.Todo, bool) {
	if tool != "TodoWrite" || len(input) == 0 {
		return nil, false
	}
	var in struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Todos == nil {
		return nil, false
	}
	out := make([]ui.Todo, 0, len(in.Todos))
	for _, t := range in.Todos {
		text := t.Content
		if t.Status == string(ui.TodoInProgress) && t.ActiveForm != "" {
			text = t.ActiveForm
		}
		out = append(out, ui.Todo{Text: strings.TrimSpace(text), Status: ui.TodoStatus(t.Status)})
	}
	return out, true
}

// recentSessionsLimit is how many saved sessions the welcome screen lists.
const recentSessionsLimit = 4

// recentSessionsMsg lists this project's most recent saved sessions (not the
// current one) for the welcome screen.
func recentSessionsMsg(store *sessionstore.Store, cwd, current string) ui.SetRecentSessionsMsg {
	if store == nil {
		return ui.SetRecentSessionsMsg{}
	}
	metas, _ := store.List(cwd)
	var out []ui.RecentSession
	for _, m := range metas {
		if m.ID == current || m.MessageCount == 0 {
			continue
		}
		title := m.Title
		if title == "" {
			title = shortID(m.ID)
		}
		out = append(out, ui.RecentSession{Title: title, When: humanizeSince(m.Modified)})
		if len(out) == recentSessionsLimit {
			break
		}
	}
	return ui.SetRecentSessionsMsg{Sessions: out}
}
