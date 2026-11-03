package provider

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/apiclient"
)

// ModelCaller adapts a Provider (OpenAI-shaped chat completions) to the agent
// turn loop's agent.ModelCaller / agent.StreamingModelCaller interfaces. It is
// the translation seam between the two content models:
//
//	agent.ModelRequest (Anthropic-shaped blocks)      ChatRequest (OpenAI roles)
//	  system prompt                              ->     {role:"system"}
//	  user text block                            ->     {role:"user"}
//	  assistant text block                       ->     assistant .Content
//	  assistant tool_use{id,name,input}          ->     assistant .ToolCalls[]
//	  user tool_result{tool_use_id,content}      ->     {role:"tool",tool_call_id}
//	  ToolSchema{name,desc,input_schema}         ->     ChatTool{..,Parameters}
//
// and back:
//
//	ChatResponse.Content          -> assistant text block
//	ChatResponse.ToolCalls[]      -> tool_use blocks (arguments parsed to Input)
//	FinishReason tool_calls       -> agent.StopToolUse
//	FinishReason stop             -> agent.StopEndTurn
//	FinishReason length           -> agent.StopMaxTokens
//	FinishReason content_filter   -> agent.StopRefusal
//
// The adapter also stashes the last turn's usage and model id and exposes them
// via LastTurnUsage, structurally satisfying internal/conversation's
// TurnUsageReporter so local /cost accounting works.
type ModelCaller struct {
	// Provider is the backing model provider (required).
	Provider Provider
	// Model is the provider-specific model id sent on every request (required).
	Model string
	// DefaultMaxTokens is applied when a ModelRequest leaves MaxTokens unset (0).
	DefaultMaxTokens int
	// Temperature, when non-nil, is sent on every request.
	Temperature *float64
	// ImagesUnsupported, when true, degrades tool-produced image content to a
	// short text note instead of sending image parts — for a model that cannot
	// accept image input. A caller built by NewModelCaller derives it from the
	// capability table (CapabilitiesFor: a KNOWN non-vision model) at
	// construction and on every SetModel, and re-checks the table per request
	// (so OpenRouter catalog data that arrives later is honored). A
	// struct-literal caller keeps full manual control (default false: images
	// are sent).
	ImagesUnsupported bool

	// MaxRetries is how many times a transient failure (429 non-billing, 529 /
	// overloaded, 5xx, 408, 409, connection error) is retried with backoff
	// before giving up (see retry.go). NewModelCaller sets it from
	// GOCODE_MAX_RETRIES (default 10); 0 disables retries (the zero value of a
	// struct literal therefore never retries).
	MaxRetries int
	// OnRetry, when set, is called just before each backoff wait with the
	// 1-based retry number, the retry budget, the wait and the failure that
	// triggered it (the CLI shows it in the status line / on stderr).
	OnRetry func(attempt, max int, delay time.Duration, err error)
	// OnRetryStart, when set, is called after the wait, right before the retried
	// request is issued (the CLI restores the normal thinking status).
	OnRetryStart func(attempt, max int)
	// Sleep, Rand and Now are test seams for the backoff wait, the jitter sample
	// and the Retry-After HTTP-date clock. Nil uses a ctx-aware timer,
	// math/rand/v2 and time.Now. Sleep must return ctx.Err() when ctx is done.
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func() float64
	Now   func() time.Time

	// OnUsage, when set, receives the usage, model id and wall time of every
	// response this caller produces, right as it is produced. A caller
	// dedicated to one consumer (a subagent) reports its spend to the
	// session's cost ledger through it. A usage observer on the request
	// context (WithUsageObserver) takes its place for that call, so a
	// response is reported once.
	OnUsage UsageObserver

	// mu guards the last-turn usage stash. Turns are serial within a Submit, so
	// the mutex exists only to keep a concurrent LastTurnUsage reader (the cost
	// wrapper) race-clean.
	mu        sync.Mutex
	lastUsage apiclient.Usage
	lastModel string
	haveUsage bool

	// effort is the reasoning-effort level sent on every request ("" = unset),
	// guarded by mu (SetEffort / CurrentEffort). support remembers the models
	// that rejected effort this session so it is not re-sent to them; it is
	// shared with subagent callers derived via InheritEffort.
	effort  string
	support *effortSupport

	// fallbackModel is the armed fallback model ("" = none), guarded by mu
	// (SetFallbackModel / FallbackModel; see fallback.go).
	fallbackModel string

	// autoCaps is set by NewModelCaller: the capability table drives the
	// image fallback, effort gating and the default output cap.
	autoCaps bool
}

