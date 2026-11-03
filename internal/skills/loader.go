package skills

import (
	"path"
	"regexp"
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/projectmemory"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// Size and length caps.
const (
	// maxFrontmatterBytes is the untrusted-input cap on the raw YAML block.
	maxFrontmatterBytes = 65536
	// maxDisplayNameLen caps the frontmatter `name`.
	maxDisplayNameLen = 256
	// skillFileName is the per-skill manifest basename.
	skillFileName = "SKILL.md"
	// skillsDirName is the per-root skills subdirectory.
	skillsDirName = "skills"
	// gocodeDirName is the project config directory.
	gocodeDirName = ".gocode"
	// claudeDirName is Claude Code's config directory. gocode<->Claude
	// interop: gocode reads skills that Claude Code
	// installed under <cwd>/.claude/skills and <home>/.claude/skills so they are
	// discoverable alongside gocode's own skills.
	claudeDirName = ".claude"
)

// ParseResult is the output of parsing a SKILL.md file.
type ParseResult struct {
	// Frontmatter is the parsed, normalized frontmatter (zero value when the
	// file has no frontmatter block).
	Frontmatter Frontmatter
	// Content is the body after the frontmatter block (the full file when no
	// frontmatter is present).
	Content string
	// ParseError, when non-empty, explains why the frontmatter was ignored
	// (too long/nested, or malformed); Content is still returned.
	ParseError string
}

// ParseSkillMD parses a SKILL.md file's bytes. The filename is used only to
// default the display name.
func ParseSkillMD(data []byte, filePath string) ParseResult {
	doc := frontmatter.ParseDocument(string(data)) // strips the BOM
	if !doc.HasFrontmatter {
		return ParseResult{Content: doc.Body}
	}
	block, body := doc.Block, doc.Body

	// Untrusted guard.
	if len(block) > maxFrontmatterBytes || containsYAMLAnchor(block) {
		return ParseResult{
			Content:    body,
			ParseError: "the frontmatter is too long, too nested, or may hold a YAML anchor",
		}
	}
	if doc.Err != nil {
		return ParseResult{Content: body, ParseError: doc.Err.Error()}
	}
	return ParseResult{Frontmatter: normalizeFrontmatter(doc.Data), Content: body}
}

// containsYAMLAnchor is the conservative anchor/alias guard: a
// bare & or * at the start of a value can define/reference a YAML anchor, which
// the loader refuses to expand for untrusted input.
func containsYAMLAnchor(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		t := strings.TrimSpace(line)
		if i := strings.Index(t, ": "); i >= 0 {
			v := strings.TrimSpace(t[i+2:])
			if strings.HasPrefix(v, "&") || strings.HasPrefix(v, "*") {
				return true
			}
		}
	}
	return false
}

// normalizeFrontmatter maps the raw key/value map onto the typed Frontmatter,
// applying key normalization and the string-or-list coercion for tool lists.
func normalizeFrontmatter(raw map[string]any) Frontmatter {
	fm := Frontmatter{UserInvocable: true}
	get := func(keys ...string) (any, bool) {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				return v, true
			}
		}
		return nil, false
	}
	fm.Name = capName(asString(first(raw, "name")))
	fm.Description = asString(first(raw, "description"))
	fm.Model = asString(first(raw, "model"))
	fm.AllowedTools = parseToolList(first(raw, "allowed-tools", "allowedTools"))
	fm.DisallowedTools = parseToolList(first(raw, "disallowed-tools", "disallowedTools"))
	fm.ArgumentHint = asString(first(raw, "argument-hint", "argumentHint"))
	fm.Effort = asString(first(raw, "effort"))
	fm.Shell = asString(first(raw, "shell"))
	fm.Version = asString(first(raw, "version"))
	fm.WhenToUse = asString(first(raw, "when_to_use", "whenToUse"))
	// `paths:` reads like a rule file's (comma lists, brace expansion).
	fm.Paths = projectmemory.PathPatterns(first(raw, "paths"))
	fm.Context = strings.ToLower(asString(first(raw, "context")))
	fm.Agent = asString(first(raw, "agent"))
	fm.License = asString(first(raw, "license"))

	if v, ok := get("disable-model-invocation", "disableModelInvocation"); ok {
		fm.DisableModelInvocation = asBool(v)
	}
	if v, ok := get("user-invocable", "userInvocable"); ok {
		fm.UserInvocable = asBool(v)
		fm.userInvocableSet = true
	}
	if v, ok := get("background"); ok {
		fm.Background = asBool(v)
		fm.BackgroundSet = true
	}
	if v, ok := get("hooks"); ok {
		if mm, ok := v.(map[string]any); ok && len(mm) > 0 {
			fm.Hooks = mm
		}
	}
	// A PreToolUse/PermissionRequest hook declared outside `hooks` cannot be
	// loaded, and it may be what guards the permissions declared beside it, so
	// the skill loads with no hooks and no allowed-tools.
	if guardOutsideHooks(raw) {
		fm.Hooks = nil
		fm.AllowedTools = nil
	}
	if v, ok := get("metadata"); ok {
		if mm, ok := v.(map[string]any); ok {
			fm.Metadata = mm
		}
	}
	return fm
}

