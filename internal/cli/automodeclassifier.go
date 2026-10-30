package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/automode"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// This file wires the auto mode safety classifier (internal/automode) into a
// session: its requests go through the session's own model caller — the
// user's configured provider and key, nothing else — optionally with a
// classifier-specific model on that same provider.
//
// Classifier model, in order:
//
//  1. $GOCODE_CODE_AUTO_MODE_MODEL;
//  2. settings autoMode.model from the trusted sources (user, --settings,
//     policy);
//  3. the session's current model.

// classifierMaxRetries is the classifier's transient-failure retry budget
// .
const classifierMaxRetries = 4

// autoModeClassifierModel resolves the configured classifier model ("" keeps
// the session model).
func autoModeClassifierModel(env *Env) string {
	if v := strings.TrimSpace(envGetenv(env)("GOCODE_CODE_AUTO_MODE_MODEL")); v != "" {
		return v
	}
	ctx := settingsContext(env)
	model := ""
	for _, src := range autoModeTrustedSources {
		res := settingsload.LoadSource(fsfacade.OSFS{}, ctx, src)
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if m, ok := res.Settings["autoMode"].(map[string]any); ok {
			if s, ok := m["model"].(string); ok && strings.TrimSpace(s) != "" {
				model = strings.TrimSpace(s) // later (higher-priority) sources win
			}
		}
	}
	return model
}

// autoModePromptVariant resolves the classifier prompt variant ("full" or
// "compact"; "" keeps automode.DefaultPrompt): $GOCODE_CODE_AUTO_MODE_PROMPT,
// then settings autoMode.prompt from the trusted sources (user, --settings,
// policy; later sources win). Unrecognized values are ignored.
func autoModePromptVariant(env *Env) string {
	if v, ok := automode.NormalizePrompt(envGetenv(env)("GOCODE_CODE_AUTO_MODE_PROMPT")); ok {
		return v
	}
	ctx := settingsContext(env)
	variant := ""
	for _, src := range autoModeTrustedSources {
		res := settingsload.LoadSource(fsfacade.OSFS{}, ctx, src)
		if res.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		if m, ok := res.Settings["autoMode"].(map[string]any); ok {
			if s, ok := m["prompt"].(string); ok {
				if v, ok := automode.NormalizePrompt(s); ok {
					variant = v
				}
			}
		}
	}
	return variant
}

// classifierCaller sends the classifier's requests with a classifier model on
// the session's current provider. A session caller that is not a provider
// caller (tests, a session still unconfigured) is used as is.
type classifierCaller struct {
	session agent.ModelCaller
	model   string

	mu     sync.Mutex
	cached *provider.ModelCaller
	from   *provider.ModelCaller
}

func (c *classifierCaller) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	cur := c.session
	if l, ok := cur.(liveCaller); ok {
		cur = l.get()
	}
	pc, ok := cur.(*provider.ModelCaller)
	if !ok || c.model == "" {
		return c.session.CallModel(ctx, req)
	}
	c.mu.Lock()
	if c.cached == nil || c.from != pc {
		mc := provider.NewModelCaller(pc.Provider, c.model)
		mc.MaxRetries = classifierMaxRetries
		mc.Sleep = pc.Sleep
		c.cached, c.from = mc, pc
	}
	mc := c.cached
	c.mu.Unlock()
	return mc.CallModel(ctx, req)
}

// autoModeDebugEnv, when set (not "0"), logs each classifier request's token
// usage — including prompt-cache hits — to stderr. Local only.
const autoModeDebugEnv = "GOCODE_AUTO_MODE_DEBUG"

// newAutoModeClassifier builds the session's classifier. memory returns the
// loaded gocode.md block, sent as the user's configuration.
func newAutoModeClassifier(env *Env, session agent.ModelCaller, memory func() string) *automode.Classifier {
	var caller agent.ModelCaller = session
	if m := autoModeClassifierModel(env); m != "" {
		caller = &classifierCaller{session: session, model: m}
	}
	var debugf func(string, ...any)
	if v := strings.TrimSpace(envGetenv(env)(autoModeDebugEnv)); v != "" && v != "0" && env != nil && env.Stderr != nil {
		w := env.Stderr
		debugf = func(format string, args ...any) { fmt.Fprintf(w, "[debug] "+format+"\n", args...) }
	}
	return &automode.Classifier{
		Caller: caller,
		Debugf: debugf,
		Rules: func() *automode.Rules {
			r, variant := userAutoModeRules(env), autoModePromptVariant(env)
			if r == nil {
				if variant == "" {
					return nil
				}
				return &automode.Rules{Prompt: variant}
			}
			return &automode.Rules{Allow: r.Allow, SoftDeny: r.SoftDeny, HardDeny: r.HardDeny, Environment: r.Environment, Prompt: variant}
		},
		UserConfig: memory,
	}
}
