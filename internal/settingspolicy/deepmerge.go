package settingspolicy

// Recursive settings merge used by the admin-tier merge engine.
//
// The merge primitive is a lodash-style `mergeWith` deep merge with a per-key
// customizer. The admin-tier orchestrator merges the lower tiers with the
// default customizer (replaceCustomizer) and the top tier with replaceCustomizer, or
// with appendCustomizer when preserveTop is set. The
// DeepMerge(dst, src, appendArrays) seam maps appendArrays==false to
// replaceCustomizer and appendArrays==true to appendCustomizer.
//
// The customizers decide, per key, whether the settings-specific combine
// (specialCombine) applies or whether the default merge (arrayUnionDefault → baseDefault)
// runs. Difference between the two: under appendCustomizer the replaceOnlySpecialKeys keys
// ("awsPairs","ripgrep") are NOT given the specialCombine and instead fall through
// to the default array-union in arrayUnionDefault.

// alwaysSpecialKeys is the set of settings keys that always take the specialCombine
// combine (under both replaceCustomizer and appendCustomizer).
var alwaysSpecialKeys = []string{
	"allowedMcpServers",
	"availableModels",
	"strictKnownMarketplaces",
	"allowedChannelPlugins",
	"allowedProviders",
}

// replaceOnlySpecialKeys is the set of keys that take the specialCombine only under replaceCustomizer (the
// default, non-preserveTop merge). Under appendCustomizer they fall through to the
// default merge.
var replaceOnlySpecialKeys = []string{
	"awsPairs",
	"ripgrep",
}

// awsCanonicalVars is the ordered list of canonical AWS credential env var
// names, used by the awsPairs combine.
var awsCanonicalVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// parentPairSuppressor is the var-name prefix synthesized for AWS pairs that
// are inherited from a parent tier.
const parentPairSuppressor = "_PARENT_PAIR_SUPPRESSOR_"

// undefinedValue is the sentinel that stands in for JavaScript `undefined`.
// Values parsed from JSON never produce it (JSON has only null, modeled as a
// present nil), but it lets the merge apply the undefined-skip rule when a
// caller injects one. See assignMergeValue / specialCombine.
type undefinedValue struct{}

// jsUndefined is the shared undefined sentinel.
var jsUndefined any = undefinedValue{}

func isUndefined(v any) bool {
	_, ok := v.(undefinedValue)
	return ok
}

// customizer is a mergeWith-style customizer: given the destination
// value, source value and key, it returns (value, true) to override the merge
// for that key, or (nil, false) to fall through to the default deep merge
// (an `undefined` return).
type customizer func(objValue, srcValue any, key string) (any, bool)

// DeepMerge merges src into dst in place. When appendArrays is false it uses
// replaceCustomizer; when true (preserveTop / gateway sessions) it uses
// appendCustomizer. It is the MergeDeps.DeepMerge implementation.
func DeepMerge(dst, src map[string]any, appendArrays bool) {
	cz := replaceCustomizer
	if appendArrays {
		cz = appendCustomizer
	}
	baseMerge(dst, src, cz)
}

// baseMerge walks the keys of src and merges each
// into dst, deferring object/array keys to baseMergeDeep.
func baseMerge(dst, src map[string]any, cz customizer) {
	for key, srcValue := range src {
		objValue, has := dst[key]
		if isJSObject(srcValue) {
			baseMergeDeep(dst, key, objValue, has, srcValue, cz)
			continue
		}
		newValue, handled := cz(objValue, srcValue, key)
		if !handled {
			newValue = srcValue
		}
		assignMergeValue(dst, key, has, newValue)
	}
}

// baseMergeDeep handles a key whose source value is an object or array. The
// customizer gets first refusal; otherwise the merge recurses into a shared
// container (merging arrays index-wise and objects key-wise) or clones the
// source when the destination has no compatible container.
func baseMergeDeep(dst map[string]any, key string, objValue any, has bool, srcValue any, cz customizer) {
	newValue, handled := cz(objValue, srcValue, key)
	isCommon := !handled
	if isCommon {
		switch sv := srcValue.(type) {
		case []any:
			if ov, ok := objValue.([]any); ok {
				newValue = mergeArray(ov, sv, cz)
			} else {
				// objValue is not an array: clone the source and do not
				// recurse (isCommon becomes false).
				newValue = deepClone(sv)
				isCommon = false
			}
		case map[string]any:
			var container map[string]any
			if ov, ok := objValue.(map[string]any); ok {
				container = ov
			} else {
				container = map[string]any{}
			}
			baseMerge(container, sv, cz)
			newValue = container
			isCommon = false // recursion already done in place
		default:
			newValue = srcValue
			isCommon = false
		}
	}
	_ = isCommon
	assignMergeValue(dst, key, has, newValue)
}

