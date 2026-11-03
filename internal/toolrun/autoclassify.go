package toolrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/automode"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// This file runs the auto mode safety classifier for an "ask" the decider
// marked Classify (auto mode, no rule settled the call, not on the safe
// allowlist, not an edit inside the working directories). Deny rules, ask
// rules, the path guards, safety checks and sandbox rules were all applied by
// the decider first and are never sent here.
//
//   - allow       → the call runs;
//   - block       → denied; the model gets the classifier's reason
//    . After 3 consecutive or 20 total blocks the next block is
//     put to the user instead when someone can be asked;
//   - no verdict  → (request error, timeout, refusal, unparseable answer, or
//     no classifier wired) the user is asked when interactive, else the call
//     is denied with "temporarily unavailable" guidance.
//
// A call never runs without a verdict or the user's answer.

// AutoClassifier decides an auto mode call (automode.Classifier implements it).
type AutoClassifier interface {
	Classify(ctx context.Context, req automode.Request) automode.Result
}

// ReasonClassifier is the decision reason of an ask the classifier handed to
// the user (a denial-limit fallback or no verdict).
const ReasonClassifier = "classifier"

// WithAutoClassifier installs the auto mode classifier.
func WithAutoClassifier(c AutoClassifier) Option {
	return func(d *Dispatcher) { d.Classifier = c }
}

// SetAutoClassifier installs (or clears) the auto mode classifier.
func (d *Dispatcher) SetAutoClassifier(c AutoClassifier) { d.Classifier = c }

// denialTracking counts classifier blocks (consecutive / total denials).
type denialTracking struct {
	mu          sync.Mutex
	consecutive int
	total       int
}

type toolUseIDKey struct{}

// withToolUseID carries the dispatched tool_use id, so the classifier's
// transcript stops at this call.
func withToolUseID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolUseIDKey{}, id)
}

func toolUseIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(toolUseIDKey{}).(string)
	return id
}

// denyRuleStrings renders the live deny rules as "Tool(content)" strings for
// the classifier's settings-deny line.
func (d *Dispatcher) denyRuleStrings() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range d.SnapshotRules() {
		if r.Behavior != toolreg.RuleDeny {
			continue
		}
		s := r.ToolName
		if r.Content != "" {
			s += "(" + r.Content + ")"
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// classifierCwd is the working directory the call runs in (the classifier's
// verdict-cache key includes it).
func (d *Dispatcher) classifierCwd() string {
	if d.WorkDir != "" {
		return d.WorkDir
	}
	wd, _ := os.Getwd()
	return wd
}

// classifyAsk runs the classifier for a Classify ask. done=true means the
// outcome is final; otherwise next is the ask to put to the user.
func (d *Dispatcher) classifyAsk(ctx context.Context, pc toolreg.Call, input json.RawMessage, decision toolreg.Decision, canAsk bool) (out askOutcome, next toolreg.Decision, done bool) {
	res := automode.Result{Verdict: automode.VerdictUnavailable, Reason: "Classifier unavailable - blocking for safety"}
	if d.Classifier != nil {
		res = d.Classifier.Classify(ctx, automode.Request{
			Action:     automode.Action{Tool: string(pc.Tool), Input: input, ID: toolUseIDFrom(ctx), Hints: d.externalHints(string(pc.Tool))},
			Transcript: agent.TranscriptFrom(ctx),
			DenyRules:  d.denyRuleStrings(),
			Cwd:        d.classifierCwd(),
		})
	}
	if err := ctx.Err(); err != nil {
		return d.rejected(ctx, "", false), decision, true
	}
	switch res.Verdict {
	case automode.VerdictAllow:
		d.denials.mu.Lock()
		d.denials.consecutive = 0
		d.denials.mu.Unlock()
		return askOutcome{allowed: true}, decision, true
	case automode.VerdictBlock:
		d.denials.mu.Lock()
		d.denials.consecutive++
		d.denials.total++
		consec, total := d.denials.consecutive, d.denials.total
		limit := consec >= automode.MaxConsecutiveDenials || total >= automode.MaxTotalDenials
		if limit && canAsk && total >= automode.MaxTotalDenials {
			// The total counter restarts once the user is asked.
			d.denials.consecutive, d.denials.total = 0, 0
		}
		d.denials.mu.Unlock()
		if limit && canAsk {
			next = decision
			next.Classify = false
			next.ReasonType = ReasonClassifier
			next.Reason = automode.DenialLimitDisclosure(consec, total, res.Reason)
			return askOutcome{}, next, false
		}
		return askOutcome{allowed: false, denyMessage: automode.BlockMessage(res.Reason)}, decision, true
	default: // no verdict
		if canAsk {
			next = decision
			next.Classify = false
			next.ReasonType = ReasonClassifier
			next.Reason = "The auto mode classifier could not review this call (" + res.Reason + "). Approve it yourself?"
			return askOutcome{}, next, false
		}
		return askOutcome{allowed: false, denyMessage: automode.UnavailableMessage(string(pc.Tool), "", res)}, decision, true
	}
}

// outsideReadSuggestion is the session rule a "don't ask again" adds for a
// read outside the working directories, and its option label: Read(<dir>/**)
// for the directory read (a Read's parent directory; a Glob / Grep search
// directory itself).
func outsideReadSuggestion(pc toolreg.Call) ([]string, string) {
	dir := outsideReadDir(pc)
	if dir == "" || dir == "/" {
		return nil, ""
	}
	return []string{"Read(" + pathRuleContent(dir) + "/**)"}, "Yes, allow reading from " + dir + "/ during this session"
}

func outsideReadDir(pc toolreg.Call) string {
	p := pc.Path
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if pc.Tool == protocol.ToolRead {
		return filepath.Dir(p)
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return filepath.Dir(p)
	}
	return p
}
