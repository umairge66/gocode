package configimport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
)

// Cursor importer: brings Cursor config into Gocode.
//
// Source layout (user scope rooted at ~/.cursor, project scope at <cwd>/.cursor):
//
//	mcp.json          — JSONC: mcpServers                → mcpServers entries
//	rules/*.mdc       — rules with YAML frontmatter      → memory / scoped rule / skill
//	skills/<name>/    — skills (SKILL.md dirs)           → <gocode root>/skills/<name>/
//	commands/*.md     — slash commands                   → <gocode root>/commands/*.md
//	hooks.json        — Cursor hooks                     → (unmappable; event names differ)
//	.cursorrules      — legacy project rules             → appended to gocode.md
//	.agents/skills/   — shared skills                    → skills
//
// Rule mapping (rules/*.mdc), by frontmatter:
//
//	alwaysApply: true → instructions appended to gocode.md
//	globs: [...]      → a scoped rule file (frontmatter `paths:`)
//	otherwise         → a Gocode skill (a description → agent-requested; none →
//	                    manual, disable-model-invocation)

type cursorImporter struct{}

func (cursorImporter) ID() Source          { return SourceCursor }
func (cursorImporter) DisplayName() string { return "Cursor" }

// cursorHome returns the Cursor config dir.
func cursorHome(opts Options) string { return opts.sourceHome(".cursor") }

const (
	cursorMaxServers  = 100
	cursorMaxRuleDirs = 4 // rule folder depth
	cursorMaxRules    = 200
	cursorMaxSkillDir = 3 // skill folder depth
	cursorMaxSkills   = 100
	cursorMaxCommands = 200
)

func (cursorImporter) Detect(opts Options) (bool, error) {
	if opts.detectRedirected() {
		return true, nil
	}
	home := cursorHome(opts)
	cwd := opts.cwd()
	if fileExists(filepath.Join(home, "mcp.json")) ||
		fileExists(filepath.Join(home, "commands")) ||
		fileExists(filepath.Join(home, "rules")) ||
		fileExists(filepath.Join(home, "skills")) ||
		fileExists(filepath.Join(cwd, ".cursorrules")) ||
		fileExists(filepath.Join(cwd, ".cursor")) {
		return true, nil
	}
	return false, nil
}

func (c cursorImporter) Import(opts Options) (ImportResult, error) {
	res := ImportResult{SourceID: SourceCursor, DisplayName: c.DisplayName()}
	home := cursorHome(opts)
	cwd := opts.cwd()
	gocode := opts.gocodeDir()

	if opts.userScopeRedirected() {
		res.unmap(ScopeUser, "Cursor user-scope config", redirectedReason)
		opts.Scope = ScopeProject
	}
	if opts.scopeAllows(ScopeUser) {
		claimed := newCollisionSet()
		c.scanMCP(filepath.Join(home, "mcp.json"), ScopeUser, "~/.cursor/mcp.json", cwd, opts, &res)
		c.scanCommands(filepath.Join(home, "commands"), ScopeUser, "~/.cursor/commands", cwd, opts, claimed, &res)
		// User rules land in <config>/rules, where projectmemory reads them.
		c.scanRules(ruleScan{
			dir: filepath.Join(home, "rules"), scope: ScopeUser, cwd: cwd,
			gocodeMd:  filepath.Join(opts.configDir(), "gocode.md"),
			rulesOut:  filepath.Join(opts.configDir(), "rules"),
			skillsOut: filepath.Join(gocode, "skills"),
			label:     "~/.cursor/rules", claimed: claimed,
		}, &res)
		for _, pair := range [][2]string{
			{filepath.Join(home, "skills"), "~/.cursor/skills"},
			{filepath.Join(filepath.Dir(home), ".agents", "skills"), "~/.agents/skills"},
		} {
			c.scanSkills(skillScan{
				root: pair[0], scope: ScopeUser, cwd: cwd,
				skillsOut: filepath.Join(gocode, "skills"),
				label:     pair[1], claimed: claimed,
			}, &res)
		}
		c.scanHooks(filepath.Join(home, "hooks.json"), ScopeUser, "~/.cursor/hooks.json", &res)
	}

	projDotCursor := filepath.Join(cwd, ".cursor")
	if opts.scopeAllows(ScopeProject) && projDotCursor != home {
		claimed := newCollisionSet()
		if cursorRulesPath := filepath.Join(cwd, ".cursorrules"); fileExists(cursorRulesPath) {
			res.add(ImportItem{
				ID:                "cursor:project:cursorrules",
				Kind:              KindInstructions,
				Scope:             ScopeProject,
				Label:             ".cursorrules",
				Fingerprint:       cursorRulesPath,
				SourcePath:        cursorRulesPath,
				TargetPath:        filepath.Join(cwd, "gocode.md"),
				AppendInstruction: true,
				applyKind:         applyAppend,
				marker:            "cursor:project:cursorrules",
				containIn:         cwd,
			})
		}
		if containedNoSymlink(cwd, projDotCursor) == "" {
			if fileExists(projDotCursor) {
				res.unmap(ScopeProject, ".cursor/", "Is (or is under) a symlink — skipping project-scope read for safety.")
			}
			c.scanMisc(home, cwd, opts, &res)
			return res, nil
		}
		c.scanRules(ruleScan{
			dir: filepath.Join(projDotCursor, "rules"), scope: ScopeProject, cwd: cwd,
			gocodeMd:  filepath.Join(cwd, "gocode.md"),
			rulesOut:  filepath.Join(cwd, ".gocode", "rules"),
			skillsOut: filepath.Join(cwd, ".gocode", "skills"),
			label:     ".cursor/rules", claimed: claimed,
		}, &res)
		c.scanMCP(filepath.Join(projDotCursor, "mcp.json"), ScopeProject, ".cursor/mcp.json", cwd, opts, &res)
		c.scanCommands(filepath.Join(projDotCursor, "commands"), ScopeProject, ".cursor/commands", cwd, opts, claimed, &res)
		c.scanSkills(skillScan{
			root: filepath.Join(projDotCursor, "skills"), scope: ScopeProject, cwd: cwd,
			skillsOut: filepath.Join(cwd, ".gocode", "skills"),
			label:     ".cursor/skills", claimed: claimed,
		}, &res)
		c.scanHooks(filepath.Join(projDotCursor, "hooks.json"), ScopeProject, ".cursor/hooks.json", &res)
		c.scanSkills(skillScan{
			root: filepath.Join(cwd, ".agents", "skills"), scope: ScopeProject, cwd: cwd,
			skillsOut: filepath.Join(cwd, ".gocode", "skills"),
			label:     ".agents/skills", claimed: claimed,
		}, &res)
	}

	c.scanMisc(home, cwd, opts, &res)
	return res, nil
}

