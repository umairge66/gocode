package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

// -----------------------------------------------------------------------------
// `gocode agents --json [--all] [--cwd <path>]`: the machine-readable listing
// of background sessions (the interactive fleet view is the TTY form of
// `gocode agents`).
//
// Each entry: {pid?, id, cwd, kind:"background", startedAt, sessionId, name?,
// state}, sorted by startedAt. Without --all, sessions that are neither live
// nor working/blocked are omitted. There is no live-process registry of
// interactive sessions, so only background sessions are listed.

type agentsJSONEntry struct {
	PID       int    `json:"pid,omitempty"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	StartedAt int64  `json:"startedAt"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name,omitempty"`
	State     string `json:"state"`
	// detail is the state's detail line (table view only; not in --json).
	detail string
}

// agentsDisplayState maps a job to its listing state.
func agentsDisplayState(st *daemon.JobState) string {
	if st.IsSettled() {
		switch daemon.TerminalKind(st.State) {
		case "success":
			return "done"
		case "failure":
			return "failed"
		default:
			return "stopped"
		}
	}
	if st.Tempo == "blocked" {
		return "blocked"
	}
	return "working"
}

// liveJobRecords asks the daemon which jobs are live (empty when it is down).
func liveJobRecords(configDir string) map[string]daemon.JobRecord {
	out := map[string]daemon.JobRecord{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rep := daemon.NewControlClient(configDir).Request(ctx, daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpList})
	if !rep.OK || len(rep.Jobs) == 0 {
		return out
	}
	var recs []daemon.JobRecord
	if json.Unmarshal(rep.Jobs, &recs) != nil {
		return out
	}
	for _, r := range recs {
		out[r.Short] = r
	}
	return out
}

// buildAgentsJSON assembles the listing.
func buildAgentsJSON(configDir, cwdFilter string, all bool, live map[string]daemon.JobRecord, now time.Time) []agentsJSONEntry {
	liveSet := map[string]bool{}
	for k, r := range live {
		if r.Outcome == "" {
			liveSet[k] = true
		}
	}
	root := ""
	if cwdFilter != "" {
		if abs, err := filepath.Abs(cwdFilter); err == nil {
			root = abs
		}
	}
	under := func(p string) bool {
		if root == "" {
			return true
		}
		rel, err := filepath.Rel(root, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	var out []agentsJSONEntry
	for _, e := range daemon.MarkStale(daemon.ListJobs(configDir), liveSet, now) {
		st := e.State
		origin := st.OriginCwd
		if origin == "" {
			origin = st.Cwd
		}
		if !under(origin) {
			continue
		}
		state := agentsDisplayState(st)
		rec, isLive := live[e.ID]
		if isLive && rec.Outcome != "" {
			isLive = false
		}
		if !all && !isLive && state != "working" && state != "blocked" {
			continue
		}
		entry := agentsJSONEntry{ID: e.ID, Cwd: st.Cwd, Kind: "background", SessionID: st.SessionID,
			Name: strings.TrimSpace(firstNonEmpty(st.Name, st.DisplayIntent, st.Intent)), State: state,
			detail: agentsDetail(st)}
		if t, err := time.Parse(time.RFC3339Nano, st.CreatedAt); err == nil {
			entry.StartedAt = t.UnixMilli()
		}
		if isLive {
			entry.PID = rec.PID
			if rec.StartedAt > 0 {
				entry.StartedAt = rec.StartedAt
			}
			if rec.Cwd != "" {
				entry.Cwd = rec.Cwd
			}
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	if out == nil {
		out = []agentsJSONEntry{}
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// runAgentsJSON prints the listing.
func runAgentsJSON(env *Env, cwdFilter string, all bool) int {
	configDir := daemonConfigHome(env)
	entries := buildAgentsJSON(configDir, cwdFilter, all, liveJobRecords(configDir), time.Now())
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	fmt.Fprintln(env.Stdout, string(b))
	return 0
}

// runAgentsTable prints the background sessions as a table (in place of an
// interactive fleet view).
func runAgentsTable(env *Env, cwdFilter string, all bool) int {
	configDir := daemonConfigHome(env)
	entries := buildAgentsJSON(configDir, cwdFilter, all, liveJobRecords(configDir), time.Now())
	if len(entries) == 0 {
		fmt.Fprintln(env.Stdout, "No background sessions. Start one with: gocode --bg \"<prompt>\"")
		return 0
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tNAME\tDETAIL\tSTARTED\tCWD")
	for _, e := range entries {
		started := time.UnixMilli(e.StartedAt).Format("2006-01-02 15:04")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.State, firstNonEmpty(e.Name, "-"), firstNonEmpty(e.detail, "-"), started, e.Cwd)
	}
	tw.Flush()
	fmt.Fprintln(env.Stdout, "\n  gocode attach <id>   open in this terminal\n  gocode logs <id>     show recent output\n  gocode stop <id>     stop a session")
	return 0
}

// agentsDetail is the table's detail column: what a blocked job needs, else
// the state detail, on one line and capped at 60 characters.
func agentsDetail(st *daemon.JobState) string {
	d := st.Detail
	if st.Tempo == "blocked" && st.Needs != "" {
		d = st.Needs
	}
	d = strings.Join(strings.Fields(d), " ")
	if r := []rune(d); len(r) > 60 {
		d = string(r[:59]) + "…"
	}
	return d
}
