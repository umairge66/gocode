// This file holds the shared result builders and value coercions used by all
// three importers.

package configimport

import (
	"encoding/json"
	"sort"
)

func (r *ImportResult) add(it ImportItem) {
	r.Items = append(r.Items, it)
}

func (r *ImportResult) unmap(scope Scope, label, reason string) {
	r.Unmappable = append(r.Unmappable, UnmappableItem{Scope: scope, Label: label, Reason: reason})
}

func sortedKeys(m map[string]any) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// toStringSlice coerces a parsed value to []string, dropping non-strings.
func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// toStringMap coerces a parsed value to map[string]string, dropping non-strings.
func toStringMap(v any) map[string]string {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, val := range m {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
		return out
	case map[string]string:
		return m
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// jsonFingerprint renders a value as canonical JSON for dedup fingerprints.
func jsonFingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
