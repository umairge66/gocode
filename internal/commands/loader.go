package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/frontmatter"
	toolexec "github.com/umairge66/gocode/internal/tools/exec"
)

// maxCommandFileBytes is the per-file size cap for a command markdown file.
const maxCommandFileBytes = 1 << 20

// maxFrontmatterBytes caps the frontmatter block.
const maxFrontmatterBytes = 1 << 16

// Frontmatter holds the parsed YAML frontmatter of a command markdown file.
//
// Recognised keys and how each is coerced:
//
//	name                      string (String()); over 256 chars -> ignored + warning
//	description               string trimmed; number/bool stringified; else warning
//	model                     string, trimmed; "inherit" case-folded
//	allowed-tools             string or list, split on commas/spaces outside (...)
//	disallowed-tools          same; alias disallowedTools
//	argument-hint             String()
//	arguments                 list, or whitespace-split string; numeric names dropped
//	disable-model-invocation  boolean-ish (true/yes/on/1, false/no/off/0)
//	user-invocable            boolean-ish; default true
//	effort                    low|medium|high|xhigh|max ("med" alias) or an integer
//	shell                     bash|powershell (+ "pwsh" alias; also sh, zsh)
//	when_to_use, version      String()
//
// An invalid effort or shell is dropped with a warning (the command still
// loads with the default).
type Frontmatter struct {
	Name                   string
	Description            string
	Model                  string
	AllowedTools           []string
	DisallowedTools        []string
	ArgumentHint           string
	Arguments              []string // @internal typed variant of argument-hint
	DisableModelInvocation *bool
	UserInvocable          *bool
	Effort                 string
	// Shell is the normalised `shell:` value ("" when absent or invalid).
	Shell     string
	WhenToUse string
	Version   string
	// ParseError is set when the block was not valid YAML (even after the
	// lenient retry). The command still loads with default frontmatter.
	ParseError string
	// Warnings are non-fatal problems found while reading the keys.
	Warnings []string
	// raw is the parsed frontmatter mapping as written (keys not folded); the
	// plugin-only hooks checks (pluginhooks.go) scan it.
	raw map[string]any
}

// ParseFrontmatter splits a markdown document into its YAML frontmatter and
// body using the shared YAML parser (internal/frontmatter).
//
// The frontmatter block is a leading `---` fenced block; a leading
// UTF-8 BOM is stripped; CRLF is accepted. When there is no fence,
// the whole input is the body and Frontmatter is zero. When the block is
// malformed the body is still returned, Frontmatter keeps its defaults and
// ParseError carries the line-numbered error; the file still loads.
func ParseFrontmatter(doc string) (Frontmatter, string) {
	d := frontmatter.ParseDocument(doc)
	if !d.HasFrontmatter {
		return Frontmatter{}, d.Body
	}
	if len(d.Block) > maxFrontmatterBytes {
		return Frontmatter{}, d.Body
	}
	if d.Err != nil {
		return Frontmatter{ParseError: d.Err.Error()}, d.Body
	}
	fm := frontmatterFromMap(d.Data)
	fm.raw = d.Data
	if d.Problem != "" {
		fm.Warnings = append(fm.Warnings, "frontmatter "+d.Problem)
	}
	return fm, d.Body
}

// maxNameLen caps the frontmatter `name`.
const maxNameLen = 256

// effortLevels are the valid named effort levels.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// validShells is the recognised `shell:` value set.
var validShells = []string{"bash", "powershell", "sh", "zsh"}