var (
	_ agent.ModelCaller          = (*ModelCaller)(nil)
	_ agent.StreamingModelCaller = (*ModelCaller)(nil)
)

// NewModelCaller builds a ModelCaller over the given provider and model id.
//
// The caller consults the capability table (CapabilitiesFor) for the active
// model: a known non-vision model gets the image text fallback
// (ImagesUnsupported), effort is omitted for a model known not to support it
// (and clamped to its supported levels), and the model's default output cap is
// applied when neither the request nor DefaultMaxTokens sets one. It also
// starts the non-blocking refresh of the provider's live capability cache
// (OpenRouter catalog, Anthropic /v1/models, OpenAI-compatible /models).
func NewModelCaller(p Provider, model string) *ModelCaller {
	m := &ModelCaller{Provider: p, Model: model, MaxRetries: MaxRetriesFromEnv(nil), autoCaps: true}
	m.ImagesUnsupported = m.knownNonVision(model)
	RefreshProviderCapabilities(p)
	return m
}

// providerName returns the backing provider's name ("" when none).
func (m *ModelCaller) providerName() string {
	if m.Provider == nil {
		return ""
	}
	return m.Provider.Name()
}

// Capabilities returns the capability-table entry for the caller's current
// model on its provider.
func (m *ModelCaller) Capabilities() Capabilities {
	return CapabilitiesFor(m.providerName(), m.CurrentModel())
}

// knownNonVision reports whether the table KNOWS model takes no images.
func (m *ModelCaller) knownNonVision(model string) bool {
	c := CapabilitiesFor(m.providerName(), model)
	return c.Known() && !c.Vision
}

// CallModel sends one request and returns the accumulated assistant turn. It is
// the non-streaming path. Transient failures are retried with backoff (see
// withRetry); an "effort unsupported" 400 is retried once without effort first
// (not counted against the retry budget).
func (m *ModelCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	start := time.Now()
	m.clearUsage()
	creq := m.toChatRequest(req, false)
	var chatResp ChatResponse
	err := m.withRetry(ctx, func() (bool, error) {
		var err error
		chatResp, err = m.Provider.Chat(ctx, creq)
		if m.dropEffortOnReject(&creq, err) {
			chatResp, err = m.Provider.Chat(ctx, creq)
		}
		return false, err
	})
	if err != nil {
		return agent.ModelResponse{}, err
	}
	m.stashUsage(ctx, chatResp, start)
	return fromChatResponse(chatResp), nil
}

// CallModelStream is CallModel with an optional StreamHandler: each incremental
// text delta from the provider's SSE stream is forwarded to h.OnText. The
// returned ModelResponse is still the fully accumulated turn, so a caller that
// ignores the handler is unaffected.
//
// Retry rule: a failure BEFORE the first delta reached the handler (a status
// error, a connection error, an early overloaded event) is retried like
// CallModel; once any delta was forwarded the error is returned as-is, since a
// retry would duplicate the already-shown output.
func (m *ModelCaller) CallModelStream(ctx context.Context, req agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	var (
		onDelta   func(string)
		delivered bool // written and read on the calling goroutine only
	)
	if h != nil && h.OnText != nil {
		onDelta = func(s string) {
			delivered = true
			h.OnText(s)
		}
	}
	start := time.Now()
	m.clearUsage()
	creq := m.toChatRequest(req, true)
	if h != nil && h.OnThinking != nil {
		// Reasoning streamed before the answer counts as delivered output too:
		// once shown, a retry would duplicate it.
		creq.OnReasoning = func(s string) {
			delivered = true
			h.OnThinking(s)
		}
	}
	var chatResp ChatResponse
	err := m.withRetry(ctx, func() (bool, error) {
		var err error
		chatResp, err = m.Provider.StreamChat(ctx, creq, onDelta)
		// An effort rejection is a 400 returned before any SSE event, so no delta
		// has been forwarded yet and the retry streams cleanly from the start.
		if !delivered && m.dropEffortOnReject(&creq, err) {
			chatResp, err = m.Provider.StreamChat(ctx, creq, onDelta)
		}
		return delivered, err
	})
	if err != nil {
		return agent.ModelResponse{}, err
	}
	m.stashUsage(ctx, chatResp, start)
	return fromChatResponse(chatResp), nil
}

