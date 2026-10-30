package daemon

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Background-session state file: <config>/jobs/<short>/state.json.
//
// The CLI seeds it when a session is dispatched, the daemon writes the
// terminal state when the session settles, and `stop` marks it stopped. The
// listing (`gocode agents --json`) and the id/name resolver read it.

// Seed detail strings.
const (
	DetailStarting  = "starting…"
	NeedsPrompt     = "send a prompt to start"
	DetailIdleNoMsg = "(idle — " + NeedsPrompt + ")"
)

// maxStateBytes caps state.json reads.
const maxStateBytes = 8 << 20

// JobState is the persisted state of one background session. Unknown fields
// written by other versions round-trip through Extra.
type JobState struct {
	State           string          `json:"state"`
	Detail          string          `json:"detail"`
	Tempo           string          `json:"tempo,omitempty"` // active|idle|blocked
	Needs           string          `json:"needs,omitempty"`
	InFlight        json.RawMessage `json:"inFlight,omitempty"`
	Output          json.RawMessage `json:"output"`
	Children        json.RawMessage `json:"children"`
	LinkScanOffset  int             `json:"linkScanOffset"`
	Template        string          `json:"template"`
	Routine         string          `json:"routine,omitempty"`
	RespawnFlags    []string        `json:"respawnFlags"`
	BgIsolation     string          `json:"bgIsolation,omitempty"`
	Intent          string          `json:"intent"`
	DisplayIntent   string          `json:"displayIntent,omitempty"`
	InitialPrompt   string          `json:"initialPrompt,omitempty"`
	QueuedPrompt    string          `json:"queuedPrompt,omitempty"`
	Name            string          `json:"name,omitempty"`
	NameSource      string          `json:"nameSource,omitempty"`
	Color           string          `json:"color,omitempty"`
	SessionID       string          `json:"sessionId"`
	ResumeSessionID string          `json:"resumeSessionId,omitempty"`
	DaemonShort     string          `json:"daemonShort,omitempty"`
	CliVersion      string          `json:"cliVersion,omitempty"`
	Cwd             string          `json:"cwd"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	FirstTerminalAt *string         `json:"firstTerminalAt"`
	LastTerminalAt  string          `json:"lastTerminalAt,omitempty"`
	WorktreePath    string          `json:"worktreePath,omitempty"`
	OriginCwd       string          `json:"originCwd,omitempty"`
	Backend         string          `json:"backend,omitempty"`
	PID             int             `json:"pid,omitempty"`

	// Pinned is read from pins.json, never written into state.json.
	Pinned bool `json:"-"`

	Extra map[string]json.RawMessage `json:"-"`
}

type jobStateAlias JobState

// knownJobStateKeys lists the JSON keys modelled by JobState.
var knownJobStateKeys = func() map[string]struct{} {
	b, _ := json.Marshal(jobStateAlias{})
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	keys := map[string]struct{}{}
	for k := range m {
		keys[k] = struct{}{}
	}
	for _, k := range []string{"tempo", "needs", "inFlight", "routine", "bgIsolation", "displayIntent",
		"initialPrompt", "queuedPrompt", "name", "nameSource", "color", "resumeSessionId", "daemonShort",
		"cliVersion", "lastTerminalAt", "worktreePath", "originCwd", "backend", "pid"} {
		keys[k] = struct{}{}
	}
	return keys
}()

// UnmarshalJSON decodes the modelled fields and keeps the rest in Extra.
func (s *JobState) UnmarshalJSON(b []byte) error {
	var a jobStateAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	*s = JobState(a)
	for k, v := range all {
		if _, ok := knownJobStateKeys[k]; ok {
			continue
		}
		// The writer strips these display-order sidecar fields.
		switch k {
		case "pinned", "sortOrder", "stateSortOrder", "group", "needs_you":
			continue
		}
		if s.Extra == nil {
			s.Extra = map[string]json.RawMessage{}
		}
		s.Extra[k] = v
	}
	if s.Tempo == "" {
		var needsYou bool
		if raw, ok := all["needs_you"]; ok {
			_ = json.Unmarshal(raw, &needsYou)
		}
		if needsYou {
			s.Tempo = "blocked"
		} else {
			s.Tempo = "idle"
		}
	}
	if s.RespawnFlags == nil {
		s.RespawnFlags = []string{}
	}
	if s.Backend == "" {
		s.Backend = "daemon"
	}
	return nil
}

// MarshalJSON encodes the modelled fields plus Extra.
func (s JobState) MarshalJSON() ([]byte, error) {
	a := jobStateAlias(s)
	if a.Output == nil {
		a.Output = json.RawMessage("null")
	}
	if a.Children == nil {
		a.Children = json.RawMessage("null")
	}
	if a.RespawnFlags == nil {
		a.RespawnFlags = []string{}
	}
	b, err := json.Marshal(a)
	if err != nil || len(s.Extra) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range s.Extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// validate is the minimal schema gate: the required string fields must exist.
func (s *JobState) validate(raw map[string]json.RawMessage) error {
	for _, k := range []string{"state", "detail", "template", "intent", "sessionId", "cwd", "createdAt", "updatedAt"} {
		v, ok := raw[k]
		if !ok {
			return fmt.Errorf("%s: Required", k)
		}
		var str string
		if json.Unmarshal(v, &str) != nil {
			return fmt.Errorf("%s: Expected string", k)
		}
	}
	return nil
}

// TerminalKind maps a state to success/failure/stopped, or "" when the state is
// not terminal.
func TerminalKind(state string) string {
	switch state {
	case "done":
		return "success"
	case "failed":
		return "failure"
	case "stopped":
		return "stopped"
	}
	return ""
}

// IsTerminalState reports whether state is done/failed/stopped.
func IsTerminalState(state string) bool { return TerminalKind(state) != "" }

// IsSettled reports a terminal state whose tempo is not active.
func (s *JobState) IsSettled() bool { return IsTerminalState(s.State) && s.Tempo != "active" }

// IsExecOnly reports an `--exec` job (a shell command; never re-run for you).
func (s *JobState) IsExecOnly() bool { return s.Template == "exec" && len(s.RespawnFlags) == 0 }

// IsBlocked reports a blocked, non-exec job.
func (s *JobState) IsBlocked() bool { return s.State == "blocked" && !s.IsExecOnly() }

// SeedParams are the inputs of NewJobState.
type SeedParams struct {
	Template      string
	InitialPrompt string
	Routine       string
	RespawnFlags  []string
	Intent        string
	Name          string
	NameSource    string
	Detail        *string
	Tempo         string
	Needs         string
	SessionID     string
	Cwd           string
	WorktreePath  string
	BgIsolation   string
	Interactive   bool
	Now           time.Time
}

// NewJobState builds the seed state written when a session is dispatched.
func NewJobState(p SeedParams) *JobState {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	ts := isoTime(now)
	detail := DetailStarting
	if p.Detail != nil {
		detail = *p.Detail
	}
	tempo := p.Tempo
	if tempo == "" {
		tempo = "active"
	}
	short := p.SessionID
	if len(short) > 8 {
		short = short[:8]
	}
	flags := p.RespawnFlags
	if flags == nil {
		flags = []string{}
	}
	st := &JobState{
		State:           "working",
		Detail:          detail,
		Tempo:           tempo,
		Needs:           p.Needs,
		Template:        p.Template,
		Routine:         p.Routine,
		RespawnFlags:    flags,
		BgIsolation:     p.BgIsolation,
		Intent:          p.Intent,
		Name:            p.Name,
		NameSource:      p.NameSource,
		InitialPrompt:   p.InitialPrompt,
		SessionID:       p.SessionID,
		ResumeSessionID: p.SessionID,
		DaemonShort:     short,
		Cwd:             p.Cwd,
		CreatedAt:       ts,
		UpdatedAt:       ts,
		WorktreePath:    p.WorktreePath,
		Backend:         "daemon",
	}
	if p.Interactive {
		st.Extra = map[string]json.RawMessage{"interactiveLineage": json.RawMessage("true")}
	}
	return st
}

// isoTime renders an ISO-8601 timestamp (UTC, millisecond precision).
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ISOTime formats t as a UTC ISO-8601 timestamp with milliseconds.
func ISOTime(t time.Time) string { return isoTime(t) }

// ReadJobState reads <jobDir>/state.json. A missing file returns (nil, nil);
// an oversize, non-regular or schema-invalid file returns an error.
func ReadJobState(jobDir string) (*JobState, error) {
	p := filepath.Join(jobDir, "state.json")
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("state.json is not a regular file")
	}
	if fi.Size() > maxStateBytes {
		return nil, fmt.Errorf("state.json is too large (%d bytes)", fi.Size())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("state.json read/parse failed — %v", err)
	}
	var st JobState
	if err := st.validate(raw); err != nil {
		return nil, fmt.Errorf("state.json schema validation failed — %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json read/parse failed — %v", err)
	}
	if st.LastTerminalAt == "" && IsTerminalState(st.State) && st.Tempo != "active" {
		if st.FirstTerminalAt != nil && *st.FirstTerminalAt != "" {
			st.LastTerminalAt = *st.FirstTerminalAt
		} else {
			st.LastTerminalAt = st.UpdatedAt
		}
	}
	return &st, nil
}

// WriteJobState writes <jobDir>/state.json (pretty JSON, mode 0600) atomically
// via a temp file + rename. A settled state gains lastTerminalAt; an unsettled
// one loses it.
func WriteJobState(jobDir string, st *JobState) error {
	c := *st
	if c.IsSettled() {
		if c.LastTerminalAt == "" {
			c.LastTerminalAt = c.UpdatedAt
		}
	} else {
		c.LastTerminalAt = ""
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(jobDir, "state.json"), b, 0o600)
}

// writeFileAtomic writes data to path via a sibling temp file and rename.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%x.tmp", filepath.Base(path), rnd))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// JobEntry is one listed job.
type JobEntry struct {
	ID    string
	State *JobState
}

// ListJobs reads every job directory under <config>/jobs with a readable
// state.json, marking pinned ones from <config>/jobs/pins.json.
func ListJobs(configDir string) []JobEntry {
	dir := JobsDir(configDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	pins := readPins(filepath.Join(dir, "pins.json"))
	var out []JobEntry
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		st, err := ReadJobState(filepath.Join(dir, e.Name()))
		if err != nil || st == nil {
			continue
		}
		if _, ok := pins[e.Name()]; ok {
			st.Pinned = true
		}
		out = append(out, JobEntry{ID: e.Name(), State: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func readPins(path string) map[string]struct{} {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > maxStateBytes {
		return nil
	}
	var ids []any
	if json.Unmarshal(b, &ids) != nil {
		return nil
	}
	m := map[string]struct{}{}
	for _, v := range ids {
		if s, ok := v.(string); ok {
			m[s] = struct{}{}
		}
	}
	return m
}

// ListJobIDs returns the short-id directory names under <config>/jobs.
func ListJobIDs(configDir string) []string {
	ents, err := os.ReadDir(JobsDir(configDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if ShortIDPattern.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// staleGrace is how long a fresh, not-yet-live job is trusted.
const staleGrace = 5 * time.Second

// MarkStale reconciles listed jobs with the daemon's live set: an unsettled
// job that is neither live nor freshly created is shown as failed (or blocked,
// for a job that was already blocked).
func MarkStale(entries []JobEntry, live map[string]bool, now time.Time) []JobEntry {
	out := make([]JobEntry, 0, len(entries))
	for _, e := range entries {
		st := e.State
		if st.IsSettled() || live[e.ID] {
			out = append(out, e)
			continue
		}
		if created, err := time.Parse(time.RFC3339Nano, st.CreatedAt); err == nil && now.Sub(created) < staleGrace {
			out = append(out, e)
			continue
		}
		c := *st
		if c.IsBlocked() {
			c.Tempo = "blocked"
			c.InFlight = nil
		} else {
			c.State = "failed"
			c.Tempo = "idle"
			c.Needs = ""
			c.InFlight = nil
			c.Detail = strings.TrimSuffix(c.Detail, "; respawning")
		}
		out = append(out, JobEntry{ID: e.ID, State: &c})
	}
	return out
}
