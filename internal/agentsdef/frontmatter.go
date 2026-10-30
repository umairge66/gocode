package agentsdef

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
)

// ParseResult is the output of splitting an agent .md file into frontmatter and
// body.
type ParseResult struct {
	Frontmatter Frontmatter
	// Body is the markdown after the frontmatter block (the whole file when
	// there is no frontmatter). Descriptor.SystemPrompt is Body trimmed.
	Body string
	// ParseError, when non-empty, explains why frontmatter was ignored.
	ParseError string
}

// ParseDefinition splits an agent .md file into its frontmatter and body. The
// filePath is used only for diagnostics.
func ParseDefinition(data []byte, filePath string) ParseResult {
	doc := frontmatter.ParseDocument(string(data))
	if !doc.HasFrontmatter {
		return ParseResult{Body: doc.Body}
	}
	if doc.Err != nil {
		return ParseResult{Body: doc.Body, ParseError: doc.Err.Error()}
	}
	return ParseResult{Frontmatter: normalizeFrontmatter(doc.Data), Body: doc.Body}
}

// keyAliases maps a normalized (lowercased) alias key to its canonical key.
var keyAliases = map[string]string{
	"allowedtools": "tools",
	"whentouse":    "description",
	"when_to_use":  "description",
	"when-to-use":  "description",
}

// normalizeFrontmatter maps a raw key/value map onto the typed Frontmatter,
// applying the key aliases and the string-or-list coercions.
func normalizeFrontmatter(raw map[string]any) Frontmatter {
	// Fold aliases onto their canonical keys (first writer wins, so a canonical
	// key already present is not overwritten by an alias).
	norm := map[string]any{}
	for k, v := range raw {
		canon := k
		if c, ok := keyAliases[strings.ToLower(k)]; ok {
			canon = c
		}
		if _, exists := norm[canon]; !exists {
			norm[canon] = v
		}
	}

	get := func(keys ...string) (any, bool) {
		for _, k := range keys {
			if v, ok := norm[k]; ok {
				return v, true
			}
		}
		return nil, false
	}
	str := func(keys ...string) string {
		if v, ok := get(keys...); ok {
			s, _ := frontmatter.Scalar(v)
			return s
		}
		return ""
	}

	fm := Frontmatter{}
	fm.Name = str("name")
	fm.Description = strings.ReplaceAll(str("description"), `\n`, "\n")
	if v, ok := get("tools", "allowed-tools"); ok {
		fm.Tools = resolveToolsForFrontmatter(v)
	}
	if v, ok := get("disallowed-tools", "disallowedTools"); ok {
		fm.DisallowedTools = resolveToolsForFrontmatter(v)
	}
	if v, ok := get("skills"); ok {
		fm.Skills = parseSkillsField(v, true)
	}
	fm.Model = str("model")
	fm.Color = str("color")
	fm.Effort = str("effort")
	fm.PermissionMode = str("permissionMode", "permission-mode")
	fm.Memory = str("memory")
	fm.Isolation = str("isolation")
	fm.InitialPrompt = str("initialPrompt", "initial-prompt")
	fm.Observer = str("observer")
	fm.ObserverMessage = str("observerMessage", "observer-message")

	if v, ok := get("background"); ok {
		fm.Background = asBool(v)
	}
	if v, ok := get("omitgocodeMd", "omit-gocode-md"); ok {
		fm.OmitGocodeMd = asBool(v)
	}
	if v, ok := get("observeSubagents", "observe-subagents"); ok {
		b := asBool(v)
		fm.ObserveSubagents = &b
	}
	return fm
}

// resolveToolsForFrontmatter returns nil when the field resolves to "inherit
// all" (a "*" entry) and the explicit trimmed list otherwise. This keeps the
// Frontmatter.Tools nil-vs-nonnil distinction aligned with parseToolsField.
func resolveToolsForFrontmatter(v any) []string {
	list, explicit := parseToolsField(v, true)
	if !explicit {
		return nil
	}
	return list
}

// asBool is the boolean coercion: true/yes/on/1 are true.
func asBool(v any) bool {
	b, _ := frontmatter.Bool(v)
	return b
}

