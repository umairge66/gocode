package settingspolicy

// Settings is a dynamically-shaped settings object. Nested objects are
// map[string]any; arrays are []any; scalars are string/bool/float64/etc.
type Settings = map[string]any

// GetPath walks path through nested Settings objects and returns the value at
// the leaf. ok is false when any intermediate segment is missing or is not an
// object.
func GetPath(root any, path []string) (value any, ok bool) {
	cur := root
	for _, key := range path {
		m, isMap := cur.(map[string]any)
		if !isMap || m == nil {
			return nil, false
		}
		next, present := m[key]
		if !present {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// SetPath sets value at path, creating intermediate objects as needed. Each
// intermediate object along the path is cloned (copy-on-write) before being
// re-linked, so objects shared by reference below the mutation point are left
// untouched.
func SetPath(root map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	cur := root
	for _, key := range path[:len(path)-1] {
		clone := cloneShallow(asMap(cur[key]))
		cur[key] = clone
		cur = clone
	}
	cur[path[len(path)-1]] = value
}

// DeletePath removes the leaf at path, then prunes any ancestor objects that
// became empty as a result. Intermediate objects are cloned on the way down,
// matching SetPath's copy-on-write behavior.
func DeletePath(root map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	stack := []map[string]any{root}
	cur := root
	for _, key := range path[:len(path)-1] {
		clone := cloneShallow(asMap(cur[key]))
		cur[key] = clone
		cur = clone
		stack = append(stack, clone)
	}
	delete(cur, path[len(path)-1])
	// Prune empty ancestors from the leaf's parent upward.
	for i := len(stack) - 1; i > 0; i-- {
		if len(stack[i]) > 0 {
			break
		}
		delete(stack[i-1], path[i-1])
	}
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func cloneShallow(m map[string]any) map[string]any {
	clone := make(map[string]any, len(m))
	for k, v := range m {
		clone[k] = v
	}
	return clone
}
