package cli

import (
	"context"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/provider"
)

// Whole-session cost accounting.
//
// The main loop's rounds are recorded by the conversation's own cost wrapper
// (conversation.costModel). Everything else that calls a model is recorded
// here, into the same ledger, so /cost, the status line, the headless result
// and the spend cap see the whole tree's spend:
//
//   - subagents: each child's caller is built fresh by the subagent model
//     factory and reports every response through provider.ModelCaller.OnUsage;
//   - compaction summaries: the summarizer calls the session caller with a
//     usage observer on the context (provider.WithUsageObserver);
//   - side queries over the session caller (WebFetch summaries, prompt hook
//     verdicts, the auto mode classifier, /insights, $.model.fork): the same,
//     through sideQueryCaller, and counted in the ledger's side-query share.
//
// A context observer replaces the caller's OnUsage for that call, so no
// response is counted twice; the main loop's context never carries one.

// recordUsage records a subagent or compaction response into the session
// ledger (nil-safe before the conversation exists).
func (r *sessionRuntime) recordUsage(model string, u apiclient.Usage, d time.Duration) {
	if r != nil && r.conv != nil {
		r.conv.RecordUsage(model, u, d)
	}
}

// recordSideQuery records a side query's response into the session ledger.
func (r *sessionRuntime) recordSideQuery(model string, u apiclient.Usage, d time.Duration) {
	if r != nil && r.conv != nil {
		r.conv.RecordSideQuery(model, u, d)
	}
}

// sideQuery wraps inner so every call through it is accounted as a side
// query. A nil inner stays nil.
func (r *sessionRuntime) sideQuery(inner agent.ModelCaller) agent.ModelCaller {
	if inner == nil {
		return nil
	}
	return sideQueryCaller{inner: inner, record: r.recordSideQuery}
}

// sideQueryCaller calls inner with a usage observer on the context, so the
// provider caller reports the response's usage to record.
type sideQueryCaller struct {
	inner  agent.ModelCaller
	record provider.UsageObserver
}

func (s sideQueryCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	return s.inner.CallModel(provider.WithUsageObserver(ctx, s.record), req)
}

// withChildUsage makes a subagent caller report its spend to the session
// ledger. Only a caller built for the child is changed: one shared with the
// session (a test caller handed through as is) is already accounted.
func (r *sessionRuntime) withChildUsage(mc, session agent.ModelCaller) agent.ModelCaller {
	if pc, ok := mc.(*provider.ModelCaller); ok && mc != session {
		pc.OnUsage = r.recordUsage
	}
	return mc
}
