package headless

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// StructuredOutput tool (--json-schema): the model returns its final answer by
// calling this tool once, with the answer as the tool input; the input is
// validated against the user's schema.
const (
	StructuredOutputTool        = "StructuredOutput"
	StructuredOutputDescription = "Use this tool to return your final response in the requested structured format. You MUST call this tool exactly once at the end of your response to provide the structured output."
	StructuredOutputOK          = "Structured output provided successfully"
	// StructuredOutputEnforce is the reminder sent when a turn ends without
	// the tool having been called.
	StructuredOutputEnforce = "[structured-output-enforce] You MUST call the StructuredOutput tool to complete this request. Call this tool now."
	// DefaultStructuredOutputRetries is the number of failed StructuredOutput
	// attempts after which the run gives up.
	DefaultStructuredOutputRetries = 5
)

// StructuredOutputRetriesExhausted renders the error of a run whose
// StructuredOutput attempts all failed validation.
func StructuredOutputRetriesExhausted(attempts int, lastErr string) string {
	msg := fmt.Sprintf("Failed to provide valid structured output after %d attempts", attempts)
	if lastErr == "" {
		return msg
	}
	return msg + " — last StructuredOutput error: " + lastErr
}

// StructuredOutputMismatch renders the tool error for an input that does not
// match the schema.
func StructuredOutputMismatch(detail string) string {
	return "Output does not match required schema: " + detail
}

// ParseJSONSchema parses a --json-schema value: it must be JSON and a JSON
// object. The returned message is the startup error to print ("" on success).
func ParseJSONSchema(raw string) (map[string]any, string) {
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, ErrJSONSchemaInvalid(err)
	}
	if dec.More() {
		return nil, ErrJSONSchemaInvalid(fmt.Errorf("unexpected data after the JSON value"))
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, ErrJSONSchemaNotObject
	}
	return obj, ""
}

// ValidateSchema checks value against a JSON Schema (the commonly used
// keywords: type, enum, const, properties, required, additionalProperties,
// patternProperties, items, prefixItems, min/max for numbers, strings, arrays
// and objects, multipleOf, pattern, uniqueItems, allOf, anyOf, oneOf, not, and
// local $ref into the root document). It returns "" when the value matches,
// else the violations joined with ", ", each as "<instancePath|root>: <message>".
func ValidateSchema(schema map[string]any, value any) string {
	v := validator{root: schema}
	v.check(schema, normalize(value), "")
	return strings.Join(v.errs, ", ")
}

type validator struct {
	root  map[string]any
	errs  []string
	depth int
}

func (v *validator) fail(path, msg string) {
	if path == "" {
		path = "root"
	}
	v.errs = append(v.errs, path+": "+msg)
}

// normalize re-decodes value with json.Number so integers are told apart from
// floats exactly.
func normalize(value any) any {
	b, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return value
	}
	return out
}

func (v *validator) check(schema any, value any, path string) {
	v.depth++
	defer func() { v.depth-- }()
	if v.depth > 64 {
		return
	}
	switch s := schema.(type) {
	case bool:
		if !s {
			v.fail(path, "boolean schema is false")
		}
		return
	case map[string]any:
		v.checkObject(s, value, path)
	}
}

func (v *validator) checkObject(s map[string]any, value any, path string) {
	if ref, ok := s["$ref"].(string); ok {
		if target, ok := v.resolve(ref); ok {
			v.check(target, value, path)
		}
	}
	if t, ok := s["type"]; ok && !matchesType(t, value) {
		v.fail(path, "must be "+typeNames(t))
		return
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(e, value) {
				found = true
				break
			}
		}
		if !found {
			v.fail(path, "must be equal to one of the allowed values")
		}
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, value) {
		v.fail(path, "must be equal to constant")
	}
	switch x := value.(type) {
	case json.Number:
		v.checkNumber(s, x, path)
	case string:
		v.checkString(s, x, path)
	case []any:
		v.checkArray(s, x, path)
	case map[string]any:
		v.checkProps(s, x, path)
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, sub := range all {
			v.check(sub, value, path)
		}
	}
	if anyOf, ok := s["anyOf"].([]any); ok {
		matched := false
		for _, sub := range anyOf {
			if v.matches(sub, value) {
				matched = true
				break
			}
		}
		if !matched {
			v.fail(path, "must match a schema in anyOf")
		}
	}
	if oneOf, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, sub := range oneOf {
			if v.matches(sub, value) {
				n++
			}
		}
		if n != 1 {
			v.fail(path, "must match exactly one schema in oneOf")
		}
	}
	if not, ok := s["not"]; ok && v.matches(not, value) {
		v.fail(path, "must NOT be valid")
	}
}

// matches reports whether value validates against sub, without recording
// errors.
func (v *validator) matches(sub any, value any) bool {
	probe := validator{root: v.root, depth: v.depth}
	probe.check(sub, value, "")
	return len(probe.errs) == 0
}