// --- MCP -------------------------------------------------------------------

var (
	reCursorEnvRef   = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)
	rePureEnvRef     = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*(?::-)?\}$`)
	reBearerEnvRef   = regexp.MustCompile(`(?i)^(?:(?:Bearer|Basic|Token) )?\$\{[A-Za-z_][A-Za-z0-9_]*(?::-)?\}$`)
	reSanitizeEnvVar = regexp.MustCompile(`[^A-Za-z0-9_]`)
)

const cursorImportEnvPrefix = "CURSOR_IMPORT_"

// expandCursorVars expands ${env:VAR} → ${VAR}; ${workspaceFolder} → cwd.
func expandCursorVars(s, workspace string) string {
	s = reCursorEnvRef.ReplaceAllString(s, "${$1}")
	return strings.ReplaceAll(s, "${workspaceFolder}", workspace)
}

func expandCursorMap(m map[string]string, workspace string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expandCursorVars(v, workspace)
	}
	return out
}

// sanitizeEnvVar turns a string into a valid environment variable name.
func sanitizeEnvVar(n string) string {
	e := reSanitizeEnvVar.ReplaceAllString(n, "_")
	// A leading digit gets a "_" prefix.
	if len(e) > 0 && e[0] >= '0' && e[0] <= '9' {
		e = "_" + e
	}
	e = strings.ToUpper(e)
	if e == "" {
		return "_"
	}
	return e
}

// transportForURL picks the transport for a URL: a /sse path → sse, else http.
func transportForURL(url string) mcp.TransportType {
	if regexp.MustCompile(`/sse(?:$|[?#])`).MatchString(url) {
		return mcp.TransportSSE
	}
	return mcp.TransportHTTP
}

type cursorMapResult struct {
	config       mcp.ServerConfig
	placeholders []string
	dropped      int
	refused      string
}

// mapCursorServer maps a Cursor server: expand vars, enforce the project/user env-var
// policy, and placeholder literal secrets for user scope.
func mapCursorServer(entry map[string]any, workspace string, scope Scope, name string, used map[string]bool) cursorMapResult {
	command, _ := entry["command"].(string)
	url, _ := entry["url"].(string)
	args := toStringSlice(entry["args"])
	env := toStringMap(entry["env"])
	headers := toStringMap(entry["headers"])

	expArgs := make([]string, len(args))
	for i, a := range args {
		expArgs[i] = expandCursorVars(a, workspace)
	}
	expCommand := expandCursorVars(command, workspace)
	expURL := expandCursorVars(url, workspace)
	expEnv := expandCursorMap(env, workspace)
	expHeaders := expandCursorMap(headers, workspace)

	if scope == ScopeProject {
		for _, v := range append([]string{expURL, expCommand}, expArgs...) {
			if v != "" && strings.Contains(v, "${") {
				return cursorMapResult{refused: "Its url, command or args reference an environment variable; on connect gocode would expand it into a repo-authored command or url. Add it manually with `gocode mcp add` if you trust this repo."}
			}
		}
		dropped := len(expEnv) + len(expHeaders)
		var cfg mcp.ServerConfig
		if expURL != "" {
			cfg = mcp.ServerConfig{Type: transportForURL(expURL), URL: expURL}
		} else {
			cfg = mcp.ServerConfig{Type: mcp.TransportStdio, Command: expCommand, Args: expArgs}
		}
		return cursorMapResult{config: cfg, dropped: dropped}
	}

	// user scope
	for _, v := range append([]string{url, command}, args...) {
		if strings.Contains(reCursorEnvRef.ReplaceAllString(v, "${$1}"), "${workspaceFolder}") {
			return cursorMapResult{refused: "References `${workspaceFolder}` — per-workspace in Cursor, but gocode's user-scope MCP servers are global. Importing would pin it to this checkout and run it in every project. Add it as a project-scope server (in the specific project's .mcp.json) instead."}
		}
	}

	var droppedCount int
	var keptEnv, keptHeaders map[string]string
	if expURL != "" {
		// http/sse: keep headers, drop env.
		droppedCount = len(expEnv)
		keptHeaders = expHeaders
	} else {
		// stdio: keep env, drop headers.
		droppedCount = len(expHeaders)
		keptEnv = expEnv
	}

	placeholders, placeholderedEnv, placeholderedHeaders := placeholderSecrets(name, keptEnv, keptHeaders, used)

	var cfg mcp.ServerConfig
	if expURL != "" {
		cfg = mcp.ServerConfig{Type: transportForURL(expURL), URL: expURL, Headers: placeholderedHeaders}
	} else {
		cfg = mcp.ServerConfig{Type: mcp.TransportStdio, Command: expCommand, Args: expArgs, Env: placeholderedEnv}
	}
	return cursorMapResult{config: cfg, placeholders: placeholders, dropped: droppedCount}
}

