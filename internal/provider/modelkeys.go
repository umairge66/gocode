package provider

import "sort"

// BuiltinModelKeys returns the normalized model keys of the built-in
// capability table (see normalizeModelID), sorted. Callers that need the
// models of one family — e.g. the availableModels family step-down, which
// picks the newest allowed model of a family — filter and order them.
func BuiltinModelKeys() []string {
	out := make([]string, 0, len(builtinModels))
	for k := range builtinModels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
