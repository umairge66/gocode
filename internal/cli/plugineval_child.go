package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/frontmatter"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/toolreg"
)

// The headless child of `gocode plugin eval`.
//
// Each eval run is a separate gocode process started as
//
//	gocode -p --output-format stream-json --verbose --max-turns N
//	       --permission-mode dontAsk --setting-sources user [--model=M]
//	       [--plugin-dir P | --plugin-dir-no-mcp P]...
//	       --allowed-tools=<rules> --disallowed-tools=<rules>
//	       [--append-system-prompt=<text>]
//
// with the prompt on stdin, inside the run's sandbox (HOME, GOCODE_CONFIG_DIR,
// TMPDIR all point into it) and GOCODE_CODE_EVAL_CONFINED=1. It runs one turn
// and writes the stream-json records the harness grades: the assistant
// messages, the user messages carrying tool results, and a final result record
// with num_turns and total_cost_usd.

// evalChildMarker is the environment flag the harness sets.
const evalChildMarker = "GOCODE_CODE_EVAL_CONFINED"

// isEvalChildInvocation reports whether this `-p` run is an eval child.
func isEvalChildInvocation(env *Env, rest []string) bool {
	if env == nil || env.Getenv == nil || env.Getenv(evalChildMarker) != "1" {
		return false
	}
	v, _ := evalChildFlag(rest, "--output-format")
	return v == "stream-json"
}

// evalChildFlag returns a flag's value in either "--flag=v" or "--flag v"
// form.
func evalChildFlag(tokens []string, name string) (string, bool) {
	for i, t := range tokens {
		if strings.HasPrefix(t, name+"=") {
			return t[len(name)+1:], true
		}
		if t == name && i+1 < len(tokens) {
			return tokens[i+1], true
		}
	}
	return "", false
}

// evalChildOptions are the child flags runPrint does not know.
type evalChildOptions struct {
	maxTurns     string
	allowed      []string
	disallowed   []string
	appendSystem string
}

func parseEvalChildOptions(rest []string) evalChildOptions {
	var o evalChildOptions
	o.maxTurns, _ = evalChildFlag(rest, "--max-turns")
	if v, ok := evalChildFlag(rest, "--allowed-tools"); ok {
		o.allowed = frontmatter.SplitTools([]string{v})
	}
	if v, ok := evalChildFlag(rest, "--disallowed-tools"); ok {
		o.disallowed = frontmatter.SplitTools([]string{v})
	}
	o.appendSystem, _ = evalChildFlag(rest, "--append-system-prompt")
	return o
}

// streamWriter emits one JSON record per line.
type streamWriter struct{ w io.Writer }

func (s streamWriter) emit(v any) {
	b, err := headless.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "%s\n", b)
}

// runEvalChild runs the eval child: one headless turn, stream-json out.
func runEvalChild(env *Env, d *DispatchDefaults, rest []string) int {
	o := parseEvalChildOptions(rest)
	out := streamWriter{w: env.Stdout}
	started := time.Now()
	fail := func(msg string, status int) int {
		rec := map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"duration_ms": time.Since(started).Milliseconds(), "num_turns": 0, "result": msg,
			"total_cost_usd": 0, "permission_denials": []any{}}
		if status > 0 {
			rec["api_error_status"] = status
		}
		out.emit(rec)
		return 1
	}
	var prompt string
	if env.Stdin != nil {
		b, err := io.ReadAll(env.Stdin)
		if err == nil {
			prompt = strings.TrimSpace(string(b))
		}
	}
	if prompt == "" {
		return fail("no prompt provided on stdin", 0)
	}
	childEnv := evalChildEnv(env, o)
	return runEvalChildSession(childEnv, newPrintSession(childEnv, d), o, prompt, out, started, fail)
}

// evalChildEnv overlays --max-turns as GOCODE_CODE_MAX_TURNS (the session's
// turn bound).
func evalChildEnv(env *Env, o evalChildOptions) *Env {
	childEnv := *env
	if o.maxTurns != "" {
		parent := env.Getenv
		childEnv.Getenv = func(k string) string {
			if k == "GOCODE_CODE_MAX_TURNS" {
				return o.maxTurns
			}
			return parent(k)
		}
	}
	return &childEnv
}