// frontmatterFromMap applies the key handling to a parsed mapping.
// Keys match case-insensitively; the camelCase aliases allowedTools /
// disallowedTools are accepted.
func frontmatterFromMap(raw map[string]any) Frontmatter {
	m := make(map[string]any, len(raw))
	for k, v := range raw {
		m[strings.ToLower(k)] = v
	}
	get := func(keys ...string) (any, bool) {
		for _, k := range keys {
			if v, ok := m[k]; ok && v != nil {
				return v, true
			}
		}
		return nil, false
	}
	var fm Frontmatter
	warn := func(format string, a ...any) { fm.Warnings = append(fm.Warnings, fmt.Sprintf(format, a...)) }

	if v, ok := get("name"); ok {
		if n := frontmatter.String(v); len(n) <= maxNameLen {
			fm.Name = n
		} else {
			warn("frontmatter \"name\" is over %d characters - ignoring it", maxNameLen)
		}
	}
	if v, ok := get("description"); ok {
		if s, isScalar := frontmatter.Scalar(v); isScalar {
			fm.Description = strings.TrimSpace(s)
		} else {
			warn("description invalid - omitting")
		}
	}
	if v, ok := get("model"); ok {
		if s, isStr := v.(string); isStr {
			fm.Model = strings.TrimSpace(s)
		}
	}
	if v, ok := get("allowed-tools", "allowedtools"); ok {
		fm.AllowedTools, _ = frontmatter.ToolList(v)
	}
	if v, ok := get("disallowed-tools", "disallowedtools"); ok {
		fm.DisallowedTools, _ = frontmatter.ToolList(v)
	}
	if v, ok := get("argument-hint"); ok {
		fm.ArgumentHint = frontmatter.String(v)
	}
	if v, ok := get("arguments"); ok {
		fm.Arguments = argumentNames(v)
	}
	if v, ok := get("disable-model-invocation"); ok {
		b, _ := frontmatter.Bool(v)
		fm.DisableModelInvocation = &b
	}
	if v, ok := get("user-invocable"); ok {
		b, _ := frontmatter.Bool(v)
		fm.UserInvocable = &b
	}
	if v, ok := get("effort"); ok {
		if e, valid := normalizeEffort(v); valid {
			fm.Effort = e
		} else {
			warn("invalid effort '%s'. Valid options: %s or an integer", frontmatter.String(v), strings.Join(effortLevels, ", "))
		}
	}
	if v, ok := get("shell"); ok {
		raw := strings.TrimSpace(frontmatter.String(v))
		if k, valid := toolexec.NormalizeShellKind(raw); valid {
			fm.Shell = k
		} else {
			warn("Frontmatter 'shell: %s' is not recognized. Valid values: %s. Falling back to bash.", raw, strings.Join(validShells, ", "))
		}
	}
	if v, ok := get("when_to_use"); ok {
		fm.WhenToUse = frontmatter.String(v)
	}
	if v, ok := get("version"); ok {
		fm.Version = frontmatter.String(v)
	}
	return fm
}

// normalizeEffort validates an effort value.
func normalizeEffort(v any) (string, bool) {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return "", false
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		if s == "" {
			return "", true
		}
		if s == "med" {
			s = "medium"
		}
		for _, l := range effortLevels {
			if s == l {
				return s, true
			}
		}
		if n, err := strconv.Atoi(s); err == nil {
			return strconv.Itoa(n), true
		}
	}
	return "", false
}

// argumentNames normalises the `arguments` key: a list keeps its non-blank, non-numeric
// string entries; a string is split on whitespace.
func argumentNames(v any) []string {
	var in []string
	switch x := v.(type) {
	case string:
		in = strings.Fields(x)
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				in = append(in, s)
			}
		}
	}
	return in
}

