package toolrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// Tool input schemas declare counts and durations (Bash timeout, Read
// offset/limit, Grep head_limit, …) as JSON numbers. Models often send them
// with a fractional part or an exponent ("timeout": 120000.0, 1e5), which is
// still a whole number. Such values are accepted for integer fields instead of
// failing the call with a type error.

// unmarshal decodes the model's JSON input. A nil/empty payload decodes to the
// zero input struct (the tools validate required fields themselves). A whole
// number written as a float (120000.0, 1.2e5) decodes into an integer field.
func unmarshal(input json.RawMessage, v any) error {
	if len(input) == 0 {
		return nil
	}
	err := json.Unmarshal(input, v)
	if err == nil {
		return nil
	}
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) || !strings.HasPrefix(te.Value, "number") {
		return err
	}
	normalized, changed := normalizeWholeNumbers(input)
	if !changed {
		return err
	}
	if err2 := json.Unmarshal(normalized, v); err2 != nil {
		return err
	}
	return nil
}

// normalizeWholeNumbers rewrites every number in a JSON document that has a
// fractional part or exponent but an integral value into its plain integer
// spelling. changed reports whether anything was rewritten. Strings are never
// touched.
func normalizeWholeNumbers(input json.RawMessage) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return input, false
	}
	changed := false
	doc = rewriteNumbers(doc, &changed)
	if !changed {
		return input, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return input, false
	}
	return out, true
}

func rewriteNumbers(v any, changed *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = rewriteNumbers(e, changed)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = rewriteNumbers(e, changed)
		}
		return t
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			return t
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return t
		}
		*changed = true
		return json.Number(strconv.FormatInt(int64(f), 10))
	}
	return v
}