// hookEventNames are the hook event keys.
var hookEventNames = map[string]bool{
	"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true, "PostToolBatch": true,
	"Notification": true, "UserPromptSubmit": true, "UserPromptExpansion": true, "SessionStart": true,
	"SessionEnd": true, "Stop": true, "StopFailure": true, "SubagentStart": true, "SubagentStop": true,
	"PreCompact": true, "PostCompact": true, "PreModelSwitch": true, "PostModelSwitch": true,
	"PermissionRequest": true, "PermissionDenied": true, "Setup": true, "TeammateIdle": true,
	"TaskCreated": true, "TaskCompleted": true, "Elicitation": true, "ElicitationResult": true,
	"ConfigChange": true, "WorktreeCreate": true, "WorktreeRemove": true, "InstructionsLoaded": true,
	"CwdChanged": true, "FileChanged": true, "DirectoryAdded": true, "MessageDisplay": true,
}

// unscannedSkillKeys are frontmatter keys E8 never descends into.
var unscannedSkillKeys = map[string]bool{"metadata": true, "mcpServers": true, "lspServers": true}

// hasGuardEvent reports whether v is an object with a non-empty PreToolUse or
// PermissionRequest entry.
func hasGuardEvent(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, k := range []string{"PreToolUse", "PermissionRequest"} {
		e, ok := m[k]
		if !ok || e == nil {
			continue
		}
		if arr, ok := e.([]any); ok && len(arr) == 0 {
			continue
		}
		return true
	}
	return false
}

// looksLikeMatcher reports whether v is shaped like a hook matcher group
// .
func looksLikeMatcher(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, hasMatcher := m["matcher"]
	if !hasMatcher {
		for k := range m {
			if hookEventNames[k] {
				return false
			}
		}
	}
	switch h := m["hooks"].(type) {
	case []any:
		return len(h) > 0
	case map[string]any:
		_, typed := h["type"].(string)
		return hasMatcher || typed
	}
	return false
}

// scanGuard is the bounded recursive search for a misplaced guard hook
// .
func scanGuard(v any, depth int, matchersCount, inHooksList bool) bool {
	if hasGuardEvent(v) || (matchersCount && looksLikeMatcher(v)) {
		return true
	}
	if depth == 0 {
		return false
	}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if scanGuard(e, depth-1, matchersCount, inHooksList) {
				return true
			}
		}
	case map[string]any:
		if _, typed := t["type"].(string); inHooksList && typed {
			return false
		}
		for k, e := range t {
			if unscannedSkillKeys[k] {
				continue
			}
			_, isList := e.([]any)
			if scanGuard(e, depth-1, matchersCount && !hookEventNames[k], k == "hooks" && isList) {
				return true
			}
		}
	}
	return false
}

// guardOutsideHooks reports whether skill frontmatter declares a
// PreToolUse/PermissionRequest hook anywhere but under `hooks`.
func guardOutsideHooks(raw map[string]any) bool {
	if hasGuardEvent(raw) {
		return true
	}
	for k, v := range raw {
		if k == "hooks" || unscannedSkillKeys[k] {
			continue
		}
		if scanGuard(v, 3, !hookEventNames[k], false) {
			return true
		}
	}
	return false
}

// BuildDescriptor turns parsed frontmatter + body into a Descriptor for a skill
// discovered at skillPath (its SKILL.md) under the given scope. identifier is
// the directory-derived canonical name before namespacing; pluginID,
// when non-empty, namespaces plugin skills as "<pluginID>:<identifier>".
func BuildDescriptor(pr ParseResult, identifier string, scope Scope, pluginID, skillPath string) Descriptor {
	fm := pr.Frontmatter

	desc := strings.TrimSpace(fm.Description)
	hasUserDesc := desc != ""
	if !hasUserDesc {
		desc = deriveDescriptionFromBody(pr.Content)
	}

	name := identifier
	switch scope {
	case ScopePlugin:
		if pluginID != "" {
			name = pluginID + ":" + identifier
		}
	case ScopeSynced:
		// Reuse the settingspolicy normalizer for the "gocode-skills:"
		// namespace applied to account-synced skills.
		name = settingspolicy.NormalizeSkillName(identifier)
	}

	ctx := ContextInline
	if fm.Context == "fork" {
		ctx = ContextFork
	}

	return Descriptor{
		Name:                        name,
		UnqualifiedName:             identifier,
		DisplayName:                 fm.Name,
		Description:                 desc,
		HasUserSpecifiedDescription: hasUserDesc,
		AllowedTools:                fm.AllowedTools,
		DisallowedTools:             fm.DisallowedTools,
		ArgumentHint:                fm.ArgumentHint,
		WhenToUse:                   fm.WhenToUse,
		Paths:                       fm.Paths,
		Version:                     fm.Version,
		Model:                       fm.Model,
		DisableModelInvocation:      fm.DisableModelInvocation,
		UserInvocable:               fm.UserInvocable,
		ExecutionContext:            ctx,
		Agent:                       fm.Agent,
		Background:                  fm.Background,
		BackgroundSet:               fm.BackgroundSet,
		Hooks:                       fm.Hooks,
		Effort:                      fm.Effort,
		Shell:                       fm.Shell,
		Scope:                       scope,
		Path:                        skillPath,
		BaseDir:                     path.Dir(skillPath),
		Metadata:                    fm.Metadata,
	}
}