// placeholderSecrets replaces literal env/header values that aren't pure
// ${VAR} references with CURSOR_IMPORT_<server>_<key> placeholders.
func placeholderSecrets(server string, env, headers map[string]string, used map[string]bool) (names []string, outEnv, outHeaders map[string]string) {
	mk := func(key string) string {
		base := cursorImportEnvPrefix + sanitizeEnvVar(server) + "_" + sanitizeEnvVar(key)
		candidate := base
		for i := 2; used[candidate]; i++ {
			candidate = base + "_" + itoa(i)
		}
		used[candidate] = true
		names = append(names, candidate)
		return candidate
	}
	if env != nil {
		outEnv = make(map[string]string, len(env))
		for _, k := range sortStringKeys(env) {
			v := env[k]
			if rePureEnvRef.MatchString(v) {
				outEnv[k] = v
			} else {
				outEnv[k] = "${" + mk(k) + "}"
			}
		}
	}
	if headers != nil {
		outHeaders = make(map[string]string, len(headers))
		for _, k := range sortStringKeys(headers) {
			v := headers[k]
			if reBearerEnvRef.MatchString(v) {
				outHeaders[k] = v
			} else {
				outHeaders[k] = "${" + mk(k) + "}"
			}
		}
	}
	return names, outEnv, outHeaders
}

func (cursorImporter) scanMCP(path string, scope Scope, label, cwd string, opts Options, res *ImportResult) {
	raw, err := readFileLimited(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		res.unmap(scope, label, "Could not read. Review your MCP servers there manually.")
		return
	}
	var parsed map[string]any
	if err := parseJSONC(raw, &parsed); err != nil {
		res.unmap(scope, label, "Could not parse (unexpected structure). Review your MCP servers there manually.")
		return
	}
	servers := asNamedTable(parsed["mcpServers"])
	if servers == nil {
		if parsed["mcpServers"] != nil {
			res.unmap(scope, label, "Has an unexpected shape in "+label+" (expected an object of named entries). Review it manually.")
		}
		return
	}
	workspace := cwd
	if scope == ScopeProject {
		workspace = "."
	}
	names := sortedKeys(servers)
	if len(names) > cursorMaxServers {
		res.unmap(scope, label, "Has more MCP servers than the import scans. Review the remaining servers manually.")
		names = names[:cursorMaxServers]
	}
	used := map[string]bool{}
	for _, name := range names {
		entry, ok := servers[name].(map[string]any)
		if !ok {
			res.unmap(scope, `MCP server "`+name+`"`, "Entry in "+label+" has an unexpected shape. Review it manually.")
			continue
		}
		command, _ := entry["command"].(string)
		url, _ := entry["url"].(string)
		if command == "" && url == "" {
			res.unmap(scope, `MCP server "`+name+`"`, "Has neither a url nor a command — nothing to import. Review it manually.")
			continue
		}
		safe := safeName(name)
		mr := mapCursorServer(entry, workspace, scope, name, used)
		if mr.refused != "" {
			res.unmap(scope, `MCP server "`+name+`"`, mr.refused)
			continue
		}
		cfg := mr.config
		var suffix string
		if n := len(mr.placeholders); n > 0 {
			suffix = " — set " + itoa(n) + " " + pluralize(n, "env var", "env vars") + " " + strings.Join(mr.placeholders, ", ") +
				" (literal " + pluralize(n, "value", "values") + " in " + label + " not copied)"
		}
		if mr.dropped > 0 {
			suffix += " — " + itoa(mr.dropped) + " " + pluralize(mr.dropped, "env/header entry", "env/header entries") +
				" dropped (see " + label + "); re-add auth manually if needed"
		}
		item := ImportItem{
			ID:             "cursor:" + string(scope) + ":mcp:" + name,
			Kind:           KindMCP,
			Scope:          scope,
			Label:          `MCP server "` + name + `"`,
			Description:    firstNonEmpty(url, command),
			Fingerprint:    jsonFingerprint(cfg),
			MCP:            &cfg,
			MCPTargetScope: scope,
			TargetPath:     mcpTargetPath(scope, cwd, safe),
			applyKind:      applyMCP,
			msgName:        safe,
			msgSuffix:      suffix,
		}
		switch {
		case scope == ScopeProject:
			item.Warning = "Repo-authored MCP server — connecting runs its command or sends requests to its url. Only import if you trust this repo."
		case cursorServerLooksCredentialed(entry):
			item.Warning = "Its url or args look like they carry a literal credential, which /import copies as-is into your gocode config. Move the value into an environment variable (`${env:VAR}`) in " + label + " first, or add the server manually with `gocode mcp add`."
		}
		res.add(item)
	}
}

