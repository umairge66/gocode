package insights

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// This file holds the small amount of JavaScript-semantics machinery needed
// to produce JavaScript-compatible output byte-for-byte where it matters:
// insertion-ordered JSON objects (JS objects keep key insertion order, Go maps
// do not), JSON.stringify(v, null, 2) formatting, Number#toString,
// Number#toLocaleString("en-US") and UTF-16 based string slicing.

// jsObject is an insertion-ordered JSON object, the analogue of a JS object
// parsed by JSON.parse.
type jsObject struct {
	keys []string
	vals map[string]any
}

func newJSObject() *jsObject { return &jsObject{vals: map[string]any{}} }

// set assigns k (keeping its original position when it already exists, as a
// JS property assignment does).
func (o *jsObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsObject) get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

// str returns o[k] when it is a string, else "".
func (o *jsObject) str(k string) string {
	v, _ := o.get(k)
	s, _ := v.(string)
	return s
}

// obj returns o[k] when it is an object.
func (o *jsObject) obj(k string) *jsObject {
	v, _ := o.get(k)
	m, _ := v.(*jsObject)
	return m
}

// arr returns o[k] when it is an array.
func (o *jsObject) arr(k string) []any {
	v, _ := o.get(k)
	a, _ := v.([]any)
	return a
}

// truthy reports JS truthiness of o[k].
func (o *jsObject) truthy(k string) bool {
	v, ok := o.get(k)
	if !ok {
		return false
	}
	return jsTruthy(v)
}

func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	default:
		return true
	}
}

// jsString renders a value the way a JS template literal `${v}` would
// (undefined for a missing value).
func jsString(v any, ok bool) string {
	if !ok {
		return "undefined"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNum(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e, true)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// field renders o[k] as `${o.k}` would.
func (o *jsObject) field(k string) string {
	v, ok := o.get(k)
	return jsString(v, ok)
}

// parseJSValue parses JSON into ordered values: *jsObject, []any, string,
// float64, bool, nil.
func parseJSValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("insights: trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newJSObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
		return nil, errors.New("insights: unexpected delimiter")
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	default:
		return t, nil // string, bool, nil
	}
}

// stringifyJS is JSON.stringify(v, null, indent) for the ordered value model
// (indent "" = compact).
func stringifyJS(v any, indent string) string {
	var b strings.Builder
	writeJS(&b, v, indent, "")
	return b.String()
}

func writeJS(b *strings.Builder, v any, indent, cur string) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(jsNum(x))
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case string:
		writeJSString(b, x)
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		next := cur + indent
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + next)
			}
			writeJS(b, e, indent, next)
		}
		if indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte(']')
	case *jsObject:
		if x == nil || len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		next := cur + indent
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if indent != "" {
				b.WriteString("\n" + next)
			}
			writeJSString(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			writeJS(b, x.vals[k], indent, next)
		}
		if indent != "" {
			b.WriteString("\n" + cur)
		}
		b.WriteByte('}')
	case *counts:
		writeJS(b, x.toObject(), indent, cur)
	default:
		// Fall back to encoding/json for plain Go values.
		data, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
			return
		}
		pv, err := parseJSValue(data)
		if err != nil {
			b.WriteString("null")
			return
		}
		writeJS(b, pv, indent, cur)
	}
}

// writeJSString quotes s the way JSON.stringify does (no HTML escaping).
func writeJSString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteString(strconv.FormatInt(int64(r)>>4, 16))
				b.WriteString(strconv.FormatInt(int64(r)&0xf, 16))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jsNum is Number#toString for finite values.
func jsNum(f float64) string {
	if f == 0 {
		return "0"
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // e.g. 1e-07, 1.5e+21
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[:1]
	digits := strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + digits
}

// toLocale is Number#toLocaleString() under the en-US locale: grouping
// separators and at most three fraction digits.
func toLocale(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String()
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

func toLocaleInt(n int) string { return toLocale(float64(n)) }

// toFixed1 is Number#toFixed(1).
func toFixed1(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// jsRound is Math.round (halves round toward +Infinity).
func jsRound(f float64) float64 { return math.Floor(f + 0.5) }

// utf16Len is a JS string's .length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r) // range yields U+FFFD for invalid bytes, never -1
	}
	return n
}

// utf16Slice is s.slice(from, to) on UTF-16 code-unit offsets, never splitting
// a surrogate pair (a pair straddling a boundary is dropped on that side, as
// truncateUTF16 does).
func utf16Slice(s string, from, to int) string {
	pos := 0
	start, end := -1, len(s)
	for i, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if start < 0 && pos >= from {
			start = i
		}
		if pos+w > to {
			end = i
			break
		}
		pos += w
	}
	if start < 0 {
		return ""
	}
	if end < start {
		return ""
	}
	return s[start:end]
}

