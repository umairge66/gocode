package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/provider"
)

// This file wires the context-compaction machinery into the
// live session: it constructs a compaction.Compactor and assigns it to the
// Conversation, so BOTH autocompact (the pre-turn threshold check in
// conversation.maybeCompact) and the manual /compact command
// (conversation.compactNow → EffectTriggerCompaction) actually summarize and
// rebuild the history instead of being inert. Nothing here touches the network
// except through the SAME provider model caller the session already uses — the
// summarizer issues an ordinary model call through internal/provider, with no
// telemetry and provider API keys only.

// Context windows come from the per-model capabilities table
// (provider.ContextWindowFor: the Claude catalog, documented OpenAI
// values, live OpenRouter model data, conservative defaults for unknown
// models), recomputed whenever the model changes (see syncCompactionWindow).
const (
	// compactionMaxOutput is the per-family output cap used for the window's
	// reserved-output math (Window.MaxOutputTokens). It only feeds
	// min(MaxOutputTokens, compaction.ReservedOutputCap=20000); any value >=
	// 20000 reserves the full 20000-token cap, as for any large-output model.
	// Kept as a single constant since each model's exact output cap is not
	// known offline.
	compactionMaxOutput = 32000

	// summaryMaxOutputTokens caps the summarization turn itself (the Compactor's
	// summary request). The summary is detailed (analysis + summary
	// sections), so a generous cap is used; it is DISTINCT from the window's
	// reserved-output figure above.
	summaryMaxOutputTokens = 16384
)

// contextWindowFor resolves the active model's context window in tokens from
// the per-model capabilities table.
func contextWindowFor(prov, modelID string) int {
	return provider.ContextWindowFor(prov, modelID)
}

// planWindow builds the compaction.Window for a provider+model. MaxOutputTokens
// feeds the reserved-output math only (see compactionMaxOutput).
func planWindow(prov, modelID string) compaction.Window {
	return compaction.Window{
		ContextWindow:   contextWindowFor(prov, modelID),
		MaxOutputTokens: compactionMaxOutput,
	}
}

// estimateCompactionTokens is the Planner's EstimateTokens seam: the
// chars/4 estimate (compaction.EstimateTokens) over text, tool_use
// name+input, tool_result content (+2000 per image), and thinking blocks. It is
// NOT the primary trigger signal: the conversation measures the live context
// from the provider's real usage of the most recent response
// (conversation.ContextTokens) and uses this estimate only for the
// messages appended since that response, or for the whole history before the
// first response / right after a compaction. There is no local tokenizer
// and never calls out to count tokens (no telemetry).
func estimateCompactionTokens(msgs []compaction.Message) int {
	return compaction.EstimateTokens(msgs)
}

// summarizeCaller adapts the session's live agent.ModelCaller to the
// compaction.ModelCaller seam the Compactor drives. It resolves the caller
// lazily through a getter so the summarization turn always uses the SAME live
// provider + model + key as the session: a `/model` switch retargets the shared
// caller in place and a `/provider` switch swaps conv.Model, and either is
// picked up here on the next summary call.
type summarizeCaller struct {
	// caller returns the conversation's current model caller.
	caller func() agent.ModelCaller
}

// Call issues the summarization request through the live model caller and reads
// back the assistant text + token usage.
func (s summarizeCaller) Call(ctx context.Context, req compaction.SummarizeRequest) (compaction.SummarizeResult, error) {
	mc := s.caller()
	if mc == nil {
		return compaction.SummarizeResult{}, errNoSummarizerCaller
	}

	// The single-turn request: the conversation span being summarized, then the
	// summary-request user turn (compaction.BuildSummaryPrompt). No tools are
	// advertised and no system prompt is sent — the prompt itself bans tool use
	// and carries every instruction.
	msgs := make([]agent.Message, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		msgs = append(msgs, agent.Message{Role: m.Role, Content: m.Content})
	}
	msgs = append(msgs, agent.Message{Role: req.Prompt.Role, Content: req.Prompt.Content})

	resp, err := mc.CallModel(ctx, agent.ModelRequest{
		Messages:  msgs,
		MaxTokens: req.MaxOutputTokens,
	})
	if err != nil {
		// A "Prompt is too long" provider error must drive the Compactor's
		// head-truncation retry rather than aborting the whole compaction, so it
		// is surfaced as an APIError (a non-fatal result), not a Go error.
		if conversation.IsPromptTooLong(err) {
			msg := conversation.ErrorChainText(err)
			if !strings.HasPrefix(msg, promptTooLongMarker) {
				// The Compactor keys on this prefix; the provider's raw
				// "N tokens > M" detail follows it.
				msg = promptTooLongMarker + ": " + msg
			}
			return compaction.SummarizeResult{APIError: msg}, nil
		}
		return compaction.SummarizeResult{}, err
	}

	var text strings.Builder
	for _, b := range resp.Message.Content {
		if b.Type == apiclient.BlockText {
			text.WriteString(b.Text)
		}
	}

	res := compaction.SummarizeResult{Text: text.String()}
	// Echo the summary turn's token usage when the caller reports it
	// (provider.ModelCaller does, via LastTurnUsage). This is read immediately
	// after the summary call, before any other turn can overwrite it.
	if r, ok := mc.(conversation.TurnUsageReporter); ok {
		if _, u, have := r.LastTurnUsage(); have {
			res.Usage = u
		}
	}
	return res, nil
}