// cursorServerLooksCredentialed is a coarse check: does a url/arg look like
// it embeds a literal secret? (Used only to attach a warning.)
func cursorServerLooksCredentialed(entry map[string]any) bool {
	pair := func(k, v string) bool {
		if reSecretName.MatchString(k) && looksSecretValue(v) {
			return true
		}
		if m := reHeaderKV.FindStringSubmatch(v); m != nil && reSecretName.MatchString(m[1]) && looksSecretValue(m[2]) {
			return true
		}
		return reURLScheme.MatchString(v) && urlCarriesSecret(v)
	}
	args := toStringSlice(entry["args"])
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			if reURLScheme.MatchString(a) && urlCarriesSecret(a) {
				return true
			}
			continue
		}
		if eq := strings.IndexByte(a, '='); eq != -1 {
			if pair(a[:eq], a[eq+1:]) {
				return true
			}
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && pair(a, args[i+1]) {
			return true
		}
	}
	url, _ := entry["url"].(string)
	return url != "" && urlCarriesSecret(url)
}

var (
	reURLScheme = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	reHeaderKV  = regexp.MustCompile(`^([^:\s]+)\s*:\s*(.*)$`)
)

var reSecretName = regexp.MustCompile(`(?i)(?:api[-_]?key|access[-_]?key|secret|token|password|passwd|auth|authorization)$`)

func urlCarriesSecret(u string) bool {
	// userinfo password
	if m := regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://[^/?#@:]*:([^/?#@]*)@`).FindStringSubmatch(u); m != nil {
		if looksSecretValue(m[1]) {
			return true
		}
	}
	q := u
	if h := strings.IndexByte(q, '#'); h != -1 {
		q = q[:h]
	}
	if i := strings.IndexByte(q, '?'); i != -1 {
		for _, pair := range strings.Split(q[i+1:], "&") {
			if eq := strings.IndexByte(pair, '='); eq != -1 {
				if reSecretName.MatchString(pair[:eq]) && looksSecretValue(pair[eq+1:]) {
					return true
				}
			}
		}
	}
	return false
}

func looksSecretValue(v string) bool {
	return v != "" && !reBearerEnvRef.MatchString(reCursorEnvRef.ReplaceAllString(v, "${$1}"))
}

// --- rules -----------------------------------------------------------------

type ruleScan struct {
	dir, gocodeMd, rulesOut, skillsOut, label string
	scope                                     Scope
	cwd                                       string
	claimed                                   collisionSet
}

// cursorRuleFrontmatter is the Cursor rule frontmatter schema.
type cursorRuleFrontmatter struct {
	description string
	globs       []string
	alwaysApply bool
}

