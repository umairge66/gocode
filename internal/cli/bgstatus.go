package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/provider"
)

// -----------------------------------------------------------------------------
// Live status of a background worker: the interactive session, when it runs
// as a daemon-backed background session (GOCODE_CODE_SESSION_KIND=bg with a
// GOCODE_JOB_DIR), keeps its own <job>/state.json current so `gocode agents`
// shows what it is doing:
//
//   - a turn starts           -> tempo "active", the user's message as detail
//   - mid-turn (≤ every 15 s) -> working/active, the latest text or tool
//   - a permission prompt or question is open -> tempo "blocked", needs = the
//     prompt line ("approve Bash: rm -rf build"); cleared when answered
//   - the turn ends           -> the classifier's verdict (working / blocked /
//     done / failed, detail, needs, result)
//   - the turn is aborted     -> tempo "idle"
//   - the API call fails      -> blocked (login, rate limit, …) or failed
//
// Each change is also appended to <job>/timeline.jsonl. Every write is a
// read-modify-write of state.json serialised through one lock, and nothing is
// sent anywhere: the state lives on local disk only.

// midTurnDebounce is the mid-turn classification interval for a background
// worker.
const midTurnDebounce = 15 * time.Second

// preBootStates are the daemon-side boot states a worker clears once it is
// up.
var preBootStates = map[string]bool{"starting": true, "resuming": true, "adopted": true, "crashed": true}

// jobStatus reports the session's state into its background job record. A nil
// *jobStatus is valid and does nothing (the session is not a background
// worker).
type jobStatus struct {
	jobDir     string
	sessionID  string
	cliVersion string
	now        func() time.Time
	busy       func() bool // background subagents still running

	mu              sync.Mutex // serialises the state.json read-modify-writes
	lastClassifyAt  time.Time
	prevState       string
	kicked          bool
	turnActive      bool
	pendingText     string
	hasPending      bool
	block           string // the open permission prompt / question ("" none)
	lastBridgeNeeds string
}

