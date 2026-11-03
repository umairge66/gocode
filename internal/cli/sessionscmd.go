package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/sessionstore"
)

// `gocode sessions <list|show|rename|delete>`: the saved conversations under
// <gocode root>/projects, as /resume sees them. Every subcommand works on the
// current directory's project; --all-projects widens the listing and the
// lookup to every project directory. Nothing leaves the machine.
//
//	list   [--json] [--all-projects]
//	show   <session> [--json] [--full] [--all-projects]
//	rename <session> <title> [--all-projects]
//	delete <session> [-y|--yes] [--all-projects]
//
// show resolves <session> like --resume (id, unique id prefix, custom title,
// title); rename and delete, which change data, accept only an id or a
// unique id prefix. delete asks first unless --yes is given, as purge does.

// sessionsBoolOpts is the option spec shared by the subcommands.
var sessionsBoolOpts = map[string]string{
	"--json": "json", "--all-projects": "all", "--full": "full",
	"-y": "yes", "--yes": "yes",
}

// sessionsCmd carries the resolved store and options of one invocation.
type sessionsCmd struct {
	env     *Env
	store   *sessionstore.Store
	base    string
	cwd     string
	all     bool
	json    bool
	full    bool
	yes     bool
	operand []string
}

// runSessionsCmd parses `sessions <sub> …` and runs it.
func runSessionsCmd(env *Env, t []string) int {
	args := argsAfter(t, "sessions")
	o, err := scanOpts(args, sessionsBoolOpts, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.pos) == 0 {
		writeCommandUsage(env, "sessions")
		return 1
	}
	paths := resolveSessionPaths(env)
	base := sessionConfigHome(env, paths)
	if base == "" {
		fmt.Fprintln(env.Stderr, "Could not resolve the gocode data directory (set HOME, XDG_CONFIG_HOME or GOCODE_CONFIG_DIR).")
		return 1
	}
	store := sessionstore.New(base)
	if legacy := confighome.LegacyDir(paths.home); legacy != "" {
		store.MovedFrom = []string{legacy}
	}
	c := &sessionsCmd{
		env: env, store: store, base: base, cwd: paths.cwd,
		all: o.has("all"), json: o.has("json"), full: o.has("full"), yes: o.has("yes"),
		operand: o.pos[1:],
	}
	switch o.pos[0] {
	case "list", "ls":
		return c.list()
	case "show":
		return c.show()
	case "rename":
		return c.rename()
	case "delete", "rm":
		return c.delete()
	}
	fmt.Fprintf(env.Stderr, "error: unknown command 'sessions %s'\n", o.pos[0])
	writeCommandUsage(env, "sessions")
	return 1
}

// sessionEntry is one session as --json reports it.
type sessionEntry struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CustomTitle  string    `json:"customTitle,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	FirstPrompt  string    `json:"firstPrompt,omitempty"`
	Cwd          string    `json:"cwd,omitempty"`
	GitBranch    string    `json:"gitBranch,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	Modified     time.Time `json:"modified,omitzero"`
	MessageCount int       `json:"messageCount"`
	Tags         []string  `json:"tags"`
	Path         string    `json:"path"`
}

func toSessionEntry(m sessionstore.Meta) sessionEntry {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return sessionEntry{
		ID: m.ID, Title: m.Title, CustomTitle: m.CustomTitle, Summary: m.Summary,
		FirstPrompt: m.FirstPrompt, Cwd: m.Cwd, GitBranch: m.GitBranch,
		Created: m.Created, Modified: m.Modified, MessageCount: m.MessageCount,
		Tags: tags, Path: m.Path,
	}
}

// metas lists the sessions in scope.
func (c *sessionsCmd) metas() []sessionstore.Meta {
	var metas []sessionstore.Meta
	if c.all {
		metas, _ = c.store.ListAll()
	} else {
		metas, _ = c.store.List(c.cwd)
	}
	return metas
}

// resolve finds the operand session. idOnly restricts it to an id or a
// unique id prefix.
func (c *sessionsCmd) resolve(value string, idOnly bool) (sessionstore.Meta, bool) {
	var m sessionstore.Meta
	var ok bool
	if c.all {
		m, ok = c.store.FindAll(value)
	} else {
		m, ok = c.store.Find(c.cwd, value)
	}
	if ok && idOnly && m.ID != value && !strings.HasPrefix(m.ID, value) {
		ok = false
	}
	if !ok {
		scope := "in this project"
		if c.all {
			scope = "in any project"
		} else {
			scope += " (use --all-projects to search every project)"
		}
		fmt.Fprintf(c.env.Stderr, "No session matching %q %s.\n", value, scope)
	}
	return m, ok
}

func (c *sessionsCmd) list() int {
	metas := c.metas()
	if c.json {
		out := make([]sessionEntry, 0, len(metas))
		for _, m := range metas {
			out = append(out, toSessionEntry(m))
		}
		return c.writeJSON(out)
	}
	if len(metas) == 0 {
		if c.all {
			fmt.Fprintln(c.env.Stdout, "No saved sessions.")
		} else {
			fmt.Fprintln(c.env.Stdout, "No saved sessions in this project.")
		}
		return 0
	}
	for _, m := range metas {
		fmt.Fprintf(c.env.Stdout, "%s  %s\n", m.ID, m.Title)
		detail := resumeDetail(m)
		if c.all && m.Cwd != "" {
			detail = m.Cwd + " · " + detail
		}
		fmt.Fprintf(c.env.Stdout, "    %s\n", detail)
	}
	return 0
}