func (c cursorImporter) scanRules(rs ruleScan, res *ImportResult) {
	files, truncated := collectFiles(rs.dir, ".mdc", cursorMaxRuleDirs, cursorMaxRules)
	if truncated {
		res.unmap(rs.scope, rs.label+"/", "Has more rule files, or deeper folders, than the import scans. Review the remaining rules manually.")
	}
	for _, rel := range files {
		src := filepath.Join(rs.dir, filepath.FromSlash(rel))
		label := rs.label + "/" + rel
		st, err := os.Lstat(src)
		if err == nil && st.Size() > SkillMaxBytes {
			res.unmap(rs.scope, label, "Is larger than the 1 MB gocode loads for a skill or rule. Trim it and re-run, or port it manually.")
			continue
		}
		raw, err := readFileLimited(src)
		if err != nil {
			res.unmap(rs.scope, label, "Could not read. Review it manually.")
			continue
		}
		raw = strings.ReplaceAll(raw, "\r\n", "\n")
		fm := splitFrontmatter(raw)
		if fm.parseError {
			res.unmap(rs.scope, label, "Its frontmatter is not valid YAML, so its activation mode is unknown. Fix the frontmatter and re-run, or port it manually.")
			continue
		}
		rf := parseRuleFrontmatter(fm.fields)
		content := fm.content

		ruleKey := strings.TrimSuffix(rel, ".mdc")
		segs := strings.Split(ruleKey, "/")
		safeSegs := make([]string, len(segs))
		for i, s := range segs {
			safeSegs[i] = safeName(s)
		}
		safeJoined := strings.Join(safeSegs, "_")
		id := "cursor:" + string(rs.scope) + ":rule:" + ruleKey

		var header string
		if rf.description != "" {
			header = "# " + collapseSpaces(rf.description) + "\n\n"
		}

		// alwaysApply → instructions appended to gocode.md
		if rf.alwaysApply {
			body := header + strings.TrimRight(content, "\n") + "\n"
			res.add(ImportItem{
				ID:                id,
				Kind:              KindInstructions,
				Scope:             rs.scope,
				Label:             `Rule "` + ruleKey + `" (always applied)`,
				Description:       rf.description,
				Fingerprint:       body,
				Content:           body,
				SourcePath:        src,
				TargetPath:        rs.gocodeMd,
				AppendInstruction: true,
				applyKind:         applyAppend,
				marker:            escapeHTMLComment(id),
				containIn:         projectRoot(rs.scope, rs.cwd),
			})
			continue
		}

		// globs → scoped rule file
		if len(rf.globs) > 0 {
			target := filepath.Join(rs.rulesOut, filepath.Join(safeSegs...)) + ".md"
			if !rs.claimed.claim(target) {
				res.unmap(rs.scope, label, collisionReason)
				continue
			}
			paths := make([]string, len(rf.globs))
			for i, g := range rf.globs {
				paths[i] = emDash(g)
			}
			body := "---\n" + emitFrontmatter([]fmField{{"paths", paths}}) + "---\n\n" + header + strings.TrimRight(content, "\n") + "\n"
			res.add(ImportItem{
				ID:          id,
				Kind:        KindInstructions,
				Scope:       rs.scope,
				Label:       `Rule "` + ruleKey + `" (scoped to ` + itoa(len(rf.globs)) + ` path ` + pluralize(len(rf.globs), "pattern", "patterns") + `)`,
				Description: firstNonEmpty(rf.description, "→ "+target),
				Fingerprint: body,
				Content:     body,
				SourcePath:  src,
				TargetPath:  target,
				applyKind:   applyNewFile,
				msgName:     safeJoined,
				containIn:   projectRoot(rs.scope, rs.cwd),
			})
			continue
		}

		// otherwise → a skill
		manual := rf.description == ""
		target := filepath.Join(rs.skillsOut, safeJoined)
		if !rs.claimed.claim(target) {
			res.unmap(rs.scope, label, collisionReason)
			continue
		}
		if isReservedSkillsDir(safeJoined) {
			res.unmap(rs.scope, label, "Its name is the skills directory name gocode reserves for synced skills, so the skill would never load. Rename the rule and re-run the import.")
			continue
		}
		if reason := validateArgPlaceholders(content, nil); reason != "" {
			res.unmap(rs.scope, label, reason)
			continue
		}
		desc := firstNonEmpty(rf.description, firstHeadingOrLine(content), "Imported Cursor rule "+ruleKey+" (manual)")
		fields := []fmField{{"name", safeJoined}, {"description", emDash(desc)}}
		if manual {
			fields = append(fields, fmField{"disable-model-invocation", true})
		}
		hadMarker := hasShellExecMarker(content)
		renderedBody := content
		if hadMarker {
			renderedBody = escapeShellExec(content)
		}
		skillMd := "---\n" + emitFrontmatter(fields) + "---\n\n" + strings.TrimRight(renderedBody, "\n") + "\n"
		if len(skillMd) > SkillMaxBytes {
			res.unmap(rs.scope, label, "Is larger than the 1 MB gocode loads for a skill or rule. Trim it and re-run, or port it manually.")
			continue
		}
		warning := skillAlwaysInContextWarning
		if manual {
			warning = skillRunnableWarning
		}
		var escNote string
		if hadMarker {
			warning = "Line has a gocode shell-exec marker (```! or !`…`), inert in Cursor but run when the skill loads; if imported it is written escaped, so nothing runs — review the file. " + warning
			escNote = " — shell-exec marker escaped to plain text"
		}
		res.add(ImportItem{
			ID:          id,
			Kind:        KindSkill,
			Scope:       rs.scope,
			Label:       `Rule "` + ruleKey + `" (→ skill)`,
			Description: "→ " + filepath.Join(target, "SKILL.md"),
			Fingerprint: skillMd,
			Content:     skillMd,
			SourcePath:  src,
			TargetPath:  filepath.Join(target, "SKILL.md"),
			Warning:     warning,
			applyKind:   applyNewFile,
			msgName:     safeJoined,
			existsPath:  target,
			msgSuffix:   escNote,
			containIn:   projectRoot(rs.scope, rs.cwd),
		})
	}
}

const (
	skillAlwaysInContextWarning = "Becomes a gocode skill: its description is always in context, and once gocode loads it the body stays in context for the rest of the session. Unchecked by default; review before importing."
	skillRunnableWarning        = "Becomes a gocode skill you run with /<name>; once loaded, the body stays in context for the rest of the session. Unchecked by default; review before importing."
)

func parseRuleFrontmatter(fields map[string]any) cursorRuleFrontmatter {
	var rf cursorRuleFrontmatter
	if d, ok := fields["description"].(string); ok {
		rf.description = strings.TrimSpace(d)
	}
	switch g := fields["globs"].(type) {
	case string:
		if t := strings.TrimSpace(g); t != "" {
			rf.globs = []string{t}
		}
	case []string:
		for _, s := range g {
			if t := strings.TrimSpace(s); t != "" {
				rf.globs = append(rf.globs, t)
			}
		}
	}
	rf.alwaysApply = isTruthy(fields["alwaysApply"])
	return rf
}

// --- skills ----------------------------------------------------------------

type skillScan struct {
	root, skillsOut, label string
	scope                  Scope
	cwd                    string
	claimed                collisionSet
}

// cursor skill frontmatter key sets.
var (
	cursorSkillKeepKeys = map[string]bool{
		"description": true, "paths": true, "disable-model-invocation": true,
		"user-invocable": true, "argument-hint": true, "when_to_use": true,
		"version": true, "license": true, "compatibility": true,
	}
	cursorSkillDropKeys = map[string]bool{"icon": true, "color": true}
)