// newJobStatus returns the reporter for a background worker, or nil.
func newJobStatus(getenv func(string) string, sessionID, version string) *jobStatus {
	if getenv == nil || getenv("GOCODE_CODE_SESSION_KIND") != "bg" {
		return nil
	}
	dir := getenv("GOCODE_JOB_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	return &jobStatus{jobDir: dir, sessionID: sessionID, cliVersion: version, now: time.Now}
}

// update applies fn to the current state and writes it back. fn returns false
// to skip the write. The caller holds no lock.
func (j *jobStatus) update(fn func(st *daemon.JobState) bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.updateLocked(fn)
}

func (j *jobStatus) updateLocked(fn func(st *daemon.JobState) bool) {
	st, err := daemon.ReadJobState(j.jobDir)
	if err != nil || st == nil {
		return
	}
	if !fn(st) {
		return
	}
	_ = daemon.WriteJobState(j.jobDir, st)
}

func (j *jobStatus) stamp() string { return daemon.ISOTime(j.now()) }

// timeline appends one {at,state,detail,text} line to timeline.jsonl.
func (j *jobStatus) timeline(at, state, detail, text string) {
	b, err := json.Marshal(map[string]string{"at": at, "state": state, "detail": detail, "text": text})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(j.jobDir, "timeline.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

var reWhitespace = regexp.MustCompile(`\s+`)

// collapse folds whitespace runs to single spaces and trims.
func collapse(s string) string { return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " ")) }

// stripReminders keeps the text after the last </system-reminder>.
func stripReminders(s string) string {
	if i := strings.LastIndex(s, "</system-reminder>"); i >= 0 {
		s = s[i+len("</system-reminder>"):]
	}
	return strings.TrimSpace(s)
}

// clearPreBoot moves a daemon boot state (starting/resuming/adopted/crashed)
// to "running" once the session is up.
func (j *jobStatus) clearPreBoot() {
	if j == nil {
		return
	}
	j.update(func(st *daemon.JobState) bool {
		if !preBootStates[st.State] || st.Tempo == "blocked" {
			return false
		}
		st.State, st.Tempo, st.UpdatedAt = "running", "idle", j.stamp()
		return true
	})
}

// notePrompt records the user text of the turn about to start.
func (j *jobStatus) notePrompt(text string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.pendingText, j.hasPending = text, true
	j.mu.Unlock()
}

// turnStarted marks the turn active (its user text from notePrompt).
func (j *jobStatus) turnStarted() {
	if j == nil {
		return
	}
	j.mu.Lock()
	text := j.pendingText
	j.pendingText, j.hasPending = "", false
	j.turnActive = true
	j.kicked = true
	j.mu.Unlock()
	j.markTurnActive(text, true)
}

// markTurnActive flips the job to tempo "active" when a turn starts, showing
// the user's message as the detail unless it is the job's own intent.
func (j *jobStatus) markTurnActive(userText string, isTurnStart bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var line *[4]string
	j.updateLocked(func(st *daemon.JobState) bool {
		if st.Tempo == "active" {
			return false
		}
		if st.IsSettled() && userText == "" && !isTurnStart {
			return false
		}
		if st.Tempo == "blocked" && j.block != "" && (j.block == st.Needs || j.lastBridgeNeeds == st.Needs) {
			return false
		}
		c := ""
		if userText != "" {
			c = collapse(stripReminders(userText))
		}
		own := false
		for _, w := range []string{st.Intent, st.InitialPrompt} {
			if w != "" && collapse(w) == c {
				own = true
			}
		}
		at := j.stamp()
		if c != "" && !own {
			st.Detail = daemon.TruncateDetail(c)
			st.Output = json.RawMessage("null")
			line = &[4]string{at, st.State, st.Detail, ""}
		}
		st.Tempo, st.InFlight, st.Needs, st.UpdatedAt = "active", nil, "", at
		return true
	})
	if line != nil {
		j.timeline(line[0], line[1], line[2], line[3])
	}
}

// midTurn reports progress during a turn: working/active with the latest
// assistant text (more than 8 characters) or else the running tool's
// description. Debounced to one write per 15 s.
func (j *jobStatus) midTurn(text, toolDesc string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	kick := !j.kicked
	j.kicked = true
	j.mu.Unlock()
	if kick {
		j.markTurnActive("", false)
	}
	detail := ""
	if c := collapse(text); len([]rune(c)) > 8 {
		detail = daemon.TruncateDetail(c)
	} else {
		detail = daemon.TruncateDetail(collapse(toolDesc))
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	if !j.lastClassifyAt.IsZero() && now.Sub(j.lastClassifyAt) < midTurnDebounce {
		return
	}
	j.lastClassifyAt = now
	j.updateLocked(func(st *daemon.JobState) bool {
		if st.Tempo == "blocked" || st.State == "blocked" {
			return false
		}
		if detail == "" {
			detail = st.Detail
		}
		j.prevState = "working"
		st.State, st.Detail, st.Tempo, st.Needs, st.InFlight = "working", detail, "active", "", nil
		st.UpdatedAt = j.stamp()
		return true
	})
}

// hasLetterOrDigit is /[\p{L}\p{N}]/u.
func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// turnEnded classifies the finished turn's assistant text and writes the
// verdict.
func (j *jobStatus) turnEnded(text string) {
	if j == nil {
		return
	}
	busy := j.busy != nil && j.busy()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.turnActive, j.kicked, j.lastClassifyAt = false, false, time.Time{}
	var line *[4]string
	j.updateLocked(func(st *daemon.JobState) bool {
		var c *daemon.Classification
		if !hasLetterOrDigit(text) {
			state := st.State
			if state == "" {
				state = j.prevState
			}
			if state == "" {
				state = "working"
			}
			c = &daemon.Classification{Branch: "no-text-turn", State: state, Tempo: "idle", Detail: st.Detail}
			if state == "blocked" {
				c.Tempo, c.Needs = "blocked", st.Needs
			}
		} else {
			c = daemon.Classify(text)
		}
		if busy {
			if daemon.IsTerminalState(c.State) {
				c.State = j.prevState
				if c.State == "" {
					c.State = "working"
				}
			}
			if c.Tempo == "idle" || c.Tempo == "blocked" {
				c.Tempo = "active"
			}
		} else if c.Tempo == "active" && c.State == "working" {
			c.Tempo = "idle"
		}
		j.prevState = c.State
		terminal := daemon.IsTerminalState(c.State)
		keepBlock := st.Tempo == "blocked" && c.Tempo != "blocked" && j.block != "" &&
			(j.block == st.Needs || j.lastBridgeNeeds == st.Needs)
		tempo, needs := c.Tempo, ""
		switch {
		case terminal:
			tempo = "idle"
		case keepBlock:
			tempo, needs = "blocked", st.Needs
		case c.Tempo == "blocked":
			needs = c.Needs
		}
		at := j.stamp()
		st.State, st.Detail, st.Tempo, st.Needs, st.InFlight = c.State, c.Detail, tempo, needs, nil
		if c.Result != "" {
			b, _ := json.Marshal(map[string]string{"result": c.Result})
			st.Output = b
		} else {
			st.Output = json.RawMessage("null")
		}
		if j.sessionID != "" {
			st.ResumeSessionID = j.sessionID
		}
		if j.cliVersion != "" {
			st.CliVersion = j.cliVersion
		}
		st.UpdatedAt = at
		if terminal && st.FirstTerminalAt == nil {
			st.FirstTerminalAt = &at
		}
		line = &[4]string{at, c.State, c.Detail, truncateRunesTo(text, 4000)}
		return true
	})
	if line != nil {
		j.timeline(line[0], line[1], line[2], line[3])
	}
}

// turnAborted settles an interrupted (or command-only) turn: an active tempo
// becomes idle.
func (j *jobStatus) turnAborted() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.turnActive, j.kicked, j.lastClassifyAt = false, false, time.Time{}
	j.updateLocked(func(st *daemon.JobState) bool {
		if st.Tempo != "active" {
			return false
		}
		st.Tempo, st.InFlight, st.UpdatedAt = "idle", nil, j.stamp()
		return true
	})
}

// apiFailure records a turn that ended on an API error.
func (j *jobStatus) apiFailure(err error) {
	if j == nil || err == nil {
		return
	}
	kind := apiErrorKind(err)
	state, needs := daemon.APIFailureState(kind, err.Error())
	j.mu.Lock()
	defer j.mu.Unlock()
	j.turnActive, j.kicked, j.lastClassifyAt = false, false, time.Time{}
	if state == "" {
		return
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "Please run /login · ")
	msg = strings.TrimPrefix(msg, "Failed to authenticate. ")
	msg = strings.TrimSuffix(msg, " · Please run /login")
	if msg == "Not logged in" {
		msg = ""
	}
	p := daemon.TruncateDetail(collapse(msg))
	full := needs
	if p != "" {
		full += " · " + p
	}
	var line *[4]string
	j.updateLocked(func(st *daemon.JobState) bool {
		if st.IsSettled() {
			return false
		}
		at := j.stamp()
		tempo, n := "blocked", full
		if state == "failed" {
			tempo, n = "idle", ""
			if st.FirstTerminalAt == nil {
				st.FirstTerminalAt = &at
			}
		}
		st.State, st.Detail, st.Tempo, st.Needs, st.InFlight = state, p, tempo, n, nil
		st.Output = json.RawMessage("null")
		st.UpdatedAt = at
		line = &[4]string{at, state, p, p}
		return true
	})
	j.prevState = state
	if line != nil {
		j.timeline(line[0], line[1], line[2], line[3])
	}
}

// apiErrorKind maps a turn error to an API error kind.
func apiErrorKind(err error) string {
	if isSetupError(err) || strings.Contains(err.Error(), "run /login") {
		return "authentication_failed"
	}
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.Category {
	case provider.CategoryAuth:
		return "authentication_failed"
	case provider.CategoryCredits:
		return "billing_error"
	case provider.CategoryRateLimit:
		return "rate_limit"
	case provider.CategoryServer:
		if apiErr.Status == 529 || strings.Contains(strings.ToLower(apiErr.Message), "overload") {
			return "overloaded"
		}
		return "server_error"
	case provider.CategoryNetwork:
		return ""
	}
	if apiErr.Status == 400 || apiErr.Status == 413 {
		return "invalid_request"
	}
	return "unknown"
}

// setBlock marks the job blocked on an open permission prompt or question.
func (j *jobStatus) setBlock(text string) {
	if j == nil || text == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.block = text
	j.lastBridgeNeeds = text
	j.updateLocked(func(st *daemon.JobState) bool {
		if daemon.IsTerminalState(st.State) {
			return false
		}
		if st.Tempo == "blocked" && st.Needs == text {
			return false
		}
		st.Tempo, st.Needs, st.InFlight, st.UpdatedAt = "blocked", text, nil, j.stamp()
		return true
	})
}

// clearBlock lifts the block once the prompt is answered: back to "active"
// while the turn runs, else "idle".
func (j *jobStatus) clearBlock() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	prev := j.lastBridgeNeeds
	j.block = ""
	active := j.turnActive
	j.updateLocked(func(st *daemon.JobState) bool {
		if st.Tempo != "blocked" || (prev != "" && st.Needs != prev) {
			return false
		}
		tempo := "idle"
		if active {
			tempo = "active"
		}
		st.Tempo, st.Needs, st.InFlight, st.UpdatedAt = tempo, "", nil, j.stamp()
		return true
	})
}

// permissionBlockText is the needs line of an open permission prompt.
func permissionBlockText(toolName string, input json.RawMessage) string {
	if toolName == "ExitPlanMode" {
		return "approve plan"
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	r := ""
	for _, k := range []string{"command", "file_path", "url"} {
		if v, ok := in[k].(string); ok {
			r = v
			break
		}
	}
	if r != "" && !strings.Contains(toolName, r) {
		return daemon.TruncateDetail(collapse("approve " + toolName + ": " + r))
	}
	return "approve " + toolName
}

// questionBlockText is the needs line of an open AskUserQuestion.
func questionBlockText(question string, options []string) string {
	if question == "" {
		return "answer question"
	}
	s := "answer: " + question
	if len(options) > 0 {
		s += " (" + strings.Join(options, " · ") + ")"
	}
	return daemon.TruncateDetail(collapse(s))
}

func truncateRunesTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