// APIUsage converts a response's neutral usage into the ledger's
// apiclient.Usage (the Anthropic split: uncached input, cache read, cache
// write, output), tagged with the serving provider and any cost the provider
// reported charging.
func APIUsage(u ChatUsage, providerName string) apiclient.Usage {
	return apiclient.Usage{
		InputTokens:                u.PromptTokens,
		OutputTokens:               u.CompletionTokens,
		CacheReadInputTokens:       u.CacheReadTokens,
		CacheCreationInputTokens:   u.CacheCreationTokens,
		CacheCreation1hInputTokens: u.CacheCreation1hTokens,
		Provider:                   providerName,
		ReportedCostUSD:            u.CostUSD,
		CostReported:               u.CostReported,
	}
}

// clearUsage forgets the previous response's usage so a failed call reports
// none (LastTurnUsage ok=false) instead of the previous turn's again.
func (m *ModelCaller) clearUsage() {
	m.mu.Lock()
	m.lastUsage = apiclient.Usage{}
	m.haveUsage = false
	m.mu.Unlock()
}

// stashUsage records the usage and model id of the response just produced
// and reports them to the request's usage observer, else to OnUsage. start is
// when the call began.
func (m *ModelCaller) stashUsage(ctx context.Context, resp ChatResponse, start time.Time) {
	m.mu.Lock()
	m.lastUsage = APIUsage(resp.Usage, m.providerName())
	m.lastModel = resp.Model
	if m.lastModel == "" {
		m.lastModel = m.Model
	}
	m.haveUsage = true
	model, usage := m.lastModel, m.lastUsage
	m.mu.Unlock()
	obs := usageObserverFrom(ctx)
	if obs == nil {
		obs = m.OnUsage
	}
	if obs != nil {
		obs(model, usage, time.Since(start))
	}
}

// UsageObserver receives one response's model id, usage and wall time.
type UsageObserver func(model string, usage apiclient.Usage, d time.Duration)

type usageObserverKey struct{}

// WithUsageObserver returns ctx carrying obs: every response a ModelCaller
// produces for a call made with it is reported to obs (in place of the
// caller's OnUsage). A side query made over the session's shared caller —
// a summary, a hook verdict, a classifier check — uses it so its spend is
// accounted without disturbing the main loop's own accounting.
func WithUsageObserver(ctx context.Context, obs UsageObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, usageObserverKey{}, obs)
}

func usageObserverFrom(ctx context.Context) UsageObserver {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(usageObserverKey{}).(UsageObserver)
	return obs
}

// LastTurnUsage reports the usage and model id of the response most recently
// produced. ok is false until the first response. This method set satisfies
// internal/conversation's TurnUsageReporter structurally, so the cost wrapper
// records real per-turn usage into its ledger.
func (m *ModelCaller) LastTurnUsage() (model string, usage apiclient.Usage, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastModel, m.lastUsage, m.haveUsage
}