// truncateUTF16 returns the first n UTF-16 units of t,
// dropping a dangling high surrogate.
func truncateUTF16(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf16Len(s) <= n {
		return s
	}
	return utf16Slice(s, 0, n)
}

// counts is an insertion-ordered string→number tally (a JS `{}` used as a
// counter). Ordering matters: ties in the stable value sorts keep insertion
// order, exactly as Array#sort on Object.entries does.
type counts struct {
	keys []string
	vals map[string]float64
}

func newCounts() *counts { return &counts{vals: map[string]float64{}} }

func (c *counts) add(k string, v float64) {
	if c.vals == nil {
		c.vals = map[string]float64{}
	}
	if _, ok := c.vals[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.vals[k] += v
}

func (c *counts) get(k string) float64 {
	if c == nil {
		return 0
	}
	return c.vals[k]
}

func (c *counts) has(k string) bool {
	if c == nil {
		return false
	}
	_, ok := c.vals[k]
	return ok
}

func (c *counts) len() int {
	if c == nil {
		return 0
	}
	return len(c.keys)
}

type countEntry struct {
	key string
	val float64
}

// entries is Object.entries in JS key order (integer-like keys first,
// ascending, then insertion order).
func (c *counts) entries() []countEntry {
	if c == nil {
		return nil
	}
	var ints, rest []countEntry
	for _, k := range c.keys {
		e := countEntry{k, c.vals[k]}
		if isArrayIndex(k) {
			ints = append(ints, e)
		} else {
			rest = append(rest, e)
		}
	}
	sort.SliceStable(ints, func(i, j int) bool {
		a, _ := strconv.ParseUint(ints[i].key, 10, 32)
		b, _ := strconv.ParseUint(ints[j].key, 10, 32)
		return a < b
	})
	return append(ints, rest...)
}

// sortedDesc is Object.entries(c).sort((a,b)=>b[1]-a[1]) (stable).
func (c *counts) sortedDesc() []countEntry {
	e := c.entries()
	sort.SliceStable(e, func(i, j int) bool { return e[i].val > e[j].val })
	return e
}

func isArrayIndex(k string) bool {
	if k == "" || (len(k) > 1 && k[0] == '0') {
		return false
	}
	n, err := strconv.ParseUint(k, 10, 32)
	return err == nil && n < math.MaxUint32
}

func (c *counts) toObject() *jsObject {
	o := newJSObject()
	for _, e := range c.entries() {
		o.set(e.key, e.val)
	}
	return o
}

// MarshalJSON keeps JS key order.
func (c *counts) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("{}"), nil
	}
	return []byte(stringifyJS(c.toObject(), "")), nil
}

// UnmarshalJSON keeps the file's key order; non-numeric values are ignored.
func (c *counts) UnmarshalJSON(data []byte) error {
	v, err := parseJSValue(data)
	if err != nil {
		return err
	}
	*c = counts{vals: map[string]float64{}}
	if o, ok := v.(*jsObject); ok {
		c.addObject(o)
	}
	return nil
}

// addObject adds each numeric entry of o.
func (c *counts) addObject(o *jsObject) {
	if o == nil {
		return
	}
	for _, k := range o.keys {
		if f, ok := o.vals[k].(float64); ok {
			c.add(k, f)
		}
	}
}

// objectEntries is Object.entries(o) for an ordered object (JS key order).
func objectEntries(o *jsObject) []string {
	if o == nil {
		return nil
	}
	var ints, rest []string
	for _, k := range o.keys {
		if isArrayIndex(k) {
			ints = append(ints, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.SliceStable(ints, func(i, j int) bool {
		a, _ := strconv.ParseUint(ints[i], 10, 32)
		b, _ := strconv.ParseUint(ints[j], 10, 32)
		return a < b
	})
	return append(ints, rest...)
}

// fill interleaves a generated template's literal parts with its hole values.
func fill(parts []string, vals ...string) string {
	if len(vals) != len(parts)-1 {
		panic("insights: template hole count mismatch")
	}
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(vals) {
			b.WriteString(vals[i])
		}
	}
	return b.String()
}
