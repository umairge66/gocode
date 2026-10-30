package plugineval

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// jsRegexp compiles a pattern with JavaScript RegExp semantics (graders are
// written as `new RegExp(pattern, flags)`). The engine is
// regexp2 in ECMAScript mode, already in the module graph through goja. The
// flags d, g and y change only how JS reports matches, so they are accepted
// and ignored here; v is treated as u.
func jsRegexp(pattern, flags string) (*regexp2.Regexp, error) {
	opts := regexp2.ECMAScript
	for _, f := range flags {
		switch f {
		case 'i':
			opts |= regexp2.IgnoreCase
		case 'm':
			opts |= regexp2.Multiline
		case 's':
			opts |= regexp2.Singleline
		case 'u', 'v':
			opts |= regexp2.Unicode
		case 'd', 'g', 'y':
		default:
			return nil, fmt.Errorf("Invalid flags supplied to RegExp constructor '%s'", flags)
		}
	}
	re, err := regexp2.Compile(pattern, opts)
	if err != nil {
		return nil, fmt.Errorf("Invalid regular expression: /%s/%s: %v", pattern, flags, err)
	}
	return re, nil
}

// jsTest is RegExp.prototype.test.
func jsTest(re *regexp2.Regexp, s string) (bool, error) {
	return re.MatchString(s)
}

// jsCountMatches is `s.match(new RegExp(p, flags+"g"))?.length ?? 0`.
func jsCountMatches(re *regexp2.Regexp, s string) (int, error) {
	n := 0
	m, err := re.FindStringMatch(s)
	for m != nil && err == nil {
		n++
		m, err = re.FindNextMatch(m)
	}
	return n, err
}

// jsDefaultRegexp compiles a pattern with no flags (tool_used input_match).
func jsDefaultRegexp(pattern string) (*regexp2.Regexp, error) {
	return jsRegexp(pattern, "")
}

// globToPathRegexp: the file_exists glob ("**/" any directories, "**"
// anything, "*" one segment, "?" one character), anchored.
func globToPathRegexp(glob string) (*regexp2.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			if i+2 < len(glob) && glob[i+2] == '/' {
				b.WriteString("(?:.*/)?")
				i += 2
			} else {
				b.WriteString(".*")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString(".")
		case strings.IndexByte(`.+^${}()|[]\`, c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp2.Compile(b.String(), regexp2.ECMAScript)
}