// deriveDescriptionFromBody returns the first non-empty line of the body with a
// leading markdown heading marker stripped, truncated to 100 chars.
func deriveDescriptionFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if m := regexp.MustCompile(`^#+\s+(\S.*)$`).FindStringSubmatch(t); m != nil {
			t = m[1]
		}
		if len(t) > 100 {
			return t[:97] + "..."
		}
		return t
	}
	return ""
}

// DiscoverScope walks a skills root directory, parsing one skill per immediate
// subdirectory that contains a SKILL.md, and returns their descriptors. A
// subdirectory without SKILL.md, or whose SKILL.md fails to parse, is skipped.
func DiscoverScope(fsys fsfacade.FS, root string, scope Scope, pluginID string) []Descriptor {
	entries, err := fsys.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Descriptor
	confine := ""
	if scope == ScopeProject || scope == ScopeClaudeProject {
		confine = path.Dir(path.Dir(root))
		if r, err := fsfacade.RealpathVia(fsys, confine); err == nil {
			confine = r
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		identifier := e.Name()
		skillPath := path.Join(root, identifier, skillFileName)
		if confine != "" && !insideDir(fsys, skillPath, confine) {
			// Hardening: a repository's skill may not be a symlink
			// (git preserves them) to a file outside the project, e.g.
			// SKILL.md -> ~/.aws/credentials, whose text would otherwise be
			// loaded into the model's context (description, body).
			continue
		}
		data, err := fsys.ReadFile(skillPath)
		if err != nil {
			continue
		}
		pr := ParseSkillMD(data, skillPath)
		out = append(out, BuildDescriptor(pr, identifier, scope, pluginID, skillPath))
	}
	return out
}

// PersonalSkillsDir returns <userDir>/skills, the personal skills directory
// inside the gocode root (internal/confighome).
func PersonalSkillsDir(userDir string) string {
	return path.Join(userDir, skillsDirName)
}

// ProjectSkillsDir returns <cwd>/.gocode/skills for the given project root.
func ProjectSkillsDir(cwd string) string {
	return path.Join(cwd, gocodeDirName, skillsDirName)
}

// ClaudePersonalSkillsDir returns ~/.claude/skills for the given home
// directory. gocode<->Claude interop: like
// PersonalSkillsDir but points at Claude Code's user skills directory so skills
// Claude Code installed there are discoverable in gocode.
func ClaudePersonalSkillsDir(home string) string {
	return path.Join(home, claudeDirName, skillsDirName)
}

// ClaudeProjectSkillsDir returns <cwd>/.claude/skills for the given project
// root. gocode<->Claude interop: like
// ProjectSkillsDir but points at Claude Code's project skills directory.
func ClaudeProjectSkillsDir(cwd string) string {
	return path.Join(cwd, claudeDirName, skillsDirName)
}

// --- small frontmatter value helpers ---

func first(raw map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			return v
		}
	}
	return nil
}

// asString renders a scalar frontmatter value (string, bool or number, as
// JavaScript's String() would); lists, maps and null yield "".
func asString(v any) string {
	s, _ := frontmatter.Scalar(v)
	return s
}

// asBool is the boolean coercion: true/yes/on/1 are true;

// anything unrecognised is false.
func asBool(v any) bool {
	b, _ := frontmatter.Bool(v)
	return b
}

func capName(s string) string {
	if len(s) > maxDisplayNameLen {
		return s[:maxDisplayNameLen]
	}
	return s
}

// parseToolList coerces a frontmatter tool field (string or list) into a slice.
func parseToolList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return splitInlineList(x)
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := frontmatter.Scalar(e); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		var out []string
		for _, s := range x {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func splitInlineList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = unquote(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// insideDir reports whether p, with symlinks resolved, is dir or below it.
// An unresolvable p (missing, dangling link) is reported inside so the
// normal read path decides what happens to it.
func insideDir(fsys fsfacade.FS, p, dir string) bool {
	r, err := fsfacade.RealpathVia(fsys, p)
	if err != nil {
		return true
	}
	r, dir = comparablePath(r), comparablePath(dir)
	return r == dir || strings.HasPrefix(r, strings.TrimSuffix(dir, "/")+"/")
}

// comparablePath returns p with forward slashes, lexically cleaned, and
// lower-cased on Windows where paths compare case-insensitively.
func comparablePath(p string) string {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