// NamespacedName builds a command's fully-qualified name from its file path,
// its commands-root base directory, and its scope prefix.
//
// For a non-skill markdown file: name = `${scope}` then the file's directory
// path relative to base (segments joined by ':'), then the basename without
// ".md". E.g. base=".../commands", scope="project",
// file=".../commands/git/commit.md" -> "project:git:commit".
func NamespacedName(filePath, baseDir, scope string) string {
	dir := filepath.Dir(filePath)
	base := strings.TrimSuffix(filepath.Base(filePath), ".md")
	rel := ""
	if strings.HasPrefix(dir, baseDir) {
		rel = strings.TrimLeft(dir[len(baseDir):], `/\`)
	}
	sub := ""
	if rel != "" {
		sub = strings.Join(splitPathSegments(rel), ":")
	}
	if sub != "" {
		return scope + ":" + sub + ":" + base
	}
	return scope + ":" + base
}

func splitPathSegments(rel string) []string {
	return strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
}

// scopeForSource maps a Source to the scope prefix used in command names.
func scopeForSource(src Source) string {
	switch src {
	case SourceProject:
		return "project"
	case SourceUser:
		return "user"
	case SourceLocal:
		return "local"
	default:
		return string(src)
	}
}

// BuildCommand constructs a Command descriptor from a parsed markdown file
// (the project/user path).
//
// The description falls back to the body's first heading/line when the
// frontmatter omits it. user-invocable defaults to true; shell
// defaults to "bash"; disable-model-invocation defaults to false.
//
// Frontmatter warnings (and a parse failure, which leaves every key at its
// default) are attached to Command.Warnings prefixed with the file path; the
// command always loads.
//
// The Body is retained verbatim so a caller can run Expand +
// ExpandShellBlocks over it. Plugin marketplace attestation
// (officiallyAttested) relies on gocode's hosted registry and is not
// supported.
//
// For SourcePlugin, the plugin-only steps run as well (pluginhooks.go): the
// hooks validation that drops a skill's hooks and
// allowed-tools when a guard hook cannot be loaded. BuildPluginCommand also
// takes the session facts and the sanitized npm provenance.
func BuildCommand(filePath, baseDir string, src Source, fm Frontmatter, body string) *Command {
	if src == SourcePlugin {
		return BuildPluginCommand(filePath, baseDir, fm, body, PluginCommandOptions{})
	}
	return buildCommand(filePath, baseDir, src, fm, body)
}

// BuildPluginCommand builds a plugin command or skill (SourcePlugin) and runs
// the plugin-only checks with the given options.
func BuildPluginCommand(filePath, baseDir string, fm Frontmatter, body string, opts PluginCommandOptions) *Command {
	c := buildCommand(filePath, baseDir, SourcePlugin, fm, body)
	applyPluginChecks(c, fm.raw, opts)
	return c
}

func buildCommand(filePath, baseDir string, src Source, fm Frontmatter, body string) *Command {
	scope := scopeForSource(src)
	name := NamespacedName(filePath, baseDir, scope)
	if fm.Name != "" {
		// An explicit frontmatter name overrides the basename-derived name but
		// keeps the scope/namespace prefix (the scope is prefixed when it is
		// not already present).
		prefix := name[:strings.LastIndexByte(name, ':')+1]
		if strings.Contains(fm.Name, ":") {
			name = fm.Name
		} else {
			name = prefix + fm.Name
		}
	}

	description := fm.Description
	hasDesc := description != ""
	if !hasDesc {
		description = firstMeaningfulLine(body, "Custom command")
	}

	userInvocable := true
	if fm.UserInvocable != nil {
		userInvocable = *fm.UserInvocable
	}
	disableModel := false
	if fm.DisableModelInvocation != nil {
		disableModel = *fm.DisableModelInvocation
	}
	shell := fm.Shell
	shellExplicit := shell != ""
	if shell == "" {
		shell = "bash"
	}
	progress := "running"

	var warnings []string
	if fm.ParseError != "" {
		warnings = append(warnings, fmt.Sprintf("YAML frontmatter in %s failed to parse and was ignored: %s", filePath, fm.ParseError))
	}
	for _, w := range fm.Warnings {
		warnings = append(warnings, filePath+": "+w)
	}

	return &Command{
		Type:                        "prompt",
		Name:                        name,
		Description:                 description,
		HasUserSpecifiedDescription: hasDesc,
		AllowedTools:                fm.AllowedTools,
		DisallowedTools:             fm.DisallowedTools,
		ArgumentHint:                fm.ArgumentHint,
		ArgNames:                    nonNumericNames(fm.Arguments),
		Model:                       normalizeModel(fm.Model),
		Effort:                      fm.Effort,
		Shell:                       shell,
		ShellExplicit:               shellExplicit,
		Warnings:                    warnings,
		WhenToUse:                   fm.WhenToUse,
		Version:                     fm.Version,
		DisableModelInvocation:      disableModel,
		UserInvocable:               userInvocable,
		Hidden:                      !userInvocable,
		Source:                      src,
		ProgressMessage:             progress,
		FilePath:                    filePath,
		BaseDir:                     baseDir,
		Body:                        body,
	}
}

// normalizeModel lowercases "inherit" and trims.
func normalizeModel(m string) string {
	m = strings.TrimSpace(m)
	if strings.EqualFold(m, "inherit") {
		return "inherit"
	}
	return m
}

// nonNumericNames drops empty and purely-numeric argument names.
func nonNumericNames(in []string) []string {
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || isAllDigits(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// firstMeaningfulLine returns the first non-blank line of the body, stripping a
// leading markdown heading marker, or fallback when the body is empty.
func firstMeaningfulLine(body, fallback string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if m := headingText(t); m != "" {
			t = m
		}
		if r := []rune(t); len(r) > 100 {
			t = string(r[:97]) + "..."
		}
		return t
	}
	return fallback
}

// headingText returns the text after a leading "#"+ run, or "" when t is not a
// heading, matching /^#+\s+(\S.*)$/.
func headingText(t string) string {
	i := 0
	for i < len(t) && t[i] == '#' {
		i++
	}
	if i == 0 || i >= len(t) {
		return ""
	}
	rest := strings.TrimLeft(t[i:], " \t")
	if rest == "" || t[i] != ' ' && t[i] != '\t' {
		return ""
	}
	return rest
}

// LoadDir scans a commands root directory for *.md files (recursively) and
// builds a Command for each, tagging them with the given source.
//
// Walker semantics: symlinks are followed (to files and to
// directories); a directory already visited (same device+inode, e.g. a
// circular symlink) is skipped; unreadable entries are skipped. Files over the
// size cap are skipped. The same file reached through two paths (a symlink or
// hard link) is loaded once, first path wins (`${dev}:${ino}` dedup:
// "Skipping duplicate file ... (same inode already loaded ...)").
func LoadDir(root string, src Source) ([]*Command, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	var cmds []*Command
	// Hardening: a project command (<project>/.gocode/commands or
	// .claude/commands, which a cloned repository controls) may not be a
	// symlink — git preserves them — to a file outside the project, e.g.
	// deploy.md -> ~/.aws/credentials, whose text would be loaded as the
	// command body and listed in the model's context.
	confine := ""
	if src == SourceProject {
		confine = filepath.Dir(filepath.Dir(root))
		if r, err := filepath.EvalSymlinks(confine); err == nil {
			confine = r
		}
	}
	for _, path := range walkMarkdown(root) {
		if confine != "" {
			if r, err := filepath.EvalSymlinks(path); err == nil {
				if rel, err := filepath.Rel(confine, r); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					continue
				}
			}
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxCommandFileBytes {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fm, body := ParseFrontmatter(string(data))
		if c := BuildCommand(path, root, src, fm, body); c != nil {
			cmds = append(cmds, c)
		}
	}
	return DedupeByFile(cmds), nil
}

// walkMarkdown lists *.md files under root, following symlinks and skipping
// directories already visited (by os.SameFile), in lexical order.
func walkMarkdown(root string) []string {
	var out []string
	var visited []os.FileInfo
	var walk func(dir string)
	walk = func(dir string) {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return
		}
		for _, v := range visited {
			if os.SameFile(v, st) {
				return // circular symlink / already visited
			}
		}
		visited = append(visited, st)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.Type()&os.ModeSymlink != 0 {
				target, err := os.Stat(p)
				if err != nil {
					continue // dangling link
				}
				if target.IsDir() {
					walk(p)
				} else if target.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
					out = append(out, p)
				}
				continue
			}
			if e.IsDir() {
				walk(p)
			} else if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
				out = append(out, p)
			}
		}
	}
	walk(root)
	return out
}

// DedupeByFile drops commands whose FilePath names a file already seen (same
// underlying file per os.SameFile, i.e. reached via a symlink or hard link),
// keeping the first. A Warnings note is added to the kept command. Commands
// without a FilePath, or whose file cannot be stat'd, are always kept. Use it
// when merging the command lists of several roots.
func DedupeByFile(cmds []*Command) []*Command {
	type seen struct {
		info os.FileInfo
		cmd  *Command
	}
	var kept []seen
	out := cmds[:0:0]
outer:
	for _, c := range cmds {
		if c == nil || c.FilePath == "" {
			out = append(out, c)
			continue
		}
		st, err := os.Stat(c.FilePath)
		if err != nil {
			out = append(out, c)
			continue
		}
		for _, k := range kept {
			if os.SameFile(k.info, st) {
				k.cmd.Warnings = append(k.cmd.Warnings, fmt.Sprintf("Skipping duplicate file '%s' from %s (same inode already loaded from %s)", c.FilePath, c.Source, k.cmd.Source))
				continue outer
			}
		}
		kept = append(kept, seen{st, c})
		out = append(out, c)
	}
	return out
}
