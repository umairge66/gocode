package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/conversation"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// errNoModel is returned by the default model-caller builder when no model has
// been chosen. There is deliberately no built-in default model: the model comes
// from --model, the persisted "model" setting, or /model in a session.
var errNoModel = errors.New(`no model selected: pass --model <id>, set "model" in settings, or run /model in a session`)

// errNoCredentials / noCredentialsError are the no-key variants (run.go).

// isSetupError reports whether err only means the session is not configured
// yet (no API key or no model) — something the user can fix from inside an
// interactive session — as opposed to a real configuration error (invalid
// provider, invalid --effort) that should stop startup.
func isSetupError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNoModel) || errors.Is(err, errNoCredentials) {
		return true
	}
	for _, p := range validProviders() {
		if err.Error() == noCredentialsError(p).Error() {
			return true
		}
	}
	return false
}

// unconfiguredCaller stands in for the model caller when an interactive session
// starts without an API key or a model. It lets gocode launch and lets the user
// fix the setup with /login, /provider or /model; a message sent before then
// gets a clear, actionable error instead of a crash or a network failure.
type unconfiguredCaller struct {
	provider string
	needKey  bool
}

func (u unconfiguredCaller) reason() string {
	if u.needKey {
		return fmt.Sprintf("No API key for %s yet — run /login to add one (or /provider to switch), then /model to pick a model.", u.provider)
	}
	return "No model selected yet — run /model to pick one."
}

func (u unconfiguredCaller) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New(u.reason())
}

// apiKeyHelperNotice is shown when a settings file sets "apiKeyHelper". The
// helper is not wired in this build (it would run a shell command to fetch the
// key), so instead of silently ignoring the setting we say so.
const apiKeyHelperNotice = "apiKeyHelper is set in your settings but isn't supported in this build — add your key with /login or a <PROVIDER>_API_KEY environment variable."

// apiKeyHelperConfigured reports whether any effective settings source sets a
// non-empty "apiKeyHelper".
func apiKeyHelperConfigured(env *Env) bool {
	ctx := settingsContext(env)
	for _, or := range settingsload.EffectiveSources(fsfacade.OSFS{}, ctx, settingsload.CanonicalSourceOrder) {
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		switch v := or.Result.Settings["apiKeyHelper"].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		case map[string]any:
			if len(v) > 0 {
				return true
			}
		}
	}
	return false
}

// placeholderFor returns the unconfigured stand-in describing what is missing
// for session s (a key, or only a model).
func placeholderFor(s *printSession) unconfiguredCaller {
	return unconfiguredCaller{provider: s.Provider, needKey: s.APIKey == ""}
}

// activateCaller tries to build the real model caller from s and install it on
// conv. It returns (true, nil) when a real caller is now active, (false, nil)
// when setup is still incomplete (the placeholder is kept, refreshed to say what
// is still missing), and (false, err) for a real configuration error.
func activateCaller(conv *conversation.Conversation, s *printSession) (bool, error) {
	caller, err := s.buildCaller()
	if err != nil {
		if isSetupError(err) {
			conv.Model = placeholderFor(s)
			conv.ModelID = s.Model
			return false, nil
		}
		return false, err
	}
	conv.Model = caller
	conv.ModelID = s.Model
	return true, nil
}

// isUnconfigured reports whether conv is still running on the placeholder.
func isUnconfigured(conv *conversation.Conversation) bool {
	_, ok := conv.Model.(unconfiguredCaller)
	return ok
}

// liveCaller forwards every call to the session's CURRENT model caller (read at
// call time), so components that captured a caller at startup — the subagent
// factory, prompt hooks, the WebFetch summarizer — follow /login, /provider and
// /model instead of keeping the caller the session started with.
type liveCaller struct {
	get func() agent.ModelCaller
}

func (l liveCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	c := l.get()
	if c == nil {
		return agent.ModelResponse{}, errors.New("no model caller")
	}
	return c.CallModel(ctx, req)
}

func (l liveCaller) CallModelStream(ctx context.Context, req agent.ModelRequest, h *agent.StreamHandler) (agent.ModelResponse, error) {
	c := l.get()
	if c == nil {
		return agent.ModelResponse{}, errors.New("no model caller")
	}
	if sc, ok := c.(agent.StreamingModelCaller); ok {
		return sc.CallModelStream(ctx, req, h)
	}
	return c.CallModel(ctx, req)
}
