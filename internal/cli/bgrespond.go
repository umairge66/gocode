package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/errutil"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// -----------------------------------------------------------------------------
// `gocode agents pending [--json]` and `gocode agents respond <job> …`: the
// prompts background sessions are blocked on — a tool call's permission
// prompt, an AskUserQuestion dialog or a plan approval — listed from the
// daemon's `list` (JobRecord.Permission) and answered with its authed
// permission-response op (daemon bgpermission.go).

const (
	agentsPendingUsage = "gocode agents pending [--json]"
	agentsRespondUsage = "gocode agents respond <job> allow|deny [--message <text>] [--always] [--mode <mode>] [--answer <label>]…"
)

// bgControlClient returns the daemon client of configDir (a seam for tests).
var bgControlClient = daemon.NewControlClient

// pendingPromptEntry is one --json row of `agents pending`.
type pendingPromptEntry struct {
	Job         string          `json:"job"`
	Name        string          `json:"name,omitempty"`
	Kind        string          `json:"kind"`
	RequestID   string          `json:"requestId"`
	ToolName    string          `json:"toolName"`
	Summary     string          `json:"summary"`
	Input       json.RawMessage `json:"input"`
	ToolUseID   string          `json:"toolUseId,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Suggestions []string        `json:"suggestions,omitempty"`
	At          string          `json:"at,omitempty"`
}

// errDaemonDown reports that the daemon could not be reached.
var errDaemonDown = fmt.Errorf("the %s is not running", serviceName)

// fetchPendingPrompts asks the daemon for its jobs' open prompts, oldest
// first.
func fetchPendingPrompts(configDir string) ([]pendingPromptEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep := bgControlClient(configDir).Request(ctx, daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpList})
	if !rep.OK {
		if rep.Code == daemon.CodeNoConn {
			return nil, errDaemonDown
		}
		return nil, fmt.Errorf("couldn't list background sessions (%s)", dispatchFailureText(rep))
	}
	var recs []daemon.JobRecord
	if len(rep.Jobs) > 0 {
		if err := json.Unmarshal(rep.Jobs, &recs); err != nil {
			return nil, fmt.Errorf("couldn't read the %s's job list: %v", serviceName, err)
		}
	}
	out := []pendingPromptEntry{}
	for _, r := range recs {
		p := r.Permission
		if p == nil || r.Outcome != "" {
			continue
		}
		kind := p.Kind
		if kind == "" {
			kind = daemon.PromptPermission
		}
		out = append(out, pendingPromptEntry{
			Job: r.Short, Name: strings.TrimSpace(firstNonEmpty(r.Name, r.Intent)), Kind: kind,
			RequestID: p.RequestID, ToolName: p.ToolName, Summary: pendingSummary(kind, p),
			Input: toolInputOrEmpty(p.Input), ToolUseID: p.ToolUseID, Reason: p.Reason,
			Suggestions: p.Suggestions, At: p.At,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// pendingQuestions decodes a question prompt's {"questions":[…]} input.
func pendingQuestions(p *daemon.PendingPermission) []interact.AskRequestQuestion {
	var in struct {
		Questions []interact.AskRequestQuestion `json:"questions"`
	}
	_ = json.Unmarshal(p.Input, &in)
	return in.Questions
}

// pendingPlan decodes a plan prompt's input.
type pendingPlanInput struct {
	Plan         string   `json:"plan"`
	PlanFilePath string   `json:"planFilePath"`
	Modes        []string `json:"modes"`
}

func pendingPlan(p *daemon.PendingPermission) pendingPlanInput {
	var in pendingPlanInput
	_ = json.Unmarshal(p.Input, &in)
	return in
}

// pendingSummary is a prompt's one-line description: the command, path or
// URL of a tool call (else its input), the first question and its options,
// or the plan's first line.
func pendingSummary(kind string, p *daemon.PendingPermission) string {
	switch kind {
	case daemon.PromptQuestion:
		qs := pendingQuestions(p)
		if len(qs) == 0 {
			return "answer question"
		}
		labels := make([]string, 0, len(qs[0].Options))
		for _, o := range qs[0].Options {
			labels = append(labels, o.Label)
		}
		s := qs[0].Question
		if len(labels) > 0 {
			s += " (" + strings.Join(labels, " · ") + ")"
		}
		if len(qs) > 1 {
			s += fmt.Sprintf(" +%d more", len(qs)-1)
		}
		return oneLine(s, 200)
	case daemon.PromptPlan:
		in := pendingPlan(p)
		for _, l := range strings.Split(in.Plan, "\n") {
			if l = strings.TrimSpace(strings.TrimLeft(l, "# ")); l != "" {
				return oneLine("plan: "+l, 200)
			}
		}
		return "exit plan mode"
	}
	var in map[string]any
	_ = json.Unmarshal(p.Input, &in)
	for _, k := range []string{"command", "file_path", "url", "path", "pattern"} {
		if v, ok := in[k].(string); ok && v != "" {
			return oneLine(v, 200)
		}
	}
	return oneLine(string(toolInputOrEmpty(p.Input)), 200)
}

// runAgentsPending is `gocode agents pending [--json]`.
func runAgentsPending(env *Env, args []string) int {
	o, err := scanOpts(args, map[string]string{"--json": "json"}, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.unknown) > 0 || len(o.pos) > 0 {
		bad := append(o.unknown, o.pos...)
		fmt.Fprintf(env.Stderr, "error: unexpected argument '%s'\nUsage: %s\n", bad[0], agentsPendingUsage)
		return 1
	}
	rows, err := fetchPendingPrompts(daemonConfigHome(env))
	if errors.Is(err, errDaemonDown) {
		// No daemon, no live session: nothing is waiting.
		rows, err = []pendingPromptEntry{}, nil
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if o.has("json") {
		data, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintf(env.Stdout, "%s\n", data)
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.Stdout, "No background session is waiting for an answer.")
		return 0
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		head := r.Job
		if r.Name != "" {
			head += " · " + oneLine(r.Name, 40)
		}
		fmt.Fprintf(&b, "%s  %s  %s\n", head, r.Kind, r.ToolName)
		fmt.Fprintf(&b, "  %s\n", r.Summary)
		if r.Reason != "" {
			fmt.Fprintf(&b, "  reason: %s\n", oneLine(r.Reason, 200))
		}
		if len(r.Suggestions) > 0 {
			fmt.Fprintf(&b, "  --always adds: %s\n", r.Suggestions[0])
		}
		fmt.Fprintf(&b, "  request: %s\n", r.RequestID)
	}
	b.WriteString("\n  gocode agents respond <job> allow|deny     answer a permission prompt or plan approval\n")
	b.WriteString("  gocode agents respond <job> --answer <label>  answer a question\n")
	fmt.Fprint(env.Stdout, b.String())
	return 0
}

// respondOpts are the parsed `agents respond` arguments.
type respondOpts struct {
	job       string
	verdict   string // "allow", "deny" or "" (implied by --answer)
	message   string
	always    bool
	mode      string
	answers   []string
	requestID string
}

// parseRespondArgs reads `<job> [allow|deny] [--message <text>] [--always]
// [--mode <mode>] [--answer <label>]… [--request-id <id>]`.
func parseRespondArgs(args []string) (respondOpts, error) {
	var o respondOpts
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, val, inline := strings.Cut(a, "=")
		value := func() (string, error) {
			if inline {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("error: option '%s' argument missing", key)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case key == "--message" || key == "-m":
			o.message, err = value()
		case key == "--mode":
			o.mode, err = value()
		case key == "--answer":
			var v string
			v, err = value()
			o.answers = append(o.answers, v)
		case key == "--request-id":
			o.requestID, err = value()
		case a == "--always":
			o.always = true
		case isOptionToken(a):
			return o, fmt.Errorf("error: unknown option '%s'", a)
		default:
			pos = append(pos, a)
		}
		if err != nil {
			return o, err
		}
	}
	if len(pos) == 0 {
		return o, fmt.Errorf("error: missing <job>")
	}
	o.job = pos[0]
	if len(pos) > 1 {
		o.verdict = strings.ToLower(pos[1])
		if o.verdict != "allow" && o.verdict != "deny" {
			return o, fmt.Errorf("error: the answer is 'allow' or 'deny', not '%s'", pos[1])
		}
	}
	if len(pos) > 2 {
		return o, fmt.Errorf("error: unexpected argument '%s'", pos[2])
	}
	if o.verdict == "" {
		if len(o.answers) == 0 {
			return o, fmt.Errorf("error: say allow or deny (or --answer <label> for a question)")
		}
		o.verdict = "allow"
	}
	if o.verdict == "deny" && (o.always || o.mode != "" || len(o.answers) > 0) {
		return o, fmt.Errorf("error: --always, --mode and --answer go with allow")
	}
	return o, nil
}

// respondDecision is the Agent SDK PermissionResult the op carries, with the
// fields a decision does not set left out (a null updatedInput would be
// read as an input that is not an object).
type respondDecision struct {
	Behavior           string                      `json:"behavior"`
	UpdatedInput       json.RawMessage             `json:"updatedInput,omitempty"`
	UpdatedPermissions []headless.PermissionUpdate `json:"updatedPermissions,omitempty"`
	Message            string                      `json:"message,omitempty"`
}

// buildRespondDecision turns the arguments into the decision the prompt of
// kind expects.
func buildRespondDecision(kind string, p *daemon.PendingPermission, o respondOpts) (respondDecision, error) {
	if o.verdict == "deny" {
		return respondDecision{Behavior: "deny", Message: o.message}, nil
	}
	res := respondDecision{Behavior: "allow"}
	if o.message != "" {
		return res, fmt.Errorf("--message goes with deny (it is the feedback the session is given)")
	}
	switch kind {
	case daemon.PromptQuestion:
		if o.always || o.mode != "" {
			return res, fmt.Errorf("--always and --mode do not apply to a question")
		}
		answers, err := questionAnswers(pendingQuestions(p), o.answers)
		if err != nil {
			return res, err
		}
		res.UpdatedInput, _ = json.Marshal(map[string]any{"answers": answers})
	case daemon.PromptPlan:
		if o.always || len(o.answers) > 0 {
			return res, fmt.Errorf("--always and --answer do not apply to a plan approval")
		}
		if o.mode != "" {
			modes := pendingPlan(p).Modes
			if !containsString(modes, o.mode) {
				return res, fmt.Errorf("--mode %s is not offered; the plan can continue in: %s", o.mode, strings.Join(modes, ", "))
			}
			res.UpdatedPermissions = []headless.PermissionUpdate{{Type: "setMode", Mode: o.mode, Destination: "session"}}
		}
	default:
		if len(o.answers) > 0 || o.mode != "" {
			return res, fmt.Errorf("--answer and --mode do not apply to a permission prompt")
		}
		if o.always {
			if len(p.Suggestions) == 0 {
				return res, fmt.Errorf("this prompt offers no rule to always allow; answer allow without --always")
			}
			rule := headless.PermissionRule{ToolName: p.Suggestions[0]}
			if name, content, ok := strings.Cut(p.Suggestions[0], "("); ok && strings.HasSuffix(content, ")") {
				rule = headless.PermissionRule{ToolName: name, RuleContent: strings.TrimSuffix(content, ")")}
			}
			res.UpdatedPermissions = []headless.PermissionUpdate{{Type: "addRules", Rules: []headless.PermissionRule{rule}, Behavior: "allow", Destination: "session"}}
		}
	}
	return res, nil
}

// questionAnswers maps --answer values onto the questions in order: each
// names one of its question's option labels (case-insensitively), several
// comma-separated for a multi-select question. A question with no options
// takes the text as given.
func questionAnswers(qs []interact.AskRequestQuestion, given []string) (map[string]any, error) {
	if len(qs) == 0 {
		return nil, fmt.Errorf("the question prompt lists no questions")
	}
	if len(given) != len(qs) {
		var b strings.Builder
		fmt.Fprintf(&b, "give one --answer per question (%d %s, %d %s):", len(qs), pluralize(len(qs), "question", "questions"), len(given), pluralize(len(given), "answer", "answers"))
		for _, q := range qs {
			fmt.Fprintf(&b, "\n  %s", q.Question)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
	out := make(map[string]any, len(qs))
	for i, q := range qs {
		if len(q.Options) == 0 {
			out[q.Question] = given[i]
			continue
		}
		match := func(v string) (string, bool) {
			for _, opt := range q.Options {
				if strings.EqualFold(strings.TrimSpace(v), opt.Label) {
					return opt.Label, true
				}
			}
			return "", false
		}
		if l, ok := match(given[i]); ok {
			if q.MultiSelect {
				out[q.Question] = []string{l}
			} else {
				out[q.Question] = l
			}
			continue
		}
		if q.MultiSelect {
			var labels []string
			for _, part := range strings.Split(given[i], ",") {
				l, ok := match(part)
				if !ok {
					labels = nil
					break
				}
				labels = append(labels, l)
			}
			if len(labels) > 0 {
				out[q.Question] = labels
				continue
			}
		}
		labels := make([]string, 0, len(q.Options))
		for _, opt := range q.Options {
			labels = append(labels, opt.Label)
		}
		return nil, fmt.Errorf("%q is not an option of %q; the options are: %s", given[i], q.Question, strings.Join(labels, ", "))
	}
	return out, nil
}

// runAgentsRespond is `gocode agents respond <job> …` (its --help is the
// generic subcommand help, commands.go).
func runAgentsRespond(env *Env, args []string) int {
	o, err := parseRespondArgs(args)
	if err != nil {
		fmt.Fprintf(env.Stderr, "%v\nUsage: %s\n", err, agentsRespondUsage)
		return 1
	}
	configDir := daemonConfigHome(env)
	r := resolveJob(env, configDir, o.job, nil, agentsRespondUsage, "Answer a background session's open prompt.", true)
	if r.code != 0 {
		return max(r.code, 0)
	}
	rows, err := fetchPendingPrompts(configDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	var row *pendingPromptEntry
	for i := range rows {
		if rows[i].Job == r.short {
			row = &rows[i]
		}
	}
	if row == nil {
		fmt.Fprintf(env.Stderr, "%s is not waiting for an answer (see `gocode agents pending`)\n", r.short)
		return 1
	}
	if o.requestID != "" && o.requestID != row.RequestID {
		fmt.Fprintf(env.Stderr, "%s's open prompt is now %s, not %s — check it with `gocode agents pending`\n", r.short, row.RequestID, o.requestID)
		return 1
	}
	p := &daemon.PendingPermission{RequestID: row.RequestID, Kind: row.Kind, ToolName: row.ToolName, Input: row.Input, Suggestions: row.Suggestions}
	decision, err := buildRespondDecision(row.Kind, p, o)
	if err != nil {
		fmt.Fprintf(env.Stderr, "error: %v\n", err)
		return 1
	}
	raw, _ := json.Marshal(decision)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep := bgControlClient(configDir).Request(ctx, daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpPermissionResponse,
		Auth: daemon.ReadControlKey(configDir), Short: r.short, RequestID: row.RequestID, Decision: raw})
	if !rep.OK {
		fmt.Fprintf(env.Stderr, "couldn't answer %s: %s\n", r.short, respondFailureText(rep))
		return 1
	}
	fmt.Fprintf(env.Stdout, "%s: %s\n", r.short, respondDoneText(row, decision))
	return 0
}

// respondFailureText explains a refused permission-response.
func respondFailureText(rep daemon.Reply) string {
	switch rep.Code {
	case errutil.WorkerENoReply:
		return "the prompt is no longer open (it was answered in the terminal, or the turn moved on)"
	case errutil.WorkerEStale:
		return "the prompt was replaced by another before the answer arrived — check `gocode agents pending` and answer again"
	case errutil.WorkerEProto:
		return "the daemon did not accept the decision (" + rep.Error + ")"
	case errutil.WorkerENoJob:
		return "the session is not running"
	case errutil.WorkerEAuth:
		return "the " + serviceName + " refused this client's control key (" + rep.Error + ")"
	case daemon.CodeNoConn:
		return "the " + serviceName + " is not running"
	}
	return dispatchFailureText(rep)
}

// respondDoneText reports what was answered.
func respondDoneText(row *pendingPromptEntry, d respondDecision) string {
	switch {
	case row.Kind == daemon.PromptQuestion && d.Behavior == "allow":
		return "answered the question"
	case row.Kind == daemon.PromptQuestion:
		return "dismissed the question"
	case row.Kind == daemon.PromptPlan && d.Behavior == "allow":
		return "approved the plan"
	case row.Kind == daemon.PromptPlan:
		return "kept planning"
	case d.Behavior == "allow" && len(d.UpdatedPermissions) > 0:
		return "allowed " + row.ToolName + " and added " + row.Suggestions[0] + " for this session"
	case d.Behavior == "allow":
		return "allowed " + row.ToolName
	}
	return "denied " + row.ToolName
}
