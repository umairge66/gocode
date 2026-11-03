package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/errutil"
	"github.com/umairge66/gocode/internal/proctree"
	"github.com/umairge66/gocode/internal/provider"
)

// -----------------------------------------------------------------------------
// Background sessions, CLI side: `gocode --bg [prompt]` / `--background`,
// `--bg --exec <command>`, and the verbs `logs`, `attach`, `stop` (alias
// `kill`), `respawn` and `rm`. Listing is `gocode agents --json` (bgagents.go).
//
// Only the LOCAL daemon backend exists. Cloud / remote background sessions
// (the "remote" backend, cloud tabs, the bridge and remote-control) are hosted
// gocode services and are not supported; nothing here talks to a server.

// bgVerbs are the background-session verbs.
var bgVerbs = map[string]bool{"attach": true, "logs": true, "stop": true, "kill": true, "respawn": true, "rm": true}

// bgValueFlags are root options that consume the next argument (used to find
// the first operand).
var bgValueFlags = map[string]bool{
	"--settings": true, "--managed-settings": true, "--setting-sources": true, "--add-dir": true,
	"--plugin-dir": true, "--plugin-dir-no-mcp": true, "--mcp-config": true, "--cwd": true,
	"--debug-file": true, "--model": true, "--permission-mode": true, "--effort": true, "--agent": true,
	"--name": true, "-n": true, "--session-id": true, "--resume": true, "-r": true, "--exec": true,
	"--append-system-prompt": true, "--system-prompt": true, "--allowed-tools": true, "--allowedTools": true,
	"--disallowed-tools": true, "--disallowedTools": true, "--fallback-model": true, "--max-turns": true, "--max-budget-usd": true,
	"--output-format": true, "--input-format": true,
	"--system-prompt-file": true, "--append-system-prompt-file": true, "--agents": true,
}

// bgRootFlagsBeforeVerb are the root options allowed in front of a verb.
var bgRootFlagsBeforeVerb = map[string]bool{
	"--settings": true, "--managed-settings": true, "--setting-sources": true, "--add-dir": true,
	"--plugin-dir": true, "--plugin-dir-no-mcp": true, "--mcp-config": true, "--debug-file": true,
	"--verbose": true, "-d2e": true, "--debug-to-stderr": true, "--restricted": true, "--strict-mcp-config": true,
	"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true, "--debug": true, "-d": true,
}

func isOptionToken(s string) bool { return len(s) > 1 && strings.HasPrefix(s, "-") }

// firstOperandIndex returns the index of the first non-option argument.
func firstOperandIndex(t []string) int {
	for i := 0; i < len(t); i++ {
		a := t[i]
		if a == "--" {
			if i+1 < len(t) {
				return i + 1
			}
			return -1
		}
		if !isOptionToken(a) {
			return i
		}
		if !strings.Contains(a, "=") && bgValueFlags[a] {
			i++
		}
	}
	return -1
}

// backgroundVerbIndex returns the index of a background-session verb, or -1.
// The verb must be the first operand; behind root options it must be followed
// by an 8-hex id (or `respawn --all`) and every option before it must be an
// honoured root option.
func backgroundVerbIndex(t []string, behindRootOptions bool) int {
	i := firstOperandIndex(t)
	if i == -1 || !bgVerbs[t[i]] {
		return -1
	}
	if i == 0 {
		return 0
	}
	if !behindRootOptions || t[i-1] == "--" {
		return -1
	}
	if i+1 >= len(t) {
		return -1
	}
	next := t[i+1]
	if !(daemon.ShortIDPattern.MatchString(next) || (t[i] == "respawn" && next == "--all")) {
		return -1
	}
	for j := 0; j < i; j++ {
		a := t[j]
		key := a
		if k, _, ok := strings.Cut(a, "="); ok {
			key = k
		}
		if !bgRootFlagsBeforeVerb[key] {
			return -1
		}
		if !strings.Contains(a, "=") && bgValueFlags[a] {
			j++
		}
	}
	return i
}

// hasBackgroundFlag reports --bg / --background before any "--".
func hasBackgroundFlag(t []string) bool {
	for _, a := range t {
		if a == "--" {
			return false
		}
		if a == "--bg" || a == "--background" {
			return true
		}
	}
	return false
}

// runBackgroundVerb dispatches a background-session verb at t[idx].
func runBackgroundVerb(env *Env, t []string, idx int) int {
	verb := t[idx]
	rest := t[idx+1:]
	arg := ""
	if len(rest) > 0 {
		arg = rest[0]
	}
	configDir := daemonConfigHome(env)
	switch verb {
	case "logs":
		return bgLogs(env, configDir, arg, rest)
	case "attach":
		return bgAttach(env, configDir, arg, rest)
	case "respawn":
		return bgRespawn(env, configDir, arg, rest)
	case "rm":
		return bgRm(env, configDir, rest)
	default: // stop, kill
		return bgStop(env, configDir, arg, rest)
	}
}

// ---------------------------------------------------------------------------
// --bg / --background
// ---------------------------------------------------------------------------