func (c cursorImporter) scanSkills(ss skillScan, res *ImportResult) {
	dirs, truncated := collectSkillDirs(ss.root, cursorMaxSkillDir, cursorMaxSkills)
	if truncated {
		res.unmap(ss.scope, ss.label+"/", "Has more skills, or deeper folders, than the import scans. Review the remaining skills manually.")
	}
	for _, dir := range dirs {
		rel := filepath.ToSlash(mustRel(ss.root, dir))
		label := ss.label + "/" + rel
		safe := safeName(filepath.Base(dir))
		target := filepath.Join(ss.skillsOut, safe)
		if !ss.claimed.claim(target) {
			res.unmap(ss.scope, label, collisionReason)
			continue
		}
		skillPath := filepath.Join(dir, "SKILL.md")
		st, err := os.Lstat(skillPath)
		if err != nil || !st.Mode().IsRegular() {
			res.unmap(ss.scope, label, "Its SKILL.md is not a regular file (a symlink?). Copy the skill manually.")
			continue
		}
		if st.Size() > SkillMaxBytes {
			res.unmap(ss.scope, label, "Its SKILL.md is larger than the 1 MB gocode loads for a skill. Trim it and re-run, or copy the skill manually.")
			continue
		}
		raw, err := readFileLimited(skillPath)
		if err != nil {
			res.unmap(ss.scope, label, "Could not read. Review it manually.")
			continue
		}
		raw = strings.ReplaceAll(raw, "\r\n", "\n")
		rm := remapSkillFrontmatter(raw, safe, false)
		if !rm.ok {
			res.unmap(ss.scope, label, rm.reason)
			continue
		}
		if reason := validateArgPlaceholders(rm.body, nil); reason != "" {
			res.unmap(ss.scope, label, reason)
			continue
		}
		escaped := hasShellExecMarker(rm.body)
		var droppedNote, droppedSuffix string
		if len(rm.dropped) > 0 {
			d := "dropped " + itoa(len(rm.dropped)) + " " + pluralize(len(rm.dropped), "frontmatter key", "frontmatter keys")
			if ss.scope == ScopeUser {
				d += " (" + strings.Join(rm.dropped, ", ") + ")"
			}
			d += " that gocode does not share with Cursor"
			droppedNote = "D" + d[1:] + "."
			droppedSuffix = " — " + d
		}
		var escWarning, escSuffix string
		if escaped {
			escWarning = "SKILL.md has a gocode shell-exec marker (```! or !`…`), inert in Cursor but run when the skill loads; if imported it is written escaped, so nothing runs — review the file. "
			escSuffix = " — shell-exec marker escaped to plain text"
		}
		skillRoot := ss.root
		if ss.scope == ScopeProject {
			skillRoot = ss.cwd
		}
		name := safe
		res.add(ImportItem{
			ID:          "cursor:" + string(ss.scope) + ":skill:" + rel,
			Kind:        KindSkill,
			Scope:       ss.scope,
			Label:       `Skill "` + safe + `"`,
			Description: "→ " + target,
			Fingerprint: jsonFingerprint([]any{rel, rm.out}),
			Content:     rm.out,
			SourcePath:  dir,
			TargetPath:  filepath.Join(target, "SKILL.md"),
			Warning:     strings.TrimSpace(escWarning + cursorSkillCopyWarning + " " + droppedNote),
			applyKind:   applySkillDir,
			msgName:     safe,
			msgSuffix:   escSuffix + droppedSuffix,
			containIn:   projectRoot(ss.scope, ss.cwd),
			skillRoot:   skillRoot,
			skillPath:   dir,
			skillDir:    target,
			prepareSkill: func(raw string) (string, string) {
				r := remapSkillFrontmatter(raw, name, escaped)
				if !r.ok {
					return "", name + ": " + r.reason
				}
				if reason := validateArgPlaceholders(r.body, nil); reason != "" {
					return "", name + ": " + reason
				}
				if !escaped && hasShellExecMarker(r.body) {
					return "", name + ": SKILL.md gained a shell-exec marker since the scan — re-run /import to review it"
				}
				return r.out, ""
			},
		})
	}
}

const cursorSkillCopyWarning = "Skill directories are copied as-is — bundled scripts and body instructions become active in gocode, and once gocode loads the skill it stays in context for the session. Unchecked by default; review SKILL.md and its directory before importing."

type skillRemap struct {
	ok      bool
	reason  string
	out     string
	body    string
	dropped []string
}

