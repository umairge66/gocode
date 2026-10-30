package settingsload

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// maxSettingsBytes caps the size of a settings file that will be read.
const maxSettingsBytes = 2 * 1024 * 1024

func osGetenv(key string) string { return os.Getenv(key) }

// LoadFile reads and parses one settings file, producing the
// settingspolicy.LoadResult the merge engine consumes. The flow:
//
//	missing file (ENOENT)      -> LoadAbsent,     settings {} , no errors
//	other read error           -> LoadUnreadable, settings nil, fatal error
//	oversize (> 2 MiB)         -> LoadUnreadable, settings nil, fatal error
//	empty / whitespace content -> LoadLoaded,     settings {} , no errors
//	invalid JSON               -> LoadUnparsable, settings nil, fatal error
//	valid JSON, not an object  -> LoadMalformed,  settings nil, fatal error
//	object                     -> LoadLoaded,     settings = object
//
// After a file decodes to a JSON object it is additionally run through the
// known-key + basic-type schema check (see schema.go). Any schema violation
// (unknown top-level key, wrong value type) is DOWNGRADED to an advisory
// warning on the result (Warnings) rather than failing the load: the object
// still loads as LoadLoaded and every key is still applied. The schema is
// deliberately advisory, so validation never starts
// rejecting or dropping settings that load today. The deep per-value rules
// (enum membership, nested shapes) remain out of scope; schema.go documents
// the coverage.
func LoadFile(fsys fsfacade.FS, path string) settingspolicy.LoadResult {
	if path == "" {
		return absentResult()
	}

	// Stat first so an oversize regular file is rejected as unreadable rather
	// than slurped.
	if info, err := fsys.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > maxSettingsBytes {
		return unreadableResult(path, fmt.Errorf("settings file exceeds %d bytes", maxSettingsBytes))
	}

	data, err := fsys.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return absentResult()
		}
		return unreadableResult(path, err)
	}
	return ParseContent(path, string(data))
}

// ParseContent runs the parse+validate half of the loader on already-read
// content, independent of any filesystem. path is used only for diagnostics.
func ParseContent(path, content string) settingspolicy.LoadResult {
	// Empty content is a loaded, empty settings object.
	if strings.TrimSpace(content) == "" {
		return settingspolicy.LoadResult{
			Settings:  settingspolicy.Settings{},
			LoadState: settingspolicy.LoadLoaded,
		}
	}

	parsed, err := parseJSONC(content)
	if err != nil {
		return settingspolicy.LoadResult{
			LoadState: settingspolicy.LoadUnparsable,
			Errors: []settingspolicy.LoadError{{
				File:     path,
				Message:  "Settings file is not valid JSON: " + err.Error(),
				Severity: "fatal",
			}},
		}
	}

	obj, isObject := parsed.(map[string]any)
	if !isObject {
		return settingspolicy.LoadResult{
			LoadState: settingspolicy.LoadMalformed,
			Errors: []settingspolicy.LoadError{{
				File:     path,
				Message:  "Settings file must contain a JSON object",
				Severity: "fatal",
			}},
		}
	}

	return settingspolicy.LoadResult{
		Settings:  settingspolicy.Settings(obj),
		LoadState: settingspolicy.LoadLoaded,
		Warnings:  validateSettings(obj),
	}
}

// LoadSource resolves src's path in ctx and loads it. A source with no backing
// file in this context (diskless, no project dir, no --settings path) yields
// LoadAbsent.
//
// The policy source (policySettings, and its managedSettings alias) is not a
// single file: it is the composition of the admin tiers — the OS MDM tier,
// managed-settings.json with its drop-ins, and the HKCU fallback — produced
// by ComposePolicy, so every consumer reading the policy source through here
// sees the composed result.
func LoadSource(fsys fsfacade.FS, ctx *Context, src settingspolicy.Source) settingspolicy.LoadResult {
	if src == settingspolicy.SourcePolicy || src == settingspolicy.SourceManaged {
		return ComposePolicy(fsys, ctx).Result()
	}
	path, ok := ctx.PathForSource(src)
	if !ok {
		return absentResult()
	}
	return LoadFile(fsys, path)
}

// absentResult is the frozen "nothing here" result.
func absentResult() settingspolicy.LoadResult {
	return settingspolicy.LoadResult{
		Settings:  settingspolicy.Settings{},
		LoadState: settingspolicy.LoadAbsent,
	}
}

// unreadableResult builds the fatal "could not be read" diagnostic.
func unreadableResult(path string, err error) settingspolicy.LoadResult {
	return settingspolicy.LoadResult{
		LoadState: settingspolicy.LoadUnreadable,
		Errors: []settingspolicy.LoadError{{
			File:     path,
			Message:  "Settings file could not be read: " + err.Error(),
			Severity: "fatal",
		}},
	}
}

// parseJSONC parses JSON with // and /* */ comments and trailing commas
// stripped: a lenient reader for settings files (settings.json is documented as
// JSONC).
func parseJSONC(input string) (any, error) {
	cleaned := stripTrailingCommas(stripJSONComments(input))
	var out any
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func stripJSONComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++ // skip the closing '/'
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// stripTrailingCommas removes commas that directly precede a closing } or ]
// (ignoring whitespace), outside of string literals.
func stripTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // drop the trailing comma
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
