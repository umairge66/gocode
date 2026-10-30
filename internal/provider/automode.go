package provider

import (
	"regexp"
	"strings"
)

// autoModeModelOrder is the release-order list of model families the
// auto-mode support check compares against (spelled with the wire prefix
// "claude-").
var autoModeModelOrder = []string{
	"claude-opus-4-0", "claude-sonnet-4-0", "claude-opus-4-1", "claude-sonnet-4-5",
	"claude-haiku-4-5", "claude-opus-4-5", "claude-opus-4-6", "claude-sonnet-4-6",
	"claude-opus-4-7", "claude-opus-4-8",
}

// datedFour matches the Claude 4.0 dated ids ("claude-opus-4-20250514"), which
// canonicalize to the "-4-0" family.
var datedFour = regexp.MustCompile(`^claude-(opus|sonnet)-4-\d{8}`)

// canonicalAutoModeID reduces a model id to the family id the auto-mode check
// compares.
func canonicalAutoModeID(model string) string {
	key, _ := normalizeModelID(model)
	if m := datedFour.FindStringSubmatch(key); m != nil || key == "claude-opus-4" || key == "claude-sonnet-4" {
		if m != nil {
			return "claude-" + m[1] + "-4-0"
		}
		return key + "-0"
	}
	best := ""
	for _, fam := range append(append([]string(nil), autoModeModelOrder...), "claude-haiku-5-5") {
		if (key == fam || strings.HasPrefix(key, fam+"-")) && len(fam) > len(best) {
			best = fam
		}
	}
	if best != "" {
		return best
	}
	return key
}

// olderThan reports whether canonical id a predates b in the release order, or
// is a Claude 3.x model.
func olderThan(a, b string) bool {
	if strings.Contains(a, "claude-3-") {
		return true
	}
	ia, ib := -1, -1
	for i, m := range autoModeModelOrder {
		if m == a {
			ia = i
		}
		if m == b {
			ib = i
		}
	}
	return ia != -1 && ia < ib
}

// ModelKey reduces a model id, alias-free OpenRouter slug or dated id to the
// capability table's key space ("Anthropic/Claude-Opus-4.5[1m]" →
// "claude-opus-4-5"), for comparing model ids written in different spellings.
func ModelKey(model string) string {
	k, _ := normalizeModelID(model)
	return k
}

// Supports1MSuffix reports whether model is a table model whose window grows
// with the `[1m]` suffix (window1M above the native window). Unknown models
// report false.
func Supports1MSuffix(model string) bool {
	_, e, ok := lookupBuiltin(ModelKey(model))
	return ok && e.window1M > e.window
}

// SupportsAutoMode reports whether model may run while the session is in auto
// mode: never a model older than Opus 4.6, and on a provider other than the
// first-party Anthropic API not Opus 4.6, Sonnet 4.6 or a Haiku other than
// Haiku 5.5. Any non-Claude model is supported (the release order only lists
// Claude families).
func SupportsAutoMode(providerName, model string) bool {
	n := canonicalAutoModeID(model)
	if olderThan(n, "claude-opus-4-6") {
		return false
	}
	// Provider is not first party.
	if !strings.EqualFold(strings.TrimSpace(providerName), "anthropic") {
		if n == "claude-opus-4-6" || n == "claude-sonnet-4-6" || (strings.Contains(n, "haiku") && n != "claude-haiku-5-5") {
			return false
		}
	}
	return true
}