// remapSkillFrontmatter rewrites a SKILL.md's frontmatter to Gocode's
// shape — force `name`, map `globs`→`paths`, keep a known-key allowlist, drop
// icon/color, and record other dropped keys.
func remapSkillFrontmatter(raw, name string, escapeShell bool) skillRemap {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	fm := splitFrontmatter(raw)
	if fm.parseError {
		return skillRemap{reason: "Its SKILL.md frontmatter is not valid YAML. Fix the frontmatter and re-run, or copy the skill manually."}
	}
	body := fm.content
	rendered := body
	if escapeShell {
		rendered = escapeShellExec(body)
	}
	if !fm.hadFrontmatter {
		return skillRemap{ok: true, out: strings.TrimRight(rendered, "\n") + "\n", body: body}
	}
	var fields []fmField
	var dropped []string
	_, hasPaths := fm.fields["paths"]
	for _, key := range fm.order {
		if cursorSkillDropKeys[key] {
			continue
		}
		if key == "name" {
			fields = append(fields, fmField{"name", name})
			continue
		}
		target := key
		if key == "globs" && !hasPaths {
			target = "paths"
		}
		if !cursorSkillKeepKeys[target] {
			dropped = append(dropped, key)
			continue
		}
		v := mapSkillValue(fm.fields[key])
		if v == nil {
			dropped = append(dropped, key)
			continue
		}
		fields = append(fields, fmField{target, v})
	}
	var head string
	if len(fields) > 0 {
		head = "---\n" + emitFrontmatter(fields) + "---\n"
	}
	sep := ""
	if head == "" && strings.HasPrefix(strings.TrimLeft(rendered, " \t"), "---") {
		sep = "\n"
	}
	return skillRemap{ok: true, out: head + sep + strings.TrimRight(rendered, "\n") + "\n", body: body, dropped: dropped}
}

// mapSkillValue maps a frontmatter value: strings/[]string get em-dash normalization, bools and
// numbers pass through, everything else is dropped (nil).
func mapSkillValue(v any) any {
	switch t := v.(type) {
	case string:
		return emDash(t)
	case bool:
		return t
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = emDash(s)
		}
		return out
	}
	return nil
}

// --- commands --------------------------------------------------------------