// promptTooLongMarker is the provider error substring that triggers the
// Compactor's head-truncation retry.
const promptTooLongMarker = "Prompt is too long"

// errNoSummarizerCaller is returned if the summarizer is invoked with no live
// model caller (should not happen in a built session).
type compactionError string

func (e compactionError) Error() string { return string(e) }

const errNoSummarizerCaller = compactionError("compaction: no model caller available for summarization")

// buildCompactor constructs the session's Compactor. The Planner is seeded with
// the current model's context window, the autocompact config
// (Enabled, using internal/compaction's thresholds — ThresholdHeadroom
// etc., NOT re-invented here), and the chars/4 token estimator. The summarizer
// resolves the live caller lazily via callerFn so /model and /provider changes
// apply automatically.
//
// KeepRecentGroups is left at 0 for stock autocompact: the whole
// conversation is summarized and replaced by the single summary turn.
func buildCompactor(callerFn func() agent.ModelCaller, prov, modelID string) *compaction.Compactor {
	return &compaction.Compactor{
		Planner: compaction.Planner{
			Window:         planWindow(prov, modelID),
			Config:         compaction.Config{Enabled: true},
			EstimateTokens: estimateCompactionTokens,
		},
		Caller: summarizeCaller{caller: callerFn},
		// AnalysisFirst requests the richer <analysis>+<summary> reply shape (the
		// default); ExtractSummary strips the analysis.
		Prompt:          compaction.PromptOptions{AnalysisFirst: true},
		MaxOutputTokens: summaryMaxOutputTokens,
	}
}

// wireCompaction constructs the Compactor and assigns it to the conversation.
// It is called from buildSession once the conversation and the live caller
// exist. The caller is read through conv.Model (not captured) so the summarizer
// follows a later /provider swap.
func (r *sessionRuntime) wireCompaction(prov, modelID string) {
	if r.conv == nil {
		return
	}
	conv := r.conv
	r.conv.Compactor = buildCompactor(func() agent.ModelCaller { return conv.Model }, prov, modelID)
	r.compactionProvider = prov
	// A microcompaction that clears old Read results also forgets their read
	// state, so an Edit must re-Read the file the model can no longer see
	//. The dispatcher is read at call
	// time so a later rebuild is followed.
	r.conv.OnToolResultsCleared = func(paths []string) {
		if r.dispatcher == nil || r.dispatcher.FileState == nil {
			return
		}
		for _, p := range paths {
			r.dispatcher.FileState.Forget(p)
		}
	}
}

// syncCompactionWindow recomputes the Compactor's Planner window from the
// conversation's CURRENT model, so autocompact targets the active model's
// context size after a `/model` or `/provider` change. It is called before each
// turn (the window is read inside the pre-turn autocompact check), which keeps
// it current regardless of which model-change entry point fired.
func (r *sessionRuntime) syncCompactionWindow() {
	if r == nil || r.conv == nil || r.conv.Compactor == nil {
		return
	}
	model := r.conv.ModelID
	if pc, ok := r.conv.Model.(*provider.ModelCaller); ok {
		if m := pc.CurrentModel(); m != "" {
			model = m
		}
	}
	r.conv.Compactor.Planner.Window = planWindow(r.compactionProvider, model)
	r.conv.Compactor.Planner.Config.Enabled = !r.autoCompactOff.Load()
}

// setAutoCompact turns automatic compaction on or off. It takes effect at the
// next turn (syncCompactionWindow runs before each one); a manual /compact
// works either way.
func (r *sessionRuntime) setAutoCompact(on bool) {
	if r == nil {
		return
	}
	r.autoCompactOff.Store(!on)
}

// autoCompactSetting reports the autoCompactEnabled setting (default on).
func autoCompactSetting(env *Env) bool {
	settings, _ := cmdInfo{env: env}.effectiveSettings()
	if v, present := settingBool(settings, "autoCompactEnabled"); present {
		return v
	}
	return true
}

// compactionSnapshot estimates the token count and message count of a history
// slice, used by the hosts to report a real /compact result ("summarized N
// messages, freed ~X tokens").
func compactionSnapshot(hist []agent.Message) (tokens, msgs int) {
	// Every block counts (tool calls, tool results, thinking), not only the
	// text: a tool-heavy history is mostly tool output, which compaction frees.
	for _, m := range hist {
		tokens += compaction.EstimateMessageTokens(compaction.Message{Role: m.Role, Content: m.Content})
	}
	return tokens, len(hist)
}

// compactionReport renders the user-facing result of a manual /compact, using a
// before/after snapshot of the history. preTokens/preMsgs are the pre-compaction
// figures captured just before conv.Submit; res carries the rebuilt history.
// When no compaction actually ran (empty history, or the Compactor is not
// wired), it reports that nothing was compacted.
func compactionReport(res conversation.Result, preTokens, preMsgs int) string {
	if !res.Compacted {
		return "Nothing to compact yet."
	}
	postTokens, postMsgs := compactionSnapshot(res.History)
	freed := preTokens - postTokens
	if freed < 0 {
		freed = 0
	}
	// Messages summarized = everything before compaction minus the tail kept
	// verbatim (postMsgs-1, excluding the injected summary turn). For stock
	// autocompact (no tail) this is all pre-compaction messages.
	summarized := preMsgs - (postMsgs - 1)
	if summarized < 0 {
		summarized = 0
	}
	return fmt.Sprintf("Compacted: summarized %d messages, freed ~%d tokens.", summarized, freed)
}