// mergeArray performs index-wise array recursion. With replaceCustomizer and
// appendCustomizer this path is effectively unreachable (every array/array pair is
// resolved by the customizer at the parent key), but it is implemented to
// honour the generic mergeWith contract.
func mergeArray(dst, src []any, cz customizer) []any {
	out := make([]any, len(dst))
	copy(out, dst)
	for i, srcValue := range src {
		var objValue any
		has := i < len(out)
		if has {
			objValue = out[i]
		}
		var newValue any
		if isJSObject(srcValue) {
			switch sv := srcValue.(type) {
			case []any:
				if ov, ok := objValue.([]any); ok {
					newValue = mergeArray(ov, sv, cz)
				} else {
					newValue = deepClone(sv)
				}
			case map[string]any:
				container := map[string]any{}
				if ov, ok := objValue.(map[string]any); ok {
					container = ov
				}
				baseMerge(container, sv, cz)
				newValue = container
			}
		} else {
			v, handled := cz(objValue, srcValue, "")
			if !handled {
				v = srcValue
			}
			newValue = v
		}
		if i < len(out) {
			out[i] = newValue
		} else {
			out = append(out, newValue)
		}
	}
	return out
}

// assignMergeValue assigns a merged value: a defined value overwrites;
// an undefined value is written only when the key is absent (the undefined
// source values are skipped over existing keys).
func assignMergeValue(dst map[string]any, key string, has bool, value any) {
	if isUndefined(value) {
		if !has {
			dst[key] = value
		}
		return
	}
	dst[key] = value
}

// replaceCustomizer: alwaysSpecialKeys and replaceOnlySpecialKeys keys take the specialCombine, everything else the
// default merge.
func replaceCustomizer(objValue, srcValue any, key string) (any, bool) {
	if inList(alwaysSpecialKeys, key) || inList(replaceOnlySpecialKeys, key) {
		return specialCombine(objValue, srcValue, key), true
	}
	return arrayUnionDefault(objValue, srcValue, key)
}

// appendCustomizer: only alwaysSpecialKeys keys take the specialCombine; replaceOnlySpecialKeys keys fall through to
// the default merge (array union).
func appendCustomizer(objValue, srcValue any, key string) (any, bool) {
	if inList(alwaysSpecialKeys, key) {
		return specialCombine(objValue, srcValue, key), true
	}
	return arrayUnionDefault(objValue, srcValue, key)
}

// specialCombine is the per-key special combine. An undefined source leaves the
// destination untouched; an array source replaces (with an awsPairs-specific
// concat of inherited parent pairs); a non-object source wins outright; an
// object source is shallow-cloned with its array values copied.
func specialCombine(e, n any, key string) any {
	if isUndefined(n) {
		return e
	}
	if arr, ok := n.([]any); ok {
		if key == "awsPairs" {
			if dstArr, ok := e.([]any); ok {
				return append(cloneArray(arr), suppressedParentPairs(dstArr, arr)...)
			}
		}
		return cloneArray(arr)
	}
	m, ok := n.(map[string]any)
	if !ok {
		// Not a plain object (and not an array / undefined): the source wins.
		return n
	}
	// Shallow clone with array values copied.
	out := make(map[string]any, len(m))
	for k, v := range m {
		if a, ok := v.([]any); ok {
			out[k] = cloneArray(a)
		} else {
			out[k] = v
		}
	}
	return out
}

// arrayUnionDefault: arrays (except "fallbackModel") union as src-then-dst,
// deduped; otherwise it defers to baseDefault.
func arrayUnionDefault(e, n any, key string) (any, bool) {
	ea, eok := e.([]any)
	na, nok := n.([]any)
	if eok && nok && key != "fallbackModel" {
		return uniq(concatArrays(na, ea)), true // src then dst
	}
	return baseDefault(e, n, key)
}