// stripBgFlags removes --bg/--background before "--".
func stripBgFlags(t []string) []string {
	out := make([]string, 0, len(t))
	for i, a := range t {
		if a == "--" {
			return append(out, t[i:]...)
		}
		if a == "--bg" || a == "--background" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// bgParsed is the slice of the argv the background launcher needs.
type bgParsed struct {
	name      string
	agent     string
	resume    string
	sessionID bool
	prompt    string
	exec      string
	hasExec   bool
	flags     []string // forwarded to the worker (respawnFlags)
}

// parseBgArgs splits the (already --bg-stripped) argv.
func parseBgArgs(t []string) bgParsed {
	var p bgParsed
	var words []string
	for i := 0; i < len(t); i++ {
		a := t[i]
		if a == "--" {
			words = append(words, strings.Join(t[i+1:], " "))
			break
		}
		key, val, hasEq := strings.Cut(a, "=")
		take := func() string {
			if hasEq {
				return val
			}
			if i+1 < len(t) {
				i++
				return t[i]
			}
			return ""
		}
		switch {
		case key == "--exec":
			p.hasExec = true
			if hasEq {
				p.exec = val
			} else {
				p.exec = strings.Join(t[i+1:], " ")
				i = len(t)
			}
		case key == "--name" || key == "-n":
			p.name = take()
		case strings.HasPrefix(a, "-n") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			p.name = a[2:]
		case key == "--session-id":
			p.sessionID = true
			take()
		case key == "--resume" || key == "-r":
			p.resume = take()
		case key == "--agent":
			p.agent = take()
			p.flags = append(p.flags, "--agent", p.agent)
		case isOptionToken(a):
			p.flags = append(p.flags, a)
			if !hasEq && bgValueFlags[a] && i+1 < len(t) {
				i++
				p.flags = append(p.flags, t[i])
			}
		default:
			words = append(words, a)
		}
	}
	p.prompt = strings.TrimSpace(strings.Join(words, " "))
	return p
}

// readPipedStdin reads piped input (up to 1 MiB) for the prompt.
func readPipedStdin(env *Env) string {
	if env.StdinTTY || env.Stdin == nil {
		return ""
	}
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(env.Stdin, (1<<20)+1))
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		b := r.b
		if len(b) > 1<<20 {
			fmt.Fprintf(env.Stderr, "warning: piped stdin exceeds %d bytes, truncated\n", 1<<20)
			b = b[:1<<20]
		}
		s := string(b)
		s = strings.TrimSuffix(s, "\n")
		s = strings.TrimSuffix(s, "\r")
		return s
	case <-time.After(3 * time.Second):
		return ""
	}
}