// SetModel retargets the caller: every request issued after it returns uses
// model. An empty id is ignored. It implements conversation.ModelSelector, so a
// `/model <id>` command switches the live provider model in place.
func (m *ModelCaller) SetModel(model string) {
	if model == "" {
		return
	}
	noVision := m.autoCaps && m.knownNonVision(model)
	m.mu.Lock()
	m.Model = model
	if m.autoCaps {
		m.ImagesUnsupported = noVision
	}
	m.mu.Unlock()
}

// CurrentModel returns the model id the next request will use.
func (m *ModelCaller) CurrentModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Model
}

// SetEffort sets the reasoning-effort level for every request issued after it
// returns. "" clears it (nothing is sent). The level is expected to be one of
// EffortLevels (see NormalizeEffort); the cli validates before calling.
func (m *ModelCaller) SetEffort(level string) {
	m.mu.Lock()
	m.effort = level
	m.mu.Unlock()
}

// CurrentEffort returns the configured effort level ("" when unset). It reports
// the configured level even for a model that rejected it (see EffortSupported).
func (m *ModelCaller) CurrentEffort() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.effort
}

// EffortSupported reports whether effort is sent to model: false when the
// capability table knows the model does not take it, or once the provider
// rejected the parameter for it this session.
func (m *ModelCaller) EffortSupported(model string) bool {
	m.mu.Lock()
	sup, auto := m.support, m.autoCaps
	m.mu.Unlock()
	if auto {
		if c := CapabilitiesFor(m.providerName(), model); c.Known() && !c.Effort {
			return false
		}
	}
	return !sup.isUnsupported(model)
}

// InheritEffort copies parent's effort level onto m and shares parent's
// per-model "effort unsupported" memory, so a subagent caller neither loses the
// session's effort nor re-sends it to a model that already rejected it.
func (m *ModelCaller) InheritEffort(parent *ModelCaller) {
	if parent == nil || parent == m {
		return
	}
	parent.mu.Lock()
	if parent.support == nil {
		parent.support = &effortSupport{}
	}
	level, sup := parent.effort, parent.support
	parent.mu.Unlock()
	m.mu.Lock()
	m.effort, m.support = level, sup
	m.mu.Unlock()
}

// effortFor returns the effort to send to model: the configured level unless
// the capability table knows the model does not take effort, or the model
// rejected effort earlier this session (the retry-on-rejection safety net for a
// wrong or missing table entry). A known model's level is clamped to the
// levels it supports (e.g. xhigh → high on a low/medium/high/max model).
func (m *ModelCaller) effortFor(model string, caps Capabilities) string {
	m.mu.Lock()
	level, sup, auto := m.effort, m.support, m.autoCaps
	m.mu.Unlock()
	if level == "" || sup.isUnsupported(model) {
		return ""
	}
	if auto && caps.Known() {
		if !caps.Effort {
			return ""
		}
		level = clampEffortTo(level, caps.EffortLevels)
	}
	return level
}

// dropEffortOnReject implements the graceful effort drop: when creq carried an
// effort and err is the provider's "effort unsupported" 400, it remembers the
// model as unsupported (so later requests omit effort), clears creq.Effort and
// reports true so the caller retries exactly once without it.
func (m *ModelCaller) dropEffortOnReject(creq *ChatRequest, err error) bool {
	if err == nil || creq.Effort == "" || !IsEffortUnsupported(err) {
		return false
	}
	m.mu.Lock()
	if m.support == nil {
		m.support = &effortSupport{}
	}
	sup := m.support
	m.mu.Unlock()
	sup.markUnsupported(creq.Model)
	creq.Effort = ""
	return true
}

// CurrentProvider returns the underlying Provider. The cli uses it to reach the
// optional ModelLister capability (via ListModels) for the /model picker. The
// Provider is fixed at construction, so no lock is taken.
func (m *ModelCaller) CurrentProvider() Provider { return m.Provider }

