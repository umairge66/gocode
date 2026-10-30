package provider

import (
	"errors"
	"net/http"
	"strings"
	"sync"
)

// This file holds the reasoning-effort plumbing shared by the three providers:
// the level vocabulary, the per-provider clamping, the Anthropic beta header and
// the "this model does not support effort" classifier the ModelCaller uses to
// drop effort and retry once (see adapter.go).

// EffortLevels are the valid reasoning-effort levels, lowest to highest.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// effortAliases maps accepted spellings onto a canonical level.
var effortAliases = map[string]string{"med": "medium"}

// NormalizeEffort trims and lower-cases raw, resolves an alias ("med") and
// reports whether the result is one of EffortLevels. An empty or unknown value
// returns ("", false).
func NormalizeEffort(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if a, ok := effortAliases[s]; ok {
		s = a
	}
	for _, l := range EffortLevels {
		if s == l {
			return s, true
		}
	}
	return "", false
}

// clampOpenAIEffort maps a level onto what the OpenAI-compatible wires accept
// (low / medium / high): xhigh and max clamp to high. Unknown → "".
func clampOpenAIEffort(level string) string {
	switch level {
	case "low", "medium", "high":
		return level
	case "xhigh", "max":
		return "high"
	default:
		return ""
	}
}

// anthropicEffortBeta is the anthropic-beta token that enables
// output_config.effort.
const anthropicEffortBeta = "effort-2025-11-24"

// anthropicContext1MBeta is the anthropic-beta token for the 1M-token context
// window (the `[1m]` model suffix).
const anthropicContext1MBeta = "context-1m-2025-08-07"

// addBetaHeader merges beta into the comma-separated anthropic-beta header,
// keeping any betas already present and never duplicating one.
func addBetaHeader(h http.Header, beta string) {
	var parts []string
	for _, v := range h.Values("anthropic-beta") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				if p == beta {
					return
				}
				parts = append(parts, p)
			}
		}
	}
	parts = append(parts, beta)
	h.Set("anthropic-beta", strings.Join(parts, ","))
}

// IsEffortUnsupported reports whether err is a provider 400 rejecting the
// effort parameter, so the caller can drop effort and retry. It matches the
// effort_unsupported messages:
//
//   - "This model does not support the effort parameter"
//   - output_config… "Extra inputs are not permitted"
//   - output_config… "requires a model that supports …"
//
// plus the OpenAI-compatible equivalents for reasoning_effort / reasoning
// ("Unsupported parameter: 'reasoning_effort'…", "Unrecognized request argument
// supplied: reasoning_effort", "reasoning is not supported …").
func IsEffortUnsupported(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	hay := strings.ToLower(ae.Detail)
	if u := ae.Unwrap(); u != nil {
		hay += " " + strings.ToLower(u.Error())
	}
	if strings.Contains(hay, "effort parameter") && strings.Contains(hay, "not support") {
		return true
	}
	if strings.Contains(hay, "output_config") &&
		(strings.Contains(hay, "extra inputs are not permitted") || strings.Contains(hay, "requires a model that supports")) {
		return true
	}
	if strings.Contains(hay, "effort_unsupported") {
		return true
	}
	// "reasoning" covers reasoning_effort (OpenAI) and reasoning.effort
	// (OpenRouter). Only consulted when effort was actually sent.
	if strings.Contains(hay, "reasoning") {
		if strings.Contains(hay, "unsupported") || strings.Contains(hay, "not supported") ||
			strings.Contains(hay, "unrecognized") || strings.Contains(hay, "does not support") {
			return true
		}
	}
	return false
}

// effortSupport remembers, per model id, that the provider rejected the effort
// parameter, so it is not re-sent for the rest of the session. A parent caller
// shares its set with the subagent callers derived from it (InheritEffort).
type effortSupport struct {
	mu          sync.Mutex
	unsupported map[string]bool
}

func (s *effortSupport) isUnsupported(model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unsupported[model]
}

func (s *effortSupport) markUnsupported(model string) {
	s.mu.Lock()
	if s.unsupported == nil {
		s.unsupported = map[string]bool{}
	}
	s.unsupported[model] = true
	s.mu.Unlock()
}
