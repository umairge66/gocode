// Command automode-eval compares the auto mode classifier's prompt variants
// ("full" and "compact") on a labelled set of text-only cases.
//
//	go run ./cmd/automode-eval -prompt full|compact|both -cases <file> \
//	    [-provider openrouter] [-model M] [-rpm 12] [-max 200]
//
// Each case is classified through the normal automode.Classifier (the
// two-stage request) with the user's provider key from the environment
// (OPENROUTER_API_KEY / ANTHROPIC_API_KEY / OPENAI_API_KEY). Requests go to
// that provider's chat endpoint only — any other URL is refused, the model
// catalog is not fetched, provider-level retries are off — and are spaced to
// at most -rpm per minute and capped at -max in total. A 429 stops the run
// with partial results. The key is never printed. Nothing in a case is
// executed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/automode"
	"github.com/umairge66/gocode/internal/automode/eval"
	"github.com/umairge66/gocode/internal/provider"
)

// target is a provider's request URL: base is what the provider struct takes
// as BaseURL, endpoint the one URL the guard lets through.
type target struct{ base, endpoint string }

// endpoints are the only URLs the eval may reach, per provider (a variable so
// tests can point it at a local server).
var endpoints = map[string]target{
	"openrouter": {"https://openrouter.ai/api/v1/chat/completions", "https://openrouter.ai/api/v1/chat/completions"},
	"openai":     {"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
	"anthropic":  {"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

type options struct {
	prompt       string
	cases        string
	provider     string
	model        string
	rpm          int
	max          int
	parseRetries int
	only         string
	jsonOut      string
	verbose      bool
	skipFast     bool
}

func parseFlags(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("automode-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.prompt, "prompt", "both", "prompt variant: full, compact or both")
	fs.StringVar(&o.cases, "cases", "internal/automode/eval/testdata/cases.jsonl", "JSONL case file")
	fs.StringVar(&o.provider, "provider", "", "provider: openrouter, anthropic or openai (default $GOCODE_PROVIDER, else openrouter)")
	fs.StringVar(&o.model, "model", "", "classifier model (default $GOCODE_CODE_AUTO_MODE_MODEL, else $GOCODE_MODEL)")
	fs.IntVar(&o.rpm, "rpm", 12, "maximum requests per minute")
	fs.IntVar(&o.max, "max", 200, "hard cap on total requests")
	fs.IntVar(&o.parseRetries, "parse-retries", 1, "re-requests of an unparseable answer per stage (the live default is 4)")
	fs.StringVar(&o.only, "only", "", "comma-separated case ids to run (default: all)")
	fs.StringVar(&o.jsonOut, "json", "", "also write the raw outcomes as JSON to this file")
	fs.BoolVar(&o.verbose, "v", false, "log each raw model answer (truncated) to stderr")
	fs.BoolVar(&o.skipFast, "skip-fast", false, "answer the 64-token fast stage locally with an empty max_tokens reply instead of sending it "+
		"(for reasoning models whose fast stage always comes back empty: the thinking stage then decides, exactly as live; halves the requests)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.provider == "" {
		o.provider = strings.ToLower(strings.TrimSpace(getenv("GOCODE_PROVIDER")))
	}
	if o.provider == "" {
		o.provider = "openrouter"
	}
	if _, ok := endpoints[o.provider]; !ok {
		return o, fmt.Errorf("unknown provider %q", o.provider)
	}
	if o.model == "" {
		o.model = strings.TrimSpace(getenv("GOCODE_CODE_AUTO_MODE_MODEL"))
	}
	if o.model == "" {
		o.model = strings.TrimSpace(getenv("GOCODE_MODEL"))
	}
	if o.model == "" {
		return o, errors.New("no model: pass -model or set GOCODE_CODE_AUTO_MODE_MODEL / GOCODE_MODEL")
	}
	if o.rpm <= 0 || o.rpm > 600 {
		return o, errors.New("-rpm must be between 1 and 600")
	}
	if o.max <= 0 {
		return o, errors.New("-max must be positive")
	}
	return o, nil
}

func variantsFor(p string) ([]string, error) {
	switch strings.ToLower(p) {
	case "both":
		return []string{automode.PromptFull, automode.PromptCompact}, nil
	case automode.PromptFull, automode.PromptCompact:
		return []string{strings.ToLower(p)}, nil
	}
	return nil, fmt.Errorf("-prompt must be full, compact or both")
}

// backend builds the provider with the guarded transport.
func backend(name, key string, http eval.Doer) provider.Provider {
	base := endpoints[name].base
	switch name {
	case "anthropic":
		return &provider.Anthropic{APIKey: key, BaseURL: base, HTTP: http}
	case "openai":
		return &provider.OpenAI{APIKey: key, BaseURL: base, HTTP: http}
	}
	return &provider.OpenRouter{APIKey: key, BaseURL: base, HTTP: http}
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, getenv, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "automode-eval:", err)
		}
		return 2
	}
	variants, err := variantsFor(o.prompt)
	if err != nil {
		fmt.Fprintln(stderr, "automode-eval:", err)
		return 2
	}
	f, err := os.Open(o.cases)
	if err != nil {
		fmt.Fprintln(stderr, "automode-eval:", err)
		return 2
	}
	cases, err := eval.LoadCases(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(stderr, "automode-eval: cases:", err)
		return 2
	}
	if o.only != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(o.only, ",") {
			want[strings.TrimSpace(id)] = true
		}
		var kept []eval.Case
		for _, c := range cases {
			if want[c.ID] {
				kept = append(kept, c)
			}
		}
		cases = kept
	}

	key := ""
	for _, ev := range auth.ProviderEnvVars(auth.Provider(o.provider)) {
		if v := strings.TrimSpace(getenv(ev)); v != "" {
			key = v
			break
		}
	}
	if key == "" {
		fmt.Fprintf(stderr, "automode-eval: no API key in %s\n", strings.Join(auth.ProviderEnvVars(auth.Provider(o.provider)), "/"))
		return 2
	}
	redact := func(s string) string { return strings.ReplaceAll(s, key, "[redacted]") }

	guard := &eval.GuardedDoer{
		Next:     &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Endpoint: endpoints[o.provider].endpoint,
		Max:      o.max,
		Interval: time.Minute / time.Duration(o.rpm),
	}
	// A struct-literal caller: no catalog refresh (no other endpoint) and no
	// provider-level retries (every request is one the guard counted).
	var caller agent.ModelCaller = &provider.ModelCaller{Provider: backend(o.provider, key, guard), Model: o.model}
	if o.verbose {
		caller = &loggingCaller{next: caller, w: stderr, redact: redact}
	}
	if o.skipFast {
		caller = skipFastCaller{next: caller}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	fmt.Fprintf(stderr, "automode-eval: %d cases × %v on %s/%s (≤%d req/min, ≤%d requests)\n", len(cases), variants, o.provider, o.model, o.rpm, o.max)
	r := &eval.Runner{
		Classify: eval.NewClassifierFunc(caller, parseRetriesValue(o.parseRetries)),
		Stop:     guard.Stop(perCase(o.skipFast)),
		Progress: stderr,
		Redact:   redact,
	}
	rows, stopErr := r.Run(ctx, cases, variants)

	eval.WriteTable(stdout, rows, variants)
	fmt.Fprintf(stdout, "requests sent: %d\n", guard.Count())
	if len(variants) == 2 {
		cmp := eval.Compare(rows, variants[0], variants[1])
		fmt.Fprintf(stdout, "agreement %s vs %s: %d/%d (%.1f%%)\n", variants[0], variants[1], cmp.Agree, cmp.Paired, 100*cmp.Agreement())
		if len(cmp.Disagreements) > 0 {
			fmt.Fprintf(stdout, "  disagreements: %s\n", strings.Join(cmp.Disagreements, ", "))
		}
		fmt.Fprintf(stdout, "blocks %s caught but %s allowed: %d %v\n", variants[0], variants[1], len(cmp.Regressions), cmp.Regressions)
		d := eval.Decide(rows, len(cases), variants[0], variants[1])
		if d.Pass {
			fmt.Fprintln(stdout, "decision: PASS — compact may become the default")
		} else {
			fmt.Fprintln(stdout, "decision: FAIL — keep full as the default:")
			for _, why := range d.Reasons {
				fmt.Fprintln(stdout, "  -", why)
			}
		}
	}
	if o.jsonOut != "" {
		if err := writeJSON(o.jsonOut, rows); err != nil {
			fmt.Fprintln(stderr, "automode-eval:", err)
		}
	}
	if stopErr != nil {
		fmt.Fprintln(stderr, "automode-eval: stopped early:", redact(stopErr.Error()))
		return 1
	}
	return 0
}

// fastStageMaxTokens is the classifier's stage 1 max_tokens (automode's
// stage1MaxTokens); the thinking stage asks for far more.
const fastStageMaxTokens = 64

// skipFastCaller answers the fast stage locally with what a reasoning model
// returns for it (no text, stopped at max_tokens), so the classifier moves on
// to the thinking stage without spending a request.
type skipFastCaller struct{ next agent.ModelCaller }

func (s skipFastCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	if req.MaxTokens > 0 && req.MaxTokens <= fastStageMaxTokens {
		return agent.ModelResponse{Message: agent.AssistantMessage(), StopReason: agent.StopMaxTokens}, nil
	}
	return s.next.CallModel(ctx, req)
}

// loggingCaller logs every raw answer (stop reason and the first 300
// characters of text) — for diagnosing unparseable answers.
type loggingCaller struct {
	next   agent.ModelCaller
	w      io.Writer
	redact func(string) string
}

func (l *loggingCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	resp, err := l.next.CallModel(ctx, req)
	if err != nil {
		fmt.Fprintf(l.w, "  [model] max_tokens=%d error: %s\n", req.MaxTokens, l.redact(err.Error()))
		return resp, err
	}
	var b strings.Builder
	for _, c := range resp.Message.Content {
		b.WriteString(c.Text)
	}
	text := strings.Join(strings.Fields(b.String()), " ")
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	fmt.Fprintf(l.w, "  [model] max_tokens=%d stop=%s text=%q\n", req.MaxTokens, resp.StopReason, l.redact(text))
	return resp, err
}

// perCase is the request room a classification needs before it is started.
func perCase(skipFast bool) int {
	if skipFast {
		return 1
	}
	return 2
}

func parseRetriesValue(n int) int {
	if n <= 0 {
		return -1 // automode: negative = none
	}
	return n
}

type jsonRow struct {
	ID       string                  `json:"id"`
	Rule     string                  `json:"rule"`
	Expected string                  `json:"expected"`
	Outcomes map[string]eval.Outcome `json:"outcomes"`
}

func writeJSON(path string, rows []eval.Row) error {
	out := make([]jsonRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, jsonRow{ID: r.Case.ID, Rule: r.Case.Rule, Expected: r.Case.Expected, Outcomes: r.Outcomes})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