// baseDefault is the deepest default. It clones modelPicker, replaces
// fallbackModel arrays, unions other arrays (dst-then-src), merges the two
// marketplace/mcp object keys shallowly, and otherwise returns undefined so
// the generic deep merge takes over.
func baseDefault(e, n any, key string) (any, bool) {
	if key == "modelPicker" && !isUndefined(n) {
		return cloneModelPicker(n), true
	}
	ea, eok := e.([]any)
	na, nok := n.([]any)
	if eok && nok {
		if key == "fallbackModel" {
			return n, true
		}
		return uniq(concatArrays(ea, na)), true // dst then src
	}
	em, emok := e.(map[string]any)
	nm, nmok := n.(map[string]any)
	if (key == "extraKnownMarketplaces" || key == "managedMcpServers") && emok && nmok {
		return shallowObjectMerge(em, nm), true // {...dst, ...src}
	}
	return nil, false // undefined: fall through to default deep merge
}

// cloneModelPicker shallow-clones the object, deep-cloning a top-level
// `options` array of objects one level.
func cloneModelPicker(e any) any {
	m, ok := e.(map[string]any)
	if !ok {
		return e
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	if opts, ok := m["options"].([]any); ok {
		no := make([]any, len(opts))
		for i, r := range opts {
			if rm, ok := r.(map[string]any); ok {
				no[i] = cloneShallow(rm)
			} else {
				no[i] = r
			}
		}
		out["options"] = no
	}
	return out
}

// shallowObjectMerge returns {...e, ...n}: e's keys overlaid by n's.
func shallowObjectMerge(e, n map[string]any) map[string]any {
	out := make(map[string]any, len(e)+len(n))
	for k, v := range e {
		out[k] = v
	}
	for k, v := range n {
		out[k] = v
	}
	return out
}

// suppressedParentPairs: for the awsPairs combine, it synthesizes
// suppressor entries for canonical AWS vars that the parent (dst) referenced
// but the child (src) does not, so inherited pairs are neutralized.
func suppressedParentPairs(dst, src []any) []any {
	srcVars := map[string]struct{}{}
	for _, p := range src {
		for _, v := range awsPairVarNames(p) {
			srcVars[v] = struct{}{}
		}
	}
	var dstVars []any
	for _, p := range dst {
		for _, v := range awsPairVarNames(p) {
			dstVars = append(dstVars, v)
		}
	}
	out := []any{}
	c := 0
	for _, iv := range uniq(dstVars) {
		name, _ := iv.(string)
		if !inList(awsCanonicalVars, name) {
			continue
		}
		if _, inSrc := srcVars[name]; inSrc {
			continue
		}
		c++
		out = append(out, map[string]any{
			"accessKeyIdVar":     name,
			"secretAccessKeyVar": parentPairSuppressor + itoa(c) + "_",
		})
	}
	return out
}

// awsPairVarNames returns the string-valued var names referenced by one AWS
// pair object.
func awsPairVarNames(p any) []string {
	m, ok := p.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range []string{"accessKeyIdVar", "secretAccessKeyVar", "sessionTokenVar"} {
		if s, ok := m[k].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- small helpers ---

// isJSObject reports whether v is object-like in the JS sense (array or plain
// object), i.e. the values routed through baseMergeDeep.
func isJSObject(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

func inList(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func concatArrays(a, b []any) []any {
	out := make([]any, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

func cloneArray(a []any) []any {
	out := make([]any, len(a))
	copy(out, a)
	return out
}

// uniq is lodash `uniq` under SameValueZero: comparable elements are deduped by
// value (first occurrence kept); non-comparable elements (maps, slices) are
// always retained, mirroring reference inequality for objects.
func uniq(in []any) []any {
	seen := map[any]struct{}{}
	out := make([]any, 0, len(in))
	for _, v := range in {
		if k, ok := comparableKey(v); ok {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		out = append(out, v)
	}
	return out
}

func comparableKey(v any) (any, bool) {
	switch v.(type) {
	case nil, string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return v, true
	default:
		return nil, false
	}
}

// deepClone deep-copies a JSON-like value (maps, slices, scalars).
func deepClone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepClone(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepClone(e)
		}
		return out
	default:
		return v
	}
}

// itoa renders a small non-negative int without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
