package bedrock

import (
	"bufio"
	"os"
	"strings"
)

// iniFile is a parsed AWS shared config / credentials file: section name ->
// lowercase key -> value. It implements just the subset the AWS CLI/SDKs
// define for these files: "[section]" headers, "key = value" pairs, full-line
// comments starting with '#' or ';', and indented continuation lines (nested
// sub-properties such as "s3 =\n  max_concurrent_requests = 10"), which are
// skipped because no key gocode reads is nested.
type iniFile map[string]map[string]string

// parseINI parses the text of an AWS config/credentials file.
func parseINI(text string) iniFile {
	out := iniFile{}
	var cur map[string]string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				cur = nil
				continue
			}
			name := strings.Join(strings.Fields(line[1:end]), " ")
			if out[name] == nil {
				out[name] = map[string]string{}
			}
			cur = out[name]
			continue
		}
		if cur == nil {
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			// Continuation / nested sub-property: ignored.
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		val := strings.TrimSpace(line[eq+1:])
		// Inline comments are only recognised after whitespace (" #" / " ;"),
		// matching botocore; a '#' inside a value (e.g. a secret) is kept.
		for _, marker := range []string{" #", "\t#", " ;", "\t;"} {
			if i := strings.Index(val, marker); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		cur[key] = val
	}
	return out
}

// readINI reads and parses path. A missing file yields (nil, nil).
func readINI(path string) (iniFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseINI(string(b)), nil
}