// toChatRequest flattens an agent.ModelRequest into the neutral ChatRequest.
//
// Output cap precedence: the request's MaxTokens, then the caller's explicit
// DefaultMaxTokens, then (NewModelCaller callers) GOCODE_MAX_OUTPUT_TOKENS or,
// for known models, the capability table's MaxOutputTokens — clamped so a
// rough estimate of the prompt plus the cap stays inside the context window.
func (m *ModelCaller) toChatRequest(req agent.ModelRequest, stream bool) ChatRequest {
	model := m.CurrentModel()
	m.mu.Lock()
	auto, imagesOff := m.autoCaps, m.ImagesUnsupported
	m.mu.Unlock()
	var caps Capabilities
	if auto {
		caps = CapabilitiesFor(m.providerName(), model)
		imagesOff = imagesOff || (caps.Known() && !caps.Vision)
	}

	out := ChatRequest{
		Model:         model,
		Temperature:   m.Temperature,
		Stream:        stream,
		Tools:         toChatTools(req.Tools),
		DeferredTools: toChatTools(req.DeferredTools),
		Effort:        m.effortFor(model, caps),
		CacheSystem:   req.CacheSystemPrompt && req.System != "",
		CacheMessages: req.CacheMessages,
	}

	// The system prompt travels as one message: System, then the changing
	// SystemContext. A caching provider splits it back at SystemCacheSplit.
	system := req.System
	if req.SystemContext != "" {
		if system != "" {
			out.SystemCacheSplit = len(system)
			system += "\n\n"
		}
		system += req.SystemContext
	}
	if !out.CacheSystem {
		out.SystemCacheSplit = 0
	}
	if system != "" {
		out.Messages = append(out.Messages, ChatMessage{Role: RoleSystem, Content: system})
	}
	for _, msg := range req.Messages {
		out.Messages = append(out.Messages, toChatMessages(msg, imagesOff)...)
	}

	out.MaxTokens = req.MaxTokens
	if out.MaxTokens == 0 {
		out.MaxTokens = m.DefaultMaxTokens
	}
	if out.MaxTokens == 0 && auto {
		// GOCODE_MAX_OUTPUT_TOKENS raises or lowers the model's default output
		// cap, up to the model's limit; it applies to unknown models too (uncapped
		// there, since their limit is unknown).
		mo := ResolveMaxOutput(caps, getenv(MaxOutputEnv))
		if mo.FromEnv || (caps.Known() && mo.Tokens > 0) {
			out.MaxTokens = fitOutputToWindow(mo.Tokens, caps.ContextWindow, out)
		}
	}
	return out
}

// minDefaultOutput is the floor for a window-clamped default output cap.
const minDefaultOutput = 4096