func (cursorImporter) scanCommands(dir string, scope Scope, label, cwd string, opts Options, claimed collisionSet, res *ImportResult) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []os.DirEntry
	for _, de := range ents {
		if de.Type().IsRegular() && strings.HasSuffix(de.Name(), ".md") {
			files = append(files, de)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	if len(files) > cursorMaxCommands {
		res.unmap(scope, label+"/", "Has more command files than the import scans. Review the remaining commands manually.")
		files = files[:cursorMaxCommands]
	}
	for _, de := range files {
		base := strings.TrimSuffix(de.Name(), ".md")
		safe := safeName(base)
		src := filepath.Join(dir, de.Name())
		var target string
		if scope == ScopeUser {
			target = filepath.Join(opts.gocodeDir(), "commands", safe+".md")
		} else {
			target = filepath.Join(cwd, ".gocode", "commands", safe+".md")
		}
		if !claimed.claim(target) {
			res.unmap(scope, "Command /"+base, collisionReason)
			continue
		}
		st, err := os.Lstat(src)
		if err == nil && st.Size() > SkillMaxBytes {
			res.unmap(scope, "Command /"+base, "Is larger than the 1 MB gocode loads for a command. Trim it and re-run, or port it manually.")
			continue
		}
		body, err := readFileLimited(src)
		if err != nil {
			res.unmap(scope, "Command /"+base, "Could not read. Review it manually.")
			continue
		}
		if hasShellExecMarker(body) {
			res.unmap(scope, "Command /"+base, "Contains a `` !`…` `` or ```! shell-exec marker (inert in Cursor, live in gocode). Port it manually.")
			continue
		}
		if reason := validateArgPlaceholders(body, nil); reason != "" {
			res.unmap(scope, "Command /"+base, reason)
			continue
		}
		content := strings.TrimRight(body, "\n") + "\n"
		if strings.HasPrefix(strings.TrimLeft(body, " \t"), "---") {
			content = "\n" + content
		}
		res.add(ImportItem{
			ID:          "cursor:" + string(scope) + ":command:" + base,
			Kind:        KindCommand,
			Scope:       scope,
			Label:       "Command /" + base,
			Description: "→ " + target,
			Fingerprint: body,
			Content:     content,
			SourcePath:  src,
			TargetPath:  target,
			applyKind:   applyCopyFile,
			msgName:     base,
			containIn:   projectRoot(scope, cwd),
		})
	}
}

// --- hooks -----------------------------------------------------------------

// cursorHookEventMap maps Cursor hook event names to Gocode event names.
var cursorHookEventMap = map[string]string{
	"beforeShellExecution": "PreToolUse", "afterShellExecution": "PostToolUse",
	"beforeMCPExecution": "PreToolUse", "afterMCPExecution": "PostToolUse",
	"beforeReadFile": "PreToolUse", "afterFileEdit": "PostToolUse",
	"beforeSubmitPrompt": "UserPromptSubmit", "preToolUse": "PreToolUse",
	"postToolUse": "PostToolUse", "sessionStart": "SessionStart",
	"sessionEnd": "SessionEnd", "subagentStart": "SubagentStart",
	"subagentStop": "SubagentStop", "stop": "Stop",
}

func (cursorImporter) scanHooks(path string, scope Scope, label string, res *ImportResult) {
	raw, err := readFileLimited(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		res.unmap(scope, label, "Could not read. Review your hooks there manually.")
		return
	}
	var parsed map[string]any
	if err := parseJSONC(raw, &parsed); err != nil {
		res.unmap(scope, label, "Could not parse (unexpected structure). Re-add any hooks you need under the `hooks` key in settings.json.")
		return
	}
	hooks := asNamedTable(parsed["hooks"])
	if hooks == nil {
		if parsed["hooks"] != nil {
			res.unmap(scope, label, "Has an unexpected shape (expected a `hooks` object keyed by event). Re-add any hooks you need under the `hooks` key in settings.json.")
		}
		return
	}
	events := sortedKeys(hooks)
	if len(events) > cursorMaxServers {
		res.unmap(scope, label, "Has more hook events than the import scans. Review the remaining hooks manually.")
		events = events[:cursorMaxServers]
	}
	for _, ev := range events {
		mapped := cursorHookEventMap[ev]
		lbl := label + ": " + ev
		if mapped != "" {
			lbl += " → " + mapped
			res.unmap(scope, lbl, "Hook scripts are not translated (event payloads and exit-code contracts differ). Re-add it under the gocode event named here, via the `hooks` key in settings.json.")
		} else {
			res.unmap(scope, lbl, "No matching gocode hook event. See the `hooks` key in settings.json for the events gocode offers.")
		}
	}
}

// --- misc ------------------------------------------------------------------

func (cursorImporter) scanMisc(home, cwd string, opts Options, res *ImportResult) {
	atHome := filepath.Join(cwd, ".cursor") == home
	if opts.Scope != ScopeUser {
		if fileExists(filepath.Join(cwd, ".cursorignore")) {
			res.unmap(ScopeProject, ".cursorignore", "gocode has no direct equivalent. If you need to exclude paths, add Read/Glob deny rules under `permissions` in settings.json.")
		}
		if fileExists(filepath.Join(cwd, ".cursorindexingignore")) {
			res.unmap(ScopeProject, ".cursorindexingignore", "gocode has no codebase index; nothing to configure.")
		}
	}
	if opts.Scope != ScopeProject {
		res.unmap(ScopeUser, "Cursor User Rules", "Stored in Cursor Settings → Rules, not on disk. Paste the ones you want into ~/.config/gocode/gocode.md (your user-level memory file).")
		res.unmap(ScopeUser, "Cursor Team Rules", "Managed in the Cursor dashboard, not on disk. The gocode equivalents are an organization-wide gocode.md or managed settings.")
		res.unmap(ScopeUser, "Cursor Memories", "Stored in Cursor app-data with no file-based export. Open Cursor → Settings → Rules & Memories and copy anything you need into gocode.md.")
		res.unmap(ScopeUser, "Cursor Custom Modes", "UI state backed by a skill, with no gocode equivalent; run the imported skill with /<name> when you want it in context.")
	}
	if opts.Scope != ScopeUser && atHome && fileExists(filepath.Join(cwd, ".cursorrules")) {
		res.unmap(ScopeUser, "~/.cursorrules", "At $HOME this is user-scope. Not yet auto-imported; append it to ~/.config/gocode/gocode.md (your user-level memory file).")
	}
}

// --- shared small helpers --------------------------------------------------

// collectFiles walks dir up to maxDepth levels collecting files with ext, as
// forward-slash relative paths, sorted; truncated is set past maxFiles.
func collectFiles(dir, ext string, maxDepth, maxFiles int) (files []string, truncated bool) {
	var walk func(d, rel string, depth int)
	walk = func(d, rel string, depth int) {
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, de := range ents {
			r := de.Name()
			if rel != "" {
				r = rel + "/" + de.Name()
			}
			switch {
			case de.Type().IsRegular() && strings.HasSuffix(de.Name(), ext):
				if len(files) >= maxFiles {
					truncated = true
					return
				}
				files = append(files, r)
			case de.IsDir():
				if depth >= maxDepth {
					truncated = true
					continue
				}
				walk(filepath.Join(d, de.Name()), r, depth+1)
			}
		}
	}
	walk(dir, "", 0)
	sort.Strings(files)
	return files, truncated
}

// collectSkillDirs finds directories (up to maxDepth deep) that contain a
// SKILL.md, sorted; truncated is set past maxDirs.
func collectSkillDirs(root string, maxDepth, maxDirs int) (dirs []string, truncated bool) {
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		ents, err := os.ReadDir(d)
		if err != nil {
			return
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, de := range ents {
			if !de.IsDir() {
				continue
			}
			sub := filepath.Join(d, de.Name())
			if fileExists(filepath.Join(sub, "SKILL.md")) {
				if len(dirs) >= maxDirs {
					truncated = true
					return
				}
				dirs = append(dirs, sub)
				continue
			}
			if depth >= maxDepth {
				truncated = true
				continue
			}
			walk(sub, depth+1)
		}
	}
	walk(root, 0)
	sort.Strings(dirs)
	return dirs, truncated
}

func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return filepath.Base(target)
	}
	return rel
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	}
	return false
}

func collapseSpaces(s string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
}

// firstHeadingOrLine returns the first non-empty line with leading '#' markers
// stripped, truncated to 200 chars.
func firstHeadingOrLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if t != "" {
			r := []rune(t)
			if len(r) > 200 {
				return string(r[:200])
			}
			return t
		}
	}
	return ""
}

// escapeShellExec neutralizes Gocode shell-exec markers so an imported body is
// written as inert text: "`!" → "` !", "!`" → "! `", and a '!' at line start
// or after whitespace is backslash-escaped.
func escapeShellExec(s string) string {
	s = strings.ReplaceAll(s, "`!", "` !")
	s = strings.ReplaceAll(s, "!`", "! `")
	return reBangAfterSpace.ReplaceAllString(s, "${1}\\!")
}

var reBangAfterSpace = regexp.MustCompile(`(?m)(^|\s)!`)

func sortStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
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
