package settingsload

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Property-list decoding for the macOS managed-preferences tier.
//
// The MDM loader hands each managed-preferences document to the settings
// parser as JSON text. The conversion is done in-process instead of spawning
// a converter:
// DecodePlist reads an XML ("<?xml … <plist>") or binary ("bplist00")
// property list into JSON-compatible Go values, and PlistToJSON renders that
// as the JSON text the shared document parser consumes. Like a JSON
// conversion, <date> and <data> values have no JSON form and are rejected,
// so a document carrying them is reported unreadable rather than half-applied.

// maxPlistBytes caps a plist document, matching the 2 MiB cap applied to
// every managed document read (registry output and settings files).
const maxPlistBytes = 2 * 1024 * 1024

// maxPlistDepth bounds container nesting so a hostile document cannot recurse
// without limit.
const maxPlistDepth = 512

// errPlistNoJSON is returned for a value that has no JSON representation.
var errPlistNoJSON = errors.New("invalid object in plist for destination format")

// DecodePlist decodes an XML or binary property list into map[string]any /
// []any / string / int64 / float64 / bool values.
func DecodePlist(data []byte) (any, error) {
	if len(data) > maxPlistBytes {
		return nil, fmt.Errorf("the document exceeds %d MiB", maxPlistBytes/1048576)
	}
	if bytes.HasPrefix(data, []byte("bplist00")) {
		return decodeBinaryPlist(data)
	}
	return decodeXMLPlist(data)
}

// PlistToJSON decodes a plist and renders it as JSON text.
func PlistToJSON(data []byte) (string, error) {
	v, err := DecodePlist(data)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := writeJSONValue(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeJSONValue(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return errPlistNoJSON
		}
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case string:
		b.WriteString(quoteJSON(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSONValue(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		first := true
		for _, k := range sortedKeys(x) {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(quoteJSON(k))
			b.WriteByte(':')
			if err := writeJSONValue(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return errPlistNoJSON
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort keeps the dependency surface minimal for small maps.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// --- XML plist ---------------------------------------------------------------

func decodeXMLPlist(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil, errors.New("not a property list")
			}
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "plist" {
				return nil, fmt.Errorf("unexpected root element <%s>", se.Name.Local)
			}
			v, err := xmlNextValue(dec, 0)
			if err != nil {
				return nil, err
			}
			return v, nil
		}
	}
}

// xmlNextValue reads the next value element. It returns errXMLEnd when the
// enclosing element closes instead.
var errXMLEnd = errors.New("end of container")

func xmlNextValue(dec *xml.Decoder, depth int) (any, error) {
	if depth > maxPlistDepth {
		return nil, errors.New("property list nests too deeply")
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil, errXMLEnd
		case xml.StartElement:
			return xmlValue(dec, t, depth)
		}
	}
}

func xmlText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			return b.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("unexpected <%s> inside text element", t.Name.Local)
		}
	}
}

func xmlValue(dec *xml.Decoder, se xml.StartElement, depth int) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return m, nil
			case xml.StartElement:
				if t.Name.Local != "key" {
					return nil, fmt.Errorf("expected <key> in <dict>, got <%s>", t.Name.Local)
				}
				k, err := xmlText(dec)
				if err != nil {
					return nil, err
				}
				v, err := xmlNextValue(dec, depth+1)
				if err == errXMLEnd {
					return nil, fmt.Errorf("missing value for key %q", k)
				}
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
		}
	case "array":
		arr := []any{}
		for {
			v, err := xmlNextValue(dec, depth+1)
			if err == errXMLEnd {
				return arr, nil
			}
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	case "string":
		return xmlText(dec)
	case "integer":
		s, err := xmlText(dec)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if n, err := strconv.ParseInt(s, 0, 64); err == nil {
			return n, nil
		}
		if n, err := strconv.ParseUint(s, 0, 64); err == nil {
			return n, nil
		}
		return nil, fmt.Errorf("invalid <integer> %q", s)
	case "real":
		s, err := xmlText(dec)
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid <real> %q", s)
		}
		return f, nil
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	case "date", "data":
		return nil, errPlistNoJSON
	default:
		return nil, fmt.Errorf("unknown plist element <%s>", se.Name.Local)
	}
}

// --- binary plist (bplist00) -------------------------------------------------

type bplist struct {
	data       []byte
	offsets    []uint64
	refSize    int
	inProgress map[uint64]bool
}

func decodeBinaryPlist(data []byte) (any, error) {
	if len(data) < 8+32 {
		return nil, errors.New("binary plist too short")
	}
	tr := data[len(data)-32:]
	offIntSize := int(tr[6])
	refSize := int(tr[7])
	numObjects := binary.BigEndian.Uint64(tr[8:16])
	topObject := binary.BigEndian.Uint64(tr[16:24])
	tableOffset := binary.BigEndian.Uint64(tr[24:32])
	if offIntSize < 1 || offIntSize > 8 || refSize < 1 || refSize > 8 {
		return nil, errors.New("binary plist trailer is invalid")
	}
	limit := uint64(len(data) - 32)
	if numObjects == 0 || topObject >= numObjects || tableOffset < 8 || tableOffset > limit ||
		numObjects > (limit-tableOffset)/uint64(offIntSize) {
		return nil, errors.New("binary plist trailer is out of range")
	}
	p := &bplist{data: data[:limit], refSize: refSize, inProgress: map[uint64]bool{}}
	p.offsets = make([]uint64, numObjects)
	for i := uint64(0); i < numObjects; i++ {
		at := tableOffset + i*uint64(offIntSize)
		off := readBEUint(data[at : at+uint64(offIntSize)])
		if off < 8 || off >= tableOffset {
			return nil, errors.New("binary plist object offset out of range")
		}
		p.offsets[i] = off
	}
	return p.object(topObject, 0)
}

func readBEUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	return n
}

func (p *bplist) need(off, n uint64) error {
	if off > uint64(len(p.data)) || n > uint64(len(p.data))-off {
		return errors.New("binary plist object runs past the end")
	}
	return nil
}

// count reads the length of a sized object starting at off (marker byte at
// off); it returns the length and the offset of the payload.
func (p *bplist) count(off uint64) (uint64, uint64, error) {
	info := p.data[off] & 0x0f
	pos := off + 1
	if info != 0x0f {
		return uint64(info), pos, nil
	}
	if err := p.need(pos, 1); err != nil {
		return 0, 0, err
	}
	m := p.data[pos]
	if m&0xf0 != 0x10 {
		return 0, 0, errors.New("binary plist length is not an integer")
	}
	sz := uint64(1) << (m & 0x0f)
	if sz > 8 {
		return 0, 0, errors.New("binary plist length too large")
	}
	if err := p.need(pos+1, sz); err != nil {
		return 0, 0, err
	}
	n := readBEUint(p.data[pos+1 : pos+1+sz])
	return n, pos + 1 + sz, nil
}

func (p *bplist) ref(at uint64) (uint64, error) {
	if err := p.need(at, uint64(p.refSize)); err != nil {
		return 0, err
	}
	return readBEUint(p.data[at : at+uint64(p.refSize)]), nil
}

func (p *bplist) object(idx uint64, depth int) (any, error) {
	if idx >= uint64(len(p.offsets)) {
		return nil, errors.New("binary plist reference out of range")
	}
	if depth > maxPlistDepth {
		return nil, errors.New("property list nests too deeply")
	}
	if p.inProgress[idx] {
		return nil, errors.New("binary plist contains a reference cycle")
	}
	off := p.offsets[idx]
	if err := p.need(off, 1); err != nil {
		return nil, err
	}
	marker := p.data[off]
	switch marker >> 4 {
	case 0x0:
		switch marker {
		case 0x00:
			return nil, nil
		case 0x08:
			return false, nil
		case 0x09:
			return true, nil
		}
		return nil, fmt.Errorf("unsupported binary plist marker 0x%02x", marker)
	case 0x1:
		sz := uint64(1) << (marker & 0x0f)
		if err := p.need(off+1, sz); err != nil {
			return nil, err
		}
		b := p.data[off+1 : off+1+sz]
		switch sz {
		case 1, 2, 4:
			return int64(readBEUint(b)), nil
		case 8:
			return int64(binary.BigEndian.Uint64(b)), nil
		case 16:
			// 128-bit integers: only values that fit in 64 bits are usable.
			hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
			if hi == 0 {
				return lo, nil
			}
			if hi == math.MaxUint64 && int64(lo) < 0 {
				return int64(lo), nil
			}
			return nil, errors.New("binary plist integer out of range")
		}
		return nil, errors.New("binary plist integer size invalid")
	case 0x2:
		sz := uint64(1) << (marker & 0x0f)
		if err := p.need(off+1, sz); err != nil {
			return nil, err
		}
		b := p.data[off+1 : off+1+sz]
		switch sz {
		case 4:
			return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), nil
		case 8:
			return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
		}
		return nil, errors.New("binary plist real size invalid")
	case 0x3, 0x4:
		return nil, errPlistNoJSON // date / data
	case 0x5:
		n, pos, err := p.count(off)
		if err != nil {
			return nil, err
		}
		if err := p.need(pos, n); err != nil {
			return nil, err
		}
		// ASCII string; bytes >= 0x80 are mapped as Latin-1 for safety.
		r := make([]rune, n)
		for i, c := range p.data[pos : pos+n] {
			r[i] = rune(c)
		}
		return string(r), nil
	case 0x6:
		n, pos, err := p.count(off)
		if err != nil {
			return nil, err
		}
		if n > uint64(len(p.data))/2 {
			return nil, errors.New("binary plist string too long")
		}
		if err := p.need(pos, n*2); err != nil {
			return nil, err
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(p.data[pos+uint64(i)*2:])
		}
		return string(utf16.Decode(u)), nil
	case 0x8:
		return nil, errPlistNoJSON // UID (keyed-archiver only)
	case 0xA, 0xC:
		n, pos, err := p.count(off)
		if err != nil {
			return nil, err
		}
		if n > uint64(len(p.data)) {
			return nil, errors.New("binary plist array too long")
		}
		p.inProgress[idx] = true
		defer delete(p.inProgress, idx)
		arr := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			r, err := p.ref(pos + i*uint64(p.refSize))
			if err != nil {
				return nil, err
			}
			v, err := p.object(r, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case 0xD:
		n, pos, err := p.count(off)
		if err != nil {
			return nil, err
		}
		if n > uint64(len(p.data)) {
			return nil, errors.New("binary plist dict too long")
		}
		p.inProgress[idx] = true
		defer delete(p.inProgress, idx)
		m := make(map[string]any, n)
		valBase := pos + n*uint64(p.refSize)
		for i := uint64(0); i < n; i++ {
			kr, err := p.ref(pos + i*uint64(p.refSize))
			if err != nil {
				return nil, err
			}
			vr, err := p.ref(valBase + i*uint64(p.refSize))
			if err != nil {
				return nil, err
			}
			kv, err := p.object(kr, depth+1)
			if err != nil {
				return nil, err
			}
			k, ok := kv.(string)
			if !ok {
				return nil, errors.New("binary plist dict key is not a string")
			}
			v, err := p.object(vr, depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	}
	return nil, fmt.Errorf("unsupported binary plist marker 0x%02x", marker)
}