// fitOutputToWindow clamps a capability-derived output cap so a conservative
// estimate of the prompt (≈3 chars/token, plus a fixed allowance for tool
// schemas and framing) plus the cap fits the context window: several providers
// reject a request whose prompt + max_tokens exceeds the window. Never below
// minDefaultOutput (or the cap itself when that is smaller).
func fitOutputToWindow(capTokens, window int, req ChatRequest) int {
	if window <= 0 {
		return capTokens
	}
	chars := 0
	for _, msg := range req.Messages {
		chars += len(msg.Content)
		for _, tc := range msg.ToolCalls {
			chars += len(tc.Arguments) + len(tc.Name)
		}
		for _, im := range msg.Images {
			chars += len(im.Base64) / 64 // images are billed far below their base64 size
		}
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	room := window - (chars/3 + 2000)
	if room >= capTokens {
		return capTokens
	}
	floor := min(minDefaultOutput, capTokens)
	return max(room, floor)
}

// imagesUnsupportedNote is appended to a tool_result's text when the active
// model cannot receive images (ModelCaller.ImagesUnsupported), so a non-vision
// model still gets a legible result and the image is not sent.
const imagesUnsupportedNote = "\n\n(This tool produced image content, which the active model cannot receive; only its text result is shown.)"

// toChatMessages converts one agent.Message into one or more ChatMessages.
//
// Assistant turns collapse to a single assistant message (text content plus any
// tool_calls). User turns may expand: their tool_result blocks each become a
// standalone {role:"tool"} message, while any text is emitted as a user message.
// Blocks are walked in order so a mixed turn stays correctly ordered: pending
// user text is flushed before the first tool_result that follows it.
func toChatMessages(msg agent.Message, imagesUnsupported bool) []ChatMessage {
	if msg.Role == agent.RoleAssistant {
		var (
			text  string
			calls []ChatToolCall
		)
		for _, b := range msg.Content {
			switch b.Type {
			case apiclient.BlockText:
				text += b.Text
			case apiclient.BlockToolUse:
				calls = append(calls, ChatToolCall{
					ID:        b.ID,
					Name:      b.Name,
					Arguments: argsString(b.Input),
				})
			}
		}
		return []ChatMessage{{Role: RoleAssistant, Content: text, ToolCalls: calls}}
	}

	// User (or any non-assistant) turn.
	var (
		out     []ChatMessage
		pending string
	)
	flush := func() {
		if pending != "" {
			out = append(out, ChatMessage{Role: RoleUser, Content: pending})
			pending = ""
		}
	}
	for _, b := range msg.Content {
		switch b.Type {
		case apiclient.BlockText:
			pending += b.Text
		case apiclient.BlockToolResult:
			flush()
			tm := ChatMessage{
				Role:        RoleTool,
				Content:     b.Content,
				ToolCallID:  b.ToolUseID,
				LoadedTools: b.LoadedTools,
			}
			if len(b.Images) > 0 {
				if imagesUnsupported {
					// Non-vision model: keep the text, append a note, drop images.
					tm.Content += imagesUnsupportedNote
				} else {
					tm.Images = make([]ChatImage, len(b.Images))
					for i, im := range b.Images {
						tm.Images[i] = ChatImage{Base64: im.Base64, MediaType: im.MediaType}
					}
				}
			}
			out = append(out, tm)
		}
	}
	flush()
	if len(out) == 0 {
		// Preserve an empty user turn rather than dropping it entirely.
		out = append(out, ChatMessage{Role: RoleUser})
	}
	return out
}

// toChatTools converts agent tool schemas into neutral ChatTools.
func toChatTools(tools []agent.ToolSchema) []ChatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ChatTool, len(tools))
	for i, t := range tools {
		out[i] = ChatTool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Deferred:    t.Deferred,
		}
	}
	return out
}

// fromChatResponse translates a provider ChatResponse into an agent.ModelResponse.
func fromChatResponse(resp ChatResponse) agent.ModelResponse {
	var blocks []agent.ContentBlock
	if resp.Content != "" {
		blocks = append(blocks, agent.TextBlock(resp.Content))
	}
	for _, tc := range resp.ToolCalls {
		blocks = append(blocks, agent.ToolUseBlock(tc.ID, tc.Name, argsRaw(tc.Arguments)))
	}
	return agent.ModelResponse{
		Message:    agent.Message{Role: agent.RoleAssistant, Content: blocks, ID: resp.ID},
		StopReason: stopReason(resp.FinishReason),
		Usage: agent.ModelUsage{
			InputTokens:       resp.Usage.PromptTokens,
			OutputTokens:      resp.Usage.CompletionTokens,
			CachedInputTokens: resp.Usage.CacheReadTokens,
			CacheWriteTokens:  resp.Usage.CacheCreationTokens,
		},
	}
}

// stopReason maps an OpenAI finish_reason onto the agent loop's stop reason.
func stopReason(finish string) string {
	switch finish {
	case FinishToolCalls:
		return agent.StopToolUse
	case FinishStop:
		return agent.StopEndTurn
	case FinishLength:
		return agent.StopMaxTokens
	case FinishContentFilter:
		return agent.StopRefusal
	default:
		return agent.StopEndTurn
	}
}

// argsString renders a tool_use block's Input (JSON) as the OpenAI arguments
// string, defaulting to "{}" when empty.
func argsString(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	return string(input)
}

// argsRaw parses an OpenAI arguments string into a tool_use block's Input. An
// empty or blank-object string yields an empty object so the block is valid.
func argsRaw(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}