// runEvalChildSession runs the turn on a prepared print session.
func runEvalChildSession(childEnv *Env, s *printSession, o evalChildOptions, prompt string, out streamWriter, started time.Time, fail func(string, int) int) int {
	env := childEnv
	caller, err := s.buildCaller()
	if err != nil {
		return fail(err.Error(), 0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rt := buildSession(ctx, childEnv, s, caller, sessionConfig{noPersistence: true})
	defer rt.Close("other")
	conv := rt.conv
	for _, r := range o.disallowed {
		if rule, ok := parsePermissionRule(r, toolreg.RuleDeny, "cliArg"); ok {
			rt.ruleStore.Add(rule)
		}
	}
	// Under allowManagedPermissionRulesOnly the allow rules are ignored; the
	// deny rules above still apply.
	if !rt.dispatcher.ManagedRulesOnly {
		for _, r := range o.allowed {
			if rule, ok := parsePermissionRule(r, toolreg.RuleAllow, "cliArg"); ok {
				rt.ruleStore.Add(rule)
			}
		}
	} else if len(o.allowed) > 0 {
		fmt.Fprintln(childEnv.Stderr, managedOnlyAllowedToolsNotice(o.allowed))
	}
	// A bare disallowed tool is withheld from the model entirely, not only
	// refused when called (--disallowed-tools removes it from the tool list).
	removed := map[string]bool{}
	for _, r := range o.disallowed {
		if !strings.Contains(r, "(") {
			removed[r] = true
		}
	}
	var schemas []agent.ToolSchema
	var names []string
	for _, ts := range conv.ToolSchemas {
		if removed[ts.Name] {
			continue
		}
		schemas = append(schemas, ts)
		names = append(names, ts.Name)
	}
	conv.ToolSchemas = schemas
	if o.appendSystem != "" {
		conv.System = strings.TrimRight(conv.System, "\n") + "\n\n" + o.appendSystem
	}
	for _, n := range rt.start(ctx) {
		fmt.Fprintln(env.Stderr, "gocode -p: "+n)
	}
	cwd, _ := os.Getwd()
	init := headless.NewSystemInit(rt.currentSessionID(), cwd, s.Model, string(s.Mode))
	if names != nil {
		init.Tools = names
	}
	out.emit(init)
	turns := 0
	conv.OnMessage = func(ev agent.MessageEvent) {
		switch ev.Message.Role {
		case agent.RoleAssistant:
			turns++
			out.emit(headless.NewAssistantMessage(rt.currentSessionID(), s.Model, "", ev.Message, ev.StopReason, ev.Usage))
		case agent.RoleUser:
			if headless.HasToolResult(ev.Message) {
				out.emit(headless.NewUserMessage(rt.currentSessionID(), ev.Message))
			}
		}
	}
	res, serr := conv.Submit(ctx, prompt)
	runErr := serr
	if runErr == nil {
		runErr = res.Err
	}
	costUSD := 0.0
	if conv.Cost != nil {
		costUSD = conv.Cost.TotalCostUSD()
	}
	rec := map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"duration_ms": time.Since(started).Milliseconds(), "num_turns": turns,
		"result": res.AssistantText, "total_cost_usd": costUSD, "permission_denials": []any{}}
	code := 0
	switch {
	case runErr != nil:
		rec["subtype"], rec["is_error"], rec["result"] = "error_during_execution", true, runErr.Error()
		var apiErr *provider.APIError
		if errors.As(runErr, &apiErr) && apiErr.Status > 0 {
			rec["api_error_status"] = apiErr.Status
		}
		code = 1
	case res.Reason == agent.ReasonRepeatedToolCalls:
		rec["subtype"], rec["is_error"], rec["result"] = "error_during_execution", true, repeatedCallsReason(res.Err)
		rec["terminal_reason"] = "repeated_tool_calls"
		code = 1
	case res.Reason == agent.ReasonMaxTurns:
		rec["subtype"], rec["is_error"] = "error_max_turns", true
		rec["result"] = fmt.Sprintf("Reached max turns (%s)", o.maxTurns)
	case res.HookBlocked:
		rec["subtype"], rec["is_error"], rec["result"] = "error_during_execution", true, renderLocalEffect(res.Effect)
		code = 1
	}
	out.emit(rec)
	return code
}