func (v *validator) resolve(ref string) (any, bool) {
	if ref == "#" {
		return v.root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var cur any = v.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func (v *validator) checkNumber(s map[string]any, n json.Number, path string) {
	f, err := n.Float64()
	if err != nil {
		return
	}
	if lim, ok := num(s["minimum"]); ok && f < lim {
		v.fail(path, "must be >= "+fmtNum(lim))
	}
	if lim, ok := num(s["maximum"]); ok && f > lim {
		v.fail(path, "must be <= "+fmtNum(lim))
	}
	if lim, ok := num(s["exclusiveMinimum"]); ok && f <= lim {
		v.fail(path, "must be > "+fmtNum(lim))
	}
	if lim, ok := num(s["exclusiveMaximum"]); ok && f >= lim {
		v.fail(path, "must be < "+fmtNum(lim))
	}
	if m, ok := num(s["multipleOf"]); ok && m > 0 {
		q := f / m
		if math.Abs(q-math.Round(q)) > 1e-6 {
			v.fail(path, "must be multiple of "+fmtNum(m))
		}
	}
}

func (v *validator) checkString(s map[string]any, str string, path string) {
	n := utf8.RuneCountInString(str)
	if lim, ok := num(s["minLength"]); ok && float64(n) < lim {
		v.fail(path, fmt.Sprintf("must NOT have fewer than %s characters", fmtNum(lim)))
	}
	if lim, ok := num(s["maxLength"]); ok && float64(n) > lim {
		v.fail(path, fmt.Sprintf("must NOT have more than %s characters", fmtNum(lim)))
	}
	if p, ok := s["pattern"].(string); ok {
		if re, err := regexp.Compile(p); err == nil && !re.MatchString(str) {
			v.fail(path, fmt.Sprintf("must match pattern %q", p))
		}
	}
}

func (v *validator) checkArray(s map[string]any, arr []any, path string) {
	if lim, ok := num(s["minItems"]); ok && float64(len(arr)) < lim {
		v.fail(path, fmt.Sprintf("must NOT have fewer than %s items", fmtNum(lim)))
	}
	if lim, ok := num(s["maxItems"]); ok && float64(len(arr)) > lim {
		v.fail(path, fmt.Sprintf("must NOT have more than %s items", fmtNum(lim)))
	}
	start := 0
	if prefix, ok := s["prefixItems"].([]any); ok {
		for i := 0; i < len(prefix) && i < len(arr); i++ {
			v.check(prefix[i], arr[i], path+"/"+strconv.Itoa(i))
		}
		start = len(prefix)
	}
	if items, ok := s["items"]; ok {
		if list, isList := items.([]any); isList {
			for i := 0; i < len(list) && i < len(arr); i++ {
				v.check(list[i], arr[i], path+"/"+strconv.Itoa(i))
			}
		} else {
			for i := start; i < len(arr); i++ {
				v.check(items, arr[i], path+"/"+strconv.Itoa(i))
			}
		}
	}
	if u, ok := s["uniqueItems"].(bool); ok && u {
		for i := range arr {
			for j := i + 1; j < len(arr); j++ {
				if jsonEqual(arr[i], arr[j]) {
					v.fail(path, fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i))
					return
				}
			}
		}
	}
}

func (v *validator) checkProps(s map[string]any, obj map[string]any, path string) {
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				v.fail(path, fmt.Sprintf("must have required property '%s'", name))
			}
		}
	}
	if lim, ok := num(s["minProperties"]); ok && float64(len(obj)) < lim {
		v.fail(path, fmt.Sprintf("must NOT have fewer than %s properties", fmtNum(lim)))
	}
	if lim, ok := num(s["maxProperties"]); ok && float64(len(obj)) > lim {
		v.fail(path, fmt.Sprintf("must NOT have more than %s properties", fmtNum(lim)))
	}
	props, _ := s["properties"].(map[string]any)
	patterns, _ := s["patternProperties"].(map[string]any)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := path + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
		known := false
		if sub, ok := props[k]; ok {
			known = true
			v.check(sub, obj[k], child)
		}
		for p, sub := range patterns {
			if re, err := regexp.Compile(p); err == nil && re.MatchString(k) {
				known = true
				v.check(sub, obj[k], child)
			}
		}
		if known {
			continue
		}
		switch ap := s["additionalProperties"].(type) {
		case bool:
			if !ap {
				v.fail(path, "must NOT have additional properties")
			}
		case map[string]any:
			v.check(ap, obj[k], child)
		}
	}
}

func matchesType(t any, value any) bool {
	switch x := t.(type) {
	case string:
		return isType(x, value)
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && isType(s, value) {
				return true
			}
		}
		return false
	}
	return true
}

func isType(t string, value any) bool {
	switch t {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return true
}

func typeNames(t any) string {
	switch x := t.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(t)
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(canonical(normalize(a)))
	bb, err2 := json.Marshal(canonical(normalize(b)))
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

// canonical rewrites numbers to their float value so 1 and 1.0 compare equal.
func canonical(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonical(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = canonical(e)
		}
		return out
	}
	return v
}