// BuildFileDescriptor turns a parsed agent .md into a Descriptor for the given
// discovery scope. filePath is the file's path; baseDir is the trust/base
// directory the scope's root belongs to. It returns (Descriptor, "") on
// success or (zero, message) when the definition is rejected (missing/invalid
// name or description).
func BuildFileDescriptor(pr ParseResult, filePath, baseDir string, source Source) (Descriptor, string) {
	fm := pr.Frontmatter
	if msg := ValidateName(fm.Name); msg != "" {
		return Descriptor{}, msg
	}
	if strings.TrimSpace(fm.Description) == "" {
		return Descriptor{}, "missing required 'description' in frontmatter"
	}

	d := Descriptor{
		AgentType:        fm.Name,
		WhenToUse:        fm.Description,
		Tools:            fm.Tools,
		DisallowedTools:  fm.DisallowedTools,
		Skills:           fm.Skills,
		SystemPrompt:     strings.TrimSpace(pr.Body),
		Source:           source,
		Filename:         strings.TrimSuffix(path.Base(filePath), ".md"),
		FilePath:         filePath,
		BaseDir:          baseDir,
		Background:       fm.Background,
		OmitGocodeMd:     fm.OmitGocodeMd,
		InitialPrompt:    fm.InitialPrompt,
		Observer:         fm.Observer,
		ObserverMessage:  fm.ObserverMessage,
		ObserveSubagents: fm.ObserveSubagents,
	}

	// model: "inherit" (case-insensitive) stays "inherit"; any other non-blank
	// value is kept trimmed.
	if m := strings.TrimSpace(fm.Model); m != "" {
		if strings.EqualFold(m, "inherit") {
			d.Model = "inherit"
		} else {
			d.Model = m
		}
	}
	// color is kept only if it is a recognized color.
	if oneOf(fm.Color, agentColors) {
		d.Color = fm.Color
	}
	if oneOf(fm.Effort, effortLevels) || isIntString(fm.Effort) {
		d.Effort = fm.Effort
	}
	if oneOf(fm.PermissionMode, permissionModes) {
		d.PermissionMode = fm.PermissionMode
	}
	if oneOf(fm.Memory, memoryScopes) {
		d.Memory = fm.Memory
	}
	if oneOf(fm.Isolation, isolationModes) {
		d.Isolation = fm.Isolation
	}
	return d, ""
}

func isIntString(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '-' && i == 0 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != "-"
}

// jsonAgentSpec is the SDK/JSON agent spec (the value side of the `agents`
// object: {name: spec}).
type jsonAgentSpec struct {
	Description     string   `json:"description"`
	Tools           []string `json:"tools"`
	DisallowedTools []string `json:"disallowedTools"`
	Skills          []string `json:"skills"`
	Prompt          string   `json:"prompt"`
	Model           string   `json:"model"`
	Effort          any      `json:"effort"`
	PermissionMode  string   `json:"permissionMode"`
	Memory          string   `json:"memory"`
	Isolation       string   `json:"isolation"`
	Background      bool     `json:"background"`
	OmitGocodeMd    bool     `json:"omitgocodeMd"`
	InitialPrompt   string   `json:"initialPrompt"`
}

// ParseAgentsJSON parses an SDK `agents` object (name -> spec) into descriptors
// at SourceFlag. Invalid entries are skipped and reported in the returned
// problems slice.
func ParseAgentsJSON(data []byte) (descs []Descriptor, problems []string) {
	var obj map[string]jsonAgentSpec
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, []string{"Error parsing agents from JSON: " + err.Error()}
	}
	for name, spec := range obj {
		if msg := ValidateName(name); msg != "" {
			problems = append(problems, "Agent '"+name+"': "+msg)
			continue
		}
		if strings.TrimSpace(spec.Description) == "" {
			problems = append(problems, "Agent '"+name+"': description is required")
			continue
		}
		d := Descriptor{
			AgentType:       name,
			WhenToUse:       spec.Description,
			DisallowedTools: nilIfEmpty(spec.DisallowedTools),
			Skills:          nilIfEmpty(spec.Skills),
			SystemPrompt:    spec.Prompt,
			Source:          SourceFlag,
			BaseDir:         "flagSettings",
			Background:      spec.Background,
			OmitGocodeMd:    spec.OmitGocodeMd,
			InitialPrompt:   spec.InitialPrompt,
		}
		// tools: a list containing "*" means inherit-all (Tools stays nil).
		if spec.Tools != nil {
			d.Tools = resolveToolsForFrontmatter(spec.Tools)
		}
		if m := strings.TrimSpace(spec.Model); m != "" {
			if strings.EqualFold(m, "inherit") {
				d.Model = "inherit"
			} else {
				d.Model = m
			}
		}
		if e := effortString(spec.Effort); oneOf(e, effortLevels) || isIntString(e) {
			d.Effort = e
		}
		if oneOf(spec.PermissionMode, permissionModes) {
			d.PermissionMode = spec.PermissionMode
		}
		if oneOf(spec.Memory, memoryScopes) {
			d.Memory = spec.Memory
		}
		if oneOf(spec.Isolation, isolationModes) {
			d.Isolation = spec.Isolation
		}
		descs = append(descs, d)
	}
	return descs, problems
}

func effortString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		// JSON numbers decode as float64; render integers without a decimal.
		if x == float64(int64(x)) {
			return strings.TrimSuffix(strings.TrimRight(formatInt(int64(x)), "."), ".")
		}
	}
	return ""
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