// formatBgHints renders the post-dispatch hint block.
func formatBgHints(short, idle, name string, color bool) string {
	dim := func(s string) string {
		if color {
			return "\x1b[2m" + s + "\x1b[22m"
		}
		return s
	}
	cyan := func(s string) string {
		if color {
			return "\x1b[36m" + s + "\x1b[39m"
		}
		return s
	}
	row := func(cmd, what string) string { return dim("  " + padRight(cmd, 26) + what) }
	head := "backgrounded · " + cyan(short)
	if name != "" {
		head += " · " + name
	}
	if idle != "" {
		head += dim(" " + idle)
	}
	return strings.Join([]string{
		head,
		row("gocode agents", "list sessions"),
		row("gocode attach "+short, "open in this terminal"),
		row("gocode logs "+short, "show recent output"),
		row("gocode stop "+short, "stop this session"),
	}, "\n")
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// handleBgFlag starts a background session from the argv.
func handleBgFlag(env *Env, t []string) int {
	stripped := stripBgFlags(t)
	if len(stripped) == len(t) {
		flag := "--bg"
		for _, a := range t {
			if a == "--bg" || a == "--background" {
				flag = a
				break
			}
		}
		fmt.Fprintf(env.Stderr, "Couldn't tell whether %s is a flag or text here, so nothing was started. To start a background session put it first: gocode --bg <prompt>\n", flag)
		return 1
	}
	p := parseBgArgs(stripped)
	configDir := daemonConfigHome(env)
	if p.hasExec {
		if strings.TrimSpace(p.exec) == "" {
			fmt.Fprintln(env.Stderr, "--exec requires a command.")
			return 1
		}
		if len(p.flags) > 0 {
			fmt.Fprintf(env.Stderr, "warning: --exec ignores %s (only --name composes)\n", strings.Join(p.flags, " "))
		}
		res := spawnBgSession(env, configDir, bgSpawnOptions{exec: p.exec, intent: p.exec, name: p.name})
		if !res.ok {
			fmt.Fprintln(env.Stderr, res.err)
			return 1
		}
		label := p.name
		if label == "" {
			label = p.exec
		}
		fmt.Fprintln(env.Stdout, formatBgHints(res.short, "", label, env.StdoutTTY))
		return 0
	}
	if piped := readPipedStdin(env); piped != "" {
		if p.prompt != "" {
			p.prompt = p.prompt + "\n" + piped
		} else {
			p.prompt = piped
		}
	}
	if w := bgEnvOnlyKeyWarning(env, p.flags); w != "" {
		fmt.Fprintln(env.Stderr, w)
	}
	if p.sessionID {
		fmt.Fprintln(env.Stderr, "warning: --bg manages the session id; ignoring --session-id (use --resume <id> to continue an existing session)")
	}
	res := spawnBgSession(env, configDir, bgSpawnOptions{prompt: p.prompt, intent: p.prompt, name: p.name,
		agent: p.agent, resume: p.resume, flags: p.flags})
	if !res.ok {
		fmt.Fprintln(env.Stderr, res.err)
		return 1
	}
	idle := ""
	if res.idle {
		idle = daemon.DetailIdleNoMsg
	}
	fmt.Fprintln(env.Stdout, formatBgHints(res.short, idle, p.name, env.StdoutTTY))
	return 0
}

type bgSpawnOptions struct {
	prompt string
	intent string
	name   string
	agent  string
	resume string
	exec   string
	flags  []string
	// short/sessionID reuse an existing job (wake/respawn).
	short     string
	sessionID string
	source    string
	cwd       string
	launch    *daemon.Launch
}

type bgSpawnResult struct {
	ok    bool
	short string
	idle  bool
	err   string
}

// bgDispatchEnvNames are the caller's variables a dispatched session
// inherits from the dispatching CLI: which provider / model to use and the
// provider endpoints — never a credential.
//
// A key is never sent over the daemon socket. The job resolves its key the
// normal way — auth.json (written by /login) or the environment the daemon was
// started with — and fails fast with a clear message when it has none
// (bgnokey.go); `gocode --bg` warns up front when the caller's key exists only
// in its own environment.
var bgDispatchEnvNames = []string{"GOCODE_PROVIDER", "GOCODE_MODEL", "OPENROUTER_BASE_URL", "OPENAI_BASE_URL"}

// bgDispatchEnvVarNames is bgDispatchEnvNames plus the base-URL override
// variables of the registry's other providers (GROQ_BASE_URL, XAI_BASE_URL,
// …). The Anthropic endpoint variables keep their historical exclusion.
func bgDispatchEnvVarNames() []string {
	out := append([]string{}, bgDispatchEnvNames...)
	seen := map[string]bool{}
	for _, n := range out {
		seen[n] = true
	}
	for _, n := range provider.AllBaseURLEnvVars() {
		if seen[n] || n == provider.EnvAnthropicBaseURL {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// bgDispatchEnv is the env map sent with a dispatch: bgDispatchEnvNames and
// PATH from the caller, with any secret or provider-key name dropped
// (defence in depth: no credential is ever put in a daemon request).
func bgDispatchEnv(getenv func(string) string) map[string]string {
	out := map[string]string{}
	for _, k := range append(bgDispatchEnvVarNames(), "PATH") {
		if envsec.IsSecretEnvName(k) || envsec.IsProviderKeyEnvName(k) {
			continue
		}
		if v := getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// spawnBgSession seeds the job state and dispatches it to the daemon.
func spawnBgSession(env *Env, configDir string, o bgSpawnOptions) bgSpawnResult {
	cwd := o.cwd
	if cwd == "" {
		cwd = cwdOrEmpty(env)
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	sessionID := o.sessionID
	if sessionID == "" {
		sessionID = newUUID()
	}
	short := o.short
	if short == "" {
		short = sessionID[:8]
	}
	freshDir := o.short == ""
	jobDir := daemon.JobDir(configDir, short)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return bgSpawnResult{err: "Couldn't start the session — " + err.Error()}
	}
	idle := o.exec == "" && o.prompt == "" && o.launch == nil && o.resume == ""
	if freshDir || o.launch != nil {
		template := "bg"
		if o.exec != "" {
			template = "exec"
		} else if o.agent != "" {
			template = o.agent
		}
		sp := daemon.SeedParams{Template: template, RespawnFlags: o.flags, Intent: o.intent, Name: o.name,
			SessionID: sessionID, Cwd: cwd}
		if o.name != "" {
			sp.NameSource = "user"
		}
		if idle {
			d := daemon.DetailIdleNoMsg
			sp.Detail, sp.Tempo, sp.Needs = &d, "blocked", daemon.NeedsPrompt
		}
		st := daemon.NewJobState(sp)
		if prev, _ := daemon.ReadJobState(jobDir); prev != nil && !freshDir {
			st.CreatedAt = prev.CreatedAt
			st.Extra = prev.Extra
		}
		if err := daemon.WriteJobState(jobDir, st); err != nil {
			return bgSpawnResult{err: "Couldn't start the session — " + err.Error()}
		}
	}
	d := daemon.Dispatch{
		Proto: daemon.ProtoVersion, Short: short, SessionID: sessionID, CreatedAt: time.Now().UnixMilli(),
		Source: daemon.SourceShell, Cwd: cwd, RespawnFlags: append([]string{}, o.flags...),
		Agent: o.agent, Seed: &daemon.Seed{Intent: o.intent, Name: o.name}, Env: bgDispatchEnv(env.Getenv),
	}
	if o.source != "" {
		d.Source = o.source
	}
	if cols, rows, ok := terminalSize(); ok {
		d.Cols, d.Rows = cols, rows
	}
	switch {
	case o.launch != nil:
		d.Launch = *o.launch
	case o.exec != "":
		shell, args := daemon.ExecShell(env.Getenv, o.exec)
		d.Launch = daemon.Launch{Mode: daemon.LaunchExec, Cmd: shell, Args: args}
		d.RespawnFlags = []string{}
	case o.resume != "":
		d.Launch = daemon.Launch{Mode: daemon.LaunchResume, SessionID: o.resume, Fork: true, FlagArgs: o.flags}
	default:
		args := append([]string{"--session-id", sessionID}, o.flags...)
		if o.prompt != "" {
			args = append(args, o.prompt)
		}
		d.Launch = daemon.Launch{Mode: daemon.LaunchPrompt, Args: args}
	}
	fail := func(msg string) bgSpawnResult {
		if freshDir {
			_ = os.RemoveAll(jobDir)
		}
		return bgSpawnResult{err: msg}
	}
	if ok, reason := ensureDaemon(env, configDir); !ok {
		return fail(fmt.Sprintf("Couldn't reach the %s (%s)", serviceName, reason))
	}
	rep := dispatchToDaemon(configDir, &d)
	if rep.OK {
		return bgSpawnResult{ok: true, short: short, idle: idle}
	}
	switch rep.Code {
	case errutil.WorkerECwdGone:
		return fail(fmt.Sprintf("Couldn't start a background session (%s)", rep.Error))
	case errutil.WorkerEStale:
		return fail("Previous session is still shutting down — try again in a moment")
	}
	return fail(fmt.Sprintf("Couldn't reach the %s (%s)", serviceName, dispatchFailureText(rep)))
}

// dispatchFailureText maps a dispatch failure to the short reason.
func dispatchFailureText(rep daemon.Reply) string {
	switch rep.Code {
	case errutil.WorkerETimeout:
		return "timed out"
	case daemon.CodeNoConn:
		return "socket missing"
	case errutil.WorkerEStarting:
		return "service still starting"
	case errutil.WorkerEAuth:
		return rep.Error
	}
	if rep.Error != "" {
		return rep.Error
	}
	return string(rep.Code)
}

// dispatchToDaemon sends the authed dispatch op, retrying while the daemon is
// starting and on a stale id.
func dispatchToDaemon(configDir string, d *daemon.Dispatch) daemon.Reply {
	client := daemon.NewControlClient(configDir)
	client.Timeout = 6 * time.Second
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	d.Nonce = hex.EncodeToString(nonce)
	payload, _ := json.Marshal(d)
	var rep daemon.Reply
	starting := 40
	for attempt := 0; attempt < 3; attempt++ {
		req := daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpDispatch, Dispatch: payload,
			TimeoutMs: 5000, Auth: daemon.ReadControlKey(configDir)}
		rep = client.Request(context.Background(), req)
		for !rep.OK && rep.Code == errutil.WorkerEStarting && starting > 0 {
			starting--
			time.Sleep(200 * time.Millisecond)
			rep = client.Request(context.Background(), req)
		}
		if rep.OK || rep.Code != errutil.WorkerEStale {
			return rep
		}
		time.Sleep(500 * time.Millisecond)
	}
	return rep
}

// ---------------------------------------------------------------------------
// id / name resolution
// ---------------------------------------------------------------------------

var hexPrefixRE = regexp.MustCompile(`^[a-f0-9]{1,8}$`)

// bgUsageText is the help shared by the verbs that accept a name.
const bgNameHelp = "Part of the session name works in place of the id. Put a name with spaces in quotes."

type resolved struct {
	short     string
	byName    bool
	nameLines string
	code      int // non-zero: exit with this code
}

// resolveJob turns an id prefix (or, with byName, part of a session name) into
// a job id, printing usage/ambiguity messages.
func resolveJob(env *Env, configDir, arg string, rest []string, usage, desc string, byName bool) resolved {
	extra := warnExtraArgs(env, rest)
	if arg == "--help" || arg == "-h" {
		fmt.Fprintf(env.Stdout, "Usage: %s\n\n  %s\n", usage, desc)
		return resolved{code: -1}
	}
	if strings.HasPrefix(arg, "-") {
		fmt.Fprintf(env.Stderr, "unknown option '%s'\nUsage: %s\n", arg, usage)
		return resolved{code: 1}
	}
	if arg == "" {
		fmt.Fprintf(env.Stderr, "Usage: %s\n", usage)
		return resolved{code: 1}
	}
	var matches []string
	for _, id := range daemon.ListJobIDs(configDir) {
		if strings.HasPrefix(id, arg) {
			matches = append(matches, id)
		}
	}
	if len(matches) == 1 {
		return resolved{short: matches[0]}
	}
	h := strings.ToLower(strings.TrimSpace(arg))
	tryName := byName && !hexPrefixRE.MatchString(h)
	if tryName && !extra {
		type ranked struct {
			id   string
			st   *daemon.JobState
			rank int
		}
		var hits []ranked
		for _, e := range daemon.ListJobs(configDir) {
			if r := nameRank(e.State, h); r != 0 {
				hits = append(hits, ranked{e.ID, e.State, r})
			}
		}
		strong := false
		var named []ranked
		for _, x := range hits {
			if x.rank >= 2 {
				strong = true
			}
			if x.rank != 1 || !daemon.IsTerminalState(x.st.State) {
				named = append(named, x)
			}
		}
		if len(named) == 1 && strong {
			lines := fmt.Sprintf("Session %s · %s\n", named[0].id, named[0].st.Name)
			fmt.Fprint(env.Stderr, lines)
			return resolved{short: named[0].id, byName: true, nameLines: lines}
		}
		if len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool {
				if (hits[i].rank == 1) != (hits[j].rank == 1) {
					return hits[j].rank == 1
				}
				return hits[i].id < hits[j].id
			})
			var b strings.Builder
			if strong {
				fmt.Fprintf(&b, "%d sessions match '%s':\n", len(hits), arg)
			} else {
				fmt.Fprintf(&b, "No session name matches '%s'.\nThe task text of %d %s has it:\n", arg, len(hits), bgPlural(len(hits), "session"))
			}
			for i, x := range hits {
				if i >= 20 {
					fmt.Fprintf(&b, "  … and %d more\n", len(hits)-20)
					break
				}
				label := x.st.Name
				if label == "" {
					label = x.st.Intent
				}
				suffix := ""
				if strong && x.rank == 1 {
					suffix = "  (task text)"
				}
				fmt.Fprintf(&b, "  %s  %s%s\n", x.id, oneLine(label, 32), suffix)
			}
			switch {
			case !strong:
				b.WriteString("Use the id, or a session name.\n")
			default:
				b.WriteString("Use more of the name, or the id.\n")
			}
			fmt.Fprint(env.Stderr, b.String())
			return resolved{code: 1}
		}
	}
	if len(matches) == 0 {
		hint := ""
		if byName && !hexPrefixRE.MatchString(h) {
			if extra {
				hint = "A name must be the only argument. Put a name with spaces in quotes.\n"
			}
		} else if byName && hexPrefixRE.MatchString(h) && !daemon.ShortIDPattern.MatchString(h) {
			hint = "Text that can be the start of an id is not read as a name. Use more of the name.\n"
		}
		fmt.Fprintf(env.Stderr, "No job matching '%s'. Run 'gocode agents' to list running sessions.\n%s", arg, hint)
		return resolved{code: 1}
	}
	fmt.Fprintf(env.Stderr, "Ambiguous prefix '%s', matches: %s\n", arg, strings.Join(matches, ", "))
	return resolved{code: 1}
}

// nameRank ranks a session against a lower-cased query: exact name 3, name
// contains 2, task text contains 1.
func nameRank(st *daemon.JobState, q string) int {
	if q == "" {
		return 0
	}
	n := strings.ToLower(st.Name)
	if n == q {
		return 3
	}
	if n != "" && strings.Contains(n, q) {
		return 2
	}
	if strings.Contains(strings.ToLower(st.DisplayIntent), q) || strings.Contains(strings.ToLower(st.Intent), q) {
		return 1
	}
	return 0
}

func bgPlural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// warnExtraArgs prints the "extra arguments ignored" warning for arguments
// after the id (debug flags excepted) and reports whether there were any.
func warnExtraArgs(env *Env, rest []string) bool {
	if len(rest) <= 1 {
		return false
	}
	var extra []string
	for i := 1; i < len(rest); i++ {
		a := rest[i]
		if a == "--debug" || a == "-d" || a == "--debug-to-stderr" || a == "-d2e" ||
			strings.HasPrefix(a, "--debug=") || strings.HasPrefix(a, "--debug-file=") {
			continue
		}
		if a == "--debug-file" && i+1 < len(rest) {
			i++
			continue
		}
		extra = append(extra, a)
	}
	if len(extra) == 0 {
		return false
	}
	fmt.Fprintf(env.Stderr, "warning: extra arguments ignored: %s\n", strings.Join(extra, " "))
	return true
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// subscribeTail reads the subscribe snapshot's stream tail.
func subscribeTail(configDir, short string, tail int) ([]string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := daemon.DialControl(ctx, configDir)
	if err != nil {
		return nil, err.Error()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := daemon.WriteFrame(conn, daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpSubscribe, Short: short, Tail: tail}); err != nil {
		return nil, err.Error()
	}
	br := bufio.NewReader(conn)
	for {
		frame, err := daemon.ReadFrame(br)
		if err != nil {
			if isTimeoutErr(err) {
				return nil, fmt.Sprintf("%s did not respond — it may be stalled", serviceName)
			}
			return nil, "control socket closed"
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(frame, &m) != nil {
			continue
		}
		if ok, has := m["ok"]; has && string(ok) == "false" {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(frame, &e)
			return nil, e.Error
		}
		var snap struct {
			Type       string   `json:"type"`
			StreamTail []string `json:"streamTail"`
		}
		if json.Unmarshal(frame, &snap) == nil && snap.Type == "snapshot" {
			return snap.StreamTail, ""
		}
	}
}

func isTimeoutErr(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// bgLogs prints a session's recent terminal output.
func bgLogs(env *Env, configDir, arg string, rest []string) int {
	r := resolveJob(env, configDir, arg, rest, "gocode logs <id|name>",
		"Print the background session's recent terminal output. "+bgNameHelp, true)
	if r.code != 0 {
		return max(r.code, 0)
	}
	tail, errText := subscribeTail(configDir, r.short, 500)
	if errText != "" {
		fmt.Fprintf(env.Stderr, "Couldn't read logs for %s — %s\n", r.short, redactPipeNonce(errText))
		return 1
	}
	replay, addressed := replayFilter(strings.Join(tail, ""))
	trailer := ""
	if env.StdoutTTY {
		trailer = "\x1b[0m"
		if addressed {
			_, rows, ok := terminalSize()
			if !ok {
				rows = 9999
			}
			trailer += fmt.Sprintf("\x1b[%d;1H\n", rows)
		}
	}
	fmt.Fprint(env.Stdout, replay+trailer)
	if r.nameLines != "" && env.StdoutTTY {
		sep := ""
		if !addressed {
			sep = "\n"
		}
		fmt.Fprint(env.Stderr, sep+"\x1b[J"+r.nameLines)
	}
	return 0
}

// redactPipeNonce hides the Windows pipe nonce in an error.
func redactPipeNonce(s string) string {
	return regexp.MustCompile(`cc-daemon-[0-9a-f]{16}`).ReplaceAllString(s, "cc-daemon-*")
}

// ---------------------------------------------------------------------------
// stop / kill
// ---------------------------------------------------------------------------

// killJob asks the daemon to kill a job and reports whether it is confirmed
// gone, falling back to the roster when the daemon is unreachable.
func killJob(configDir, short string, st *daemon.JobState) (bool, string) {
	client := daemon.NewControlClient(configDir)
	req := daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpKill, Short: short}
	rep := client.Request(context.Background(), req)
	for i := 0; !rep.OK && rep.Code == errutil.WorkerEStarting && i < 10; i++ {
		time.Sleep(200 * time.Millisecond)
		rep = client.Request(context.Background(), req)
	}
	if rep.OK || rep.Code == errutil.WorkerENoJob {
		return true, ""
	}
	if rep.Code == daemon.CodeNoConn || rep.Code == errutil.WorkerETimeout {
		if w, ok := daemon.ReadRoster(configDir)[short]; ok && w.PID > 1 {
			// Only the recorded process, and only ours (a reused pid is
			// another process).
			if proctree.Alive(w.PID) && proctree.OwnedBySelf(w.PID) &&
				(w.ProcStart == "" || proctree.SameProcess(w.PID, w.ProcStart)) {
				_ = proctree.SignalGraceful(w.PID)
				deadline := time.Now().Add(3 * time.Second)
				for proctree.Alive(w.PID) && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				if proctree.Alive(w.PID) {
					_ = proctree.KillGroup(w.PID)
					if w.ReplPID > 1 {
						_ = proctree.KillGroup(w.ReplPID)
					}
					time.Sleep(500 * time.Millisecond)
				}
			}
			return !proctree.Alive(w.PID), ""
		}
		return st != nil && st.IsSettled(), ""
	}
	return false, rep.Error
}

// bgStop stops a session and marks it stopped.
func bgStop(env *Env, configDir, arg string, rest []string) int {
	r := resolveJob(env, configDir, arg, rest, "gocode stop <id>",
		"Stop a background session. Its conversation is kept; resume it later with `gocode attach <id>`.", false)
	if r.code != 0 {
		return max(r.code, 0)
	}
	jobDir := daemon.JobDir(configDir, r.short)
	st, _ := daemon.ReadJobState(jobDir)
	confirmed, errText := killJob(configDir, r.short, st)
	if !confirmed {
		if errText != "" {
			fmt.Fprintf(env.Stderr, "couldn't confirm %s was stopped — %s\n", r.short, errText)
		} else {
			fmt.Fprintf(env.Stderr, "couldn't confirm %s was stopped — the %s may be restarting. Try again in a moment.\n", r.short, serviceName)
		}
		return 1
	}
	fmt.Fprintf(env.Stdout, "stopped %s\n", r.short)
	if cur, _ := daemon.ReadJobState(jobDir); cur != nil && !cur.IsSettled() {
		now := daemon.ISOTime(time.Now())
		cur.State, cur.Detail, cur.Tempo, cur.Needs, cur.InFlight, cur.UpdatedAt = "stopped", "stopped", "idle", "", nil, now
		if cur.FirstTerminalAt == nil {
			cur.FirstTerminalAt = &now
		}
		_ = daemon.WriteJobState(jobDir, cur)
		st = cur
	}
	if st != nil && st.WorktreePath != "" {
		fmt.Fprintf(env.Stdout, "  worktree retained at %s\n  run 'gocode rm %s' to remove worktree and job state\n", st.WorktreePath, r.short)
	}
	return 0
}

// ---------------------------------------------------------------------------
// rm
// ---------------------------------------------------------------------------

const bgRmUsage = "Usage: gocode rm <id> [--discard-unpushed <commit>@<worktree-id>] [--force-remove-worktree <worktree-id>]\n"

var (
	commitRE      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	worktreeIDRE  = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	bgRmLongUsage = bgRmUsage +
		"\n  Delete a background session and its worktree. Unlike `stop`, works on already-exited sessions.\n" +
		"  --discard-unpushed <commit>@<worktree-id>  also discard the worktree's unpushed commits (and any uncommitted changes) while it is still the same worktree at that commit — pass the value a previous 'gocode rm <id>' reported\n" +
		"  --force-remove-worktree <worktree-id>      delete the worktree directory even though the WorktreeRemove hook or git couldn't remove it (its repository must list it as a worktree, or have dropped it in a removal that couldn't delete it; no uncommitted changes to tracked files, submodules included; the branch is kept) — pass the value a previous 'gocode rm <id>' reported\n"
)

// bgRm deletes a session: stop it if it runs, then remove its job state.
// Background sessions never own a worktree, so the worktree
// options are validated but have nothing to act on.
func bgRm(env *Env, configDir string, rest []string) int {
	var id string
	var extra []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--help" || a == "-h":
			fmt.Fprint(env.Stdout, bgRmLongUsage)
			return 0
		case a == "--discard-unpushed" || strings.HasPrefix(a, "--discard-unpushed="):
			v := strings.TrimPrefix(a, "--discard-unpushed=")
			if a == "--discard-unpushed" {
				if i+1 < len(rest) {
					i++
					v = rest[i]
				} else {
					v = ""
				}
			}
			parts := strings.Split(strings.ToLower(v), "@")
			if len(parts) != 2 || !commitRE.MatchString(parts[0]) || !worktreeIDRE.MatchString(parts[1]) {
				fmt.Fprint(env.Stderr, "option '--discard-unpushed' takes the <commit>@<worktree-id> value shown by a previous 'gocode rm <id>' refusal\n"+bgRmUsage)
				return 1
			}
		case a == "--force-remove-worktree" || strings.HasPrefix(a, "--force-remove-worktree="):
			v := strings.TrimPrefix(a, "--force-remove-worktree=")
			if a == "--force-remove-worktree" {
				if i+1 < len(rest) {
					i++
					v = rest[i]
				} else {
					v = ""
				}
			}
			if !worktreeIDRE.MatchString(strings.ToLower(v)) {
				fmt.Fprint(env.Stderr, "option '--force-remove-worktree' takes the <worktree-id> value shown by a previous 'gocode rm <id>' refusal\n"+bgRmUsage)
				return 1
			}
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(env.Stderr, "unknown option '%s'\n%s", a, bgRmUsage)
			return 1
		case id == "":
			id = a
		default:
			extra = append(extra, a)
		}
	}
	if id == "" {
		fmt.Fprint(env.Stderr, bgRmUsage)
		return 1
	}
	if len(extra) > 0 {
		fmt.Fprintf(env.Stderr, "warning: extra arguments ignored: %s\n", strings.Join(extra, " "))
	}
	var matches []string
	for _, j := range daemon.ListJobIDs(configDir) {
		if strings.HasPrefix(j, id) {
			matches = append(matches, j)
		}
	}
	if len(matches) != 1 {
		if len(matches) == 0 {
			fmt.Fprintf(env.Stderr, "No job matching '%s'\n", id)
		} else {
			fmt.Fprintf(env.Stderr, "Ambiguous prefix '%s', matches: %s\n", id, strings.Join(matches, ", "))
		}
		return 1
	}
	short := matches[0]
	jobDir := daemon.JobDir(configDir, short)
	st, _ := daemon.ReadJobState(jobDir)
	if st == nil || !st.IsSettled() || daemonHasJob(configDir, short) {
		confirmed, errText := killJob(configDir, short, st)
		if !confirmed {
			if errText == "" {
				errText = fmt.Sprintf("the %s may be restarting. Try again in a moment.", serviceName)
			}
			fmt.Fprintf(env.Stderr, "couldn't remove %s — %s\n", short, errText)
			return 1
		}
		waitJobGone(configDir, short, 8*time.Second)
	}
	if err := os.RemoveAll(jobDir); err != nil {
		fmt.Fprintf(env.Stderr, "couldn't remove %s — %v\n", short, err)
		return 1
	}
	out := "removed " + short
	if st != nil && st.WorktreePath != "" {
		out += "\n  worktree directory left at " + st.WorktreePath
	}
	fmt.Fprintln(env.Stdout, out)
	return 0
}

// daemonHasJob asks the daemon whether it still holds a handle for short.
func daemonHasJob(configDir, short string) bool {
	rep := daemon.NewControlClient(configDir).Request(context.Background(),
		daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpHas, Short: short})
	return rep.OK && rep.Present
}

func waitJobGone(configDir, short string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && daemonHasJob(configDir, short) {
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// respawn / wake
// ---------------------------------------------------------------------------

// wakeJob (re)starts a saved session under its own id: it resumes the
// conversation when a transcript exists, else replays the original launch.
// With force a live worker is stopped first.
func wakeJob(env *Env, configDir, short string, force bool) (bool, bool, string) {
	jobDir := daemon.JobDir(configDir, short)
	st, err := daemon.ReadJobState(jobDir)
	if err != nil || st == nil {
		return false, false, fmt.Sprintf("could not read the saved state of background session %s", short)
	}
	if st.IsExecOnly() {
		return false, false, fmt.Sprintf("session %s ran a shell command, which is not run again for you", short)
	}
	if daemonHasJob(configDir, short) {
		if !force {
			return false, true, ""
		}
		if ok, _ := killJob(configDir, short, st); !ok {
			return false, true, ""
		}
		waitJobGone(configDir, short, 8*time.Second)
		if daemonHasJob(configDir, short) {
			return false, true, ""
		}
	}
	if fi, err := os.Stat(st.Cwd); err != nil || !fi.IsDir() {
		return false, false, fmt.Sprintf("Couldn't start a background session (%s)", daemon.CwdGoneMessage(st.Cwd))
	}
	sessionID := st.ResumeSessionID
	if sessionID == "" {
		sessionID = st.SessionID
	}
	var launch daemon.Launch
	if transcriptExists(configDir)(st.Cwd, sessionID) {
		launch = daemon.Launch{Mode: daemon.LaunchResume, SessionID: sessionID, FlagArgs: st.RespawnFlags}
	} else {
		args := append([]string{"--session-id", st.SessionID}, st.RespawnFlags...)
		if st.Intent != "" {
			args = append(args, st.Intent)
		}
		launch = daemon.Launch{Mode: daemon.LaunchPrompt, Args: args}
	}
	st.State, st.Tempo, st.Detail, st.UpdatedAt = "working", "active", daemon.DetailStarting, daemon.ISOTime(time.Now())
	st.FirstTerminalAt, st.LastTerminalAt = nil, ""
	if err := daemon.WriteJobState(jobDir, st); err != nil {
		return false, false, err.Error()
	}
	if w := bgEnvOnlyKeyWarning(env, st.RespawnFlags); w != "" {
		fmt.Fprintln(env.Stderr, w)
	}
	res := spawnBgSession(env, configDir, bgSpawnOptions{short: short, sessionID: st.SessionID, source: daemon.SourceRespawn,
		cwd: st.Cwd, intent: st.Intent, name: st.Name, flags: st.RespawnFlags, launch: &launch})
	if !res.ok {
		return false, false, res.err
	}
	return true, false, ""
}

const bgRespawnUsage = "Usage: gocode respawn <id>|--all"

// bgRespawn restarts sessions so they pick up the current binary.
func bgRespawn(env *Env, configDir, arg string, rest []string) int {
	warnExtraArgs(env, rest)
	if arg == "--help" || arg == "-h" {
		fmt.Fprintf(env.Stdout, "%s\n\n  Restart a background session (or all of them) so it picks up the current gocode binary.\n", bgRespawnUsage)
		return 0
	}
	if strings.HasPrefix(arg, "-") && arg != "--all" {
		fmt.Fprintf(env.Stderr, "unknown option '%s'\n%s\n", arg, bgRespawnUsage)
		return 1
	}
	if arg == "" {
		fmt.Fprintln(env.Stderr, "usage: gocode respawn <id>|--all")
		return 1
	}
	if ok, reason := ensureDaemon(env, configDir); !ok {
		fmt.Fprintf(env.Stderr, "Couldn't respawn — %s is unavailable (%s)\n", serviceName, reason)
		return 1
	}
	if arg == "--all" {
		var live []daemon.JobEntry
		for _, e := range daemon.ListJobs(configDir) {
			if !daemon.IsTerminalState(e.State.State) {
				live = append(live, e)
			}
		}
		if len(live) == 0 {
			fmt.Fprintln(env.Stdout, "no live jobs to respawn")
			return 0
		}
		code := 0
		for _, e := range live {
			ok, alive, errText := wakeJob(env, configDir, e.ID, true)
			switch {
			case ok:
				fmt.Fprintf(env.Stdout, "respawned %s\n", e.ID)
			case alive:
				code = 1
				fmt.Fprintf(env.Stderr, "%s: still running — couldn't confirm restart, retry in a moment\n", e.ID)
			default:
				code = 1
				fmt.Fprintf(env.Stderr, "%s: %s\n", e.ID, errText)
			}
		}
		return code
	}
	var matches []string
	for _, j := range daemon.ListJobIDs(configDir) {
		if strings.HasPrefix(j, arg) {
			matches = append(matches, j)
		}
	}
	if len(matches) != 1 {
		if len(matches) == 0 {
			fmt.Fprintf(env.Stderr, "No job matching '%s'\n", arg)
		} else {
			fmt.Fprintf(env.Stderr, "Ambiguous prefix '%s', matches: %s\n", arg, strings.Join(matches, ", "))
		}
		return 1
	}
	short := matches[0]
	ok, alive, errText := wakeJob(env, configDir, short, true)
	if !ok && alive {
		fmt.Fprintf(env.Stderr, "%s: still running — couldn't confirm restart, retry in a moment\n", short)
		return 1
	}
	if !ok {
		fmt.Fprintln(env.Stderr, errText)
		return 1
	}
	fmt.Fprintf(env.Stdout, "respawned %s\n", short)
	return 0
}