func (c *sessionsCmd) show() int {
	if len(c.operand) != 1 {
		fmt.Fprintln(c.env.Stderr, "Usage: gocode sessions show <session> [--json] [--full] [--all-projects]")
		return 1
	}
	m, ok := c.resolve(c.operand[0], false)
	if !ok {
		return 1
	}
	recs, compactions, err := c.store.Records(m, c.full)
	if err != nil {
		fmt.Fprintf(c.env.Stderr, "Could not read session %s: %v\n", m.ID, err)
		return 1
	}
	if c.json {
		if recs == nil {
			recs = []sessionstore.Record{}
		}
		return c.writeJSON(struct {
			Session     sessionEntry          `json:"session"`
			Compactions int                   `json:"compactions"`
			Full        bool                  `json:"full"`
			Messages    []sessionstore.Record `json:"messages"`
		}{toSessionEntry(m), compactions, c.full, recs})
	}
	w := c.env.Stdout
	fmt.Fprintf(w, "Session:  %s\nTitle:    %s\n", m.ID, m.Title)
	if m.Cwd != "" {
		fmt.Fprintf(w, "Cwd:      %s\n", m.Cwd)
	}
	if m.GitBranch != "" {
		fmt.Fprintf(w, "Branch:   %s\n", m.GitBranch)
	}
	fmt.Fprintf(w, "Created:  %s\nModified: %s\nMessages: %d\n", fmtTime(m.Created), fmtTime(m.Modified), m.MessageCount)
	if len(m.Tags) > 0 {
		fmt.Fprintf(w, "Tags:     %s\n", strings.Join(m.Tags, ", "))
	}
	if compactions > 0 {
		scope := "showing the history after the last one (--full for every line)"
		if c.full {
			scope = "showing every line"
		}
		fmt.Fprintf(w, "Compactions: %d (%s)\n", compactions, scope)
	}
	for _, r := range recs {
		fmt.Fprintln(w)
		if r.Message == nil {
			fmt.Fprintf(w, "── %s %s  %s\n", r.Type, r.Subtype, r.UUID)
			continue
		}
		label := r.Message.Role
		switch {
		case r.IsCompactSummary:
			label += " (compact summary)"
		case r.IsMeta:
			label += " (command prompt)"
		}
		fmt.Fprintf(w, "── %s  %s  %s\n", label, r.UUID, fmtTime(r.Timestamp))
		for _, line := range recordLines(*r.Message) {
			fmt.Fprintln(w, "  "+line)
		}
	}
	return 0
}

// recordLines renders a message's blocks for `sessions show`.
func recordLines(m agent.Message) []string {
	var out []string
	for _, b := range m.Content {
		switch b.Type {
		case agent.BlockText:
			out = append(out, strings.Split(strings.TrimRight(b.Text, "\n"), "\n")...)
		case agent.BlockToolUse:
			out = append(out, fmt.Sprintf("[tool_use %s %s]", b.Name, truncateRunesTo(string(b.Input), 200)))
		case agent.BlockToolResult:
			data, _ := json.Marshal(b)
			out = append(out, fmt.Sprintf("[tool_result for %s, %d bytes]", b.ToolUseID, len(data)))
		default:
			out = append(out, "["+string(b.Type)+"]")
		}
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func (c *sessionsCmd) rename() int {
	if len(c.operand) < 2 {
		fmt.Fprintln(c.env.Stderr, "Usage: gocode sessions rename <session-id> <title> [--all-projects]")
		return 1
	}
	m, ok := c.resolve(c.operand[0], true)
	if !ok {
		return 1
	}
	title := strings.Join(c.operand[1:], " ")
	if err := c.store.Rename(m, title); err != nil {
		fmt.Fprintf(c.env.Stderr, "Could not rename session %s: %v\n", m.ID, err)
		return 1
	}
	fmt.Fprintf(c.env.Stdout, "Renamed session %s to %q.\n", shortID(m.ID), strings.Join(strings.Fields(title), " "))
	return 0
}

func (c *sessionsCmd) delete() int {
	if len(c.operand) != 1 {
		fmt.Fprintln(c.env.Stderr, "Usage: gocode sessions delete <session-id> [-y|--yes] [--all-projects]")
		return 1
	}
	m, ok := c.resolve(c.operand[0], true)
	if !ok {
		return 1
	}
	// The transcript, then the session's side state (saved tool results,
	// file-edit history, task list, debug log) — the per-session items purge
	// deletes.
	items := []string{m.Path}
	for _, p := range []string{
		filepath.Join(filepath.Dir(m.Path), m.ID),
		filepath.Join(c.base, "file-history", m.ID),
		filepath.Join(c.base, "tasks", m.ID),
		filepath.Join(c.base, "debug", m.ID+".txt"),
	} {
		if exists(p) {
			items = append(items, p)
		}
	}
	fmt.Fprintf(c.env.Stdout, "Session %s: %s (%d %s)\n", m.ID, m.Title, m.MessageCount, pluralize(m.MessageCount, "message", "messages"))
	for _, p := range items {
		fmt.Fprintf(c.env.Stdout, "  %s\n", p)
	}
	if !c.yes && !confirmYesNoFrom(c.env, newStdinReader(c.env), "Delete this session? This cannot be undone.") {
		fmt.Fprintln(c.env.Stderr, "Aborted.")
		return 1
	}
	failed := false
	for _, p := range items {
		if err := removeUnder(c.base, p); err != nil {
			fmt.Fprintf(c.env.Stderr, "Could not delete %s — %v\n", p, err)
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintf(c.env.Stdout, "Deleted session %s.\n", shortID(m.ID))
	return 0
}

func (c *sessionsCmd) writeJSON(v any) int {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(c.env.Stderr, err)
		return 1
	}
	fmt.Fprintln(c.env.Stdout, string(data))
	return 0
}
