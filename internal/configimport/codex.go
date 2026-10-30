package configimport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
)

// Codex importer: brings OpenAI Codex config into Gocode.
//
// Source layout (user scope rooted at ~/.codex, project scope at <cwd>/.codex):
//
//	config.toml        — settings TOML: mcp_servers, approval_policy, agents,
//	                     skills, sandbox_mode, web_search, hooks, features, …
//	AGENTS.md          — memory/instructions           → gocode.md
//	AGENTS.override.md — override memory                → gocode.md / gocode.local.md
//	prompts/*.md       — slash-command prompts          → <gocode root>/commands/*.md
//
// Mapping into Gocode:
//
//	[mcp_servers.<name>] → an mcpServers entry (http when `url`, else stdio)
//	approval_policy      → permissions.defaultMode (+ an escalation warning)
//	[agents.<name>]      → <gocode root>/agents/<name>.md (subagent, tools dropped)
//	[[skills.config]]    → <gocode root>/skills/<name>/ (dir copy; path-escape guarded)
//	AGENTS.md            → appended to gocode.md
//	prompts/<n>.md       → <gocode root>/commands/<n>.md

type codexImporter struct{}

func (codexImporter) ID() Source          { return SourceCodex }
func (codexImporter) DisplayName() string { return "OpenAI Codex" }

// codexHome returns the Codex config dir.
func codexHome(opts Options) string { return opts.sourceHome(".codex") }

func (codexImporter) Detect(opts Options) (bool, error) {
	if opts.detectRedirected() {
		return true, nil
	}
	home := codexHome(opts)
	cwd := opts.cwd()
	if fileExists(filepath.Join(home, "config.toml")) ||
		fileExists(filepath.Join(home, "AGENTS.md")) ||
		fileExists(filepath.Join(home, "prompts")) {
		return true, nil
	}
	if containedNoSymlink(cwd, filepath.Join(cwd, ".codex", "config.toml")) != "" &&
		fileExists(filepath.Join(cwd, ".codex", "config.toml")) {
		return true, nil
	}
	return false, nil
}

func (c codexImporter) Import(opts Options) (ImportResult, error) {
	res := ImportResult{SourceID: SourceCodex, DisplayName: c.DisplayName()}
	home := codexHome(opts)
	cwd := opts.cwd()

	if opts.userScopeRedirected() {
		res.unmap(ScopeUser, "Codex user-scope config", redirectedReason)
		opts.Scope = ScopeProject
	}
	if opts.scopeAllows(ScopeUser) {
		c.scanConfig(home, ScopeUser, cwd, opts, &res)
		c.scanPrompts(home, opts, &res)
	}
	if opts.scopeAllows(ScopeProject) {
		projDir := filepath.Join(cwd, ".codex")
		if projDir != home {
			if containedNoSymlink(cwd, filepath.Join(projDir, "config.toml")) != "" {
				if fileExists(filepath.Join(projDir, "config.toml")) {
					c.scanConfig(projDir, ScopeProject, cwd, opts, &res)
				}
			} else if fileExists(filepath.Join(projDir, "config.toml")) {
				res.unmap(ScopeProject, ".codex/config.toml", "Is (or is under) a symlink — skipping project-scope read for safety.")
			}
		}
	}
	c.scanInstructions(home, cwd, opts, &res)
	return res, nil
}

// codexConfig is the parsed subset of config.toml.
type codexConfig struct {
	ApprovalPolicy string
	SandboxMode    string
	WebSearch      *bool
	MCPServers     map[string]any
	Agents         map[string]any
	Skills         any
	Hooks          any
	Features       map[string]any
	ProjectDocKeys bool
	Extra          []string
}

var codexKnownKeys = map[string]bool{
	"model": true, "model_reasoning_effort": true, "approval_policy": true,
	"sandbox_mode": true, "web_search": true, "mcp_servers": true, "skills": true,
	"agents": true, "hooks": true, "features": true,
	"project_doc_fallback_filenames": true, "project_doc_max_bytes": true,
}

func (c codexImporter) scanConfig(dir string, scope Scope, cwd string, opts Options, res *ImportResult) {
	label := ".codex/config.toml"
	if scope == ScopeUser {
		label = "~/.codex/config.toml"
	}
	raw, err := readFileLimited(filepath.Join(dir, "config.toml"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		res.unmap(scope, label, "Could not read or parse. Review it manually.")
		return
	}
	parsed, err := parseTOML(raw)
	if err != nil {
		res.unmap(scope, label, "Could not parse (unexpected structure). Review it manually.")
		return
	}

	cfg := codexConfig{}
	for k, v := range parsed {
		switch k {
		case "approval_policy":
			cfg.ApprovalPolicy, _ = v.(string)
		case "sandbox_mode":
			cfg.SandboxMode, _ = v.(string)
		case "web_search":
			if b, ok := v.(bool); ok {
				cfg.WebSearch = &b
			}
		case "mcp_servers":
			cfg.MCPServers, _ = v.(map[string]any)
		case "agents":
			cfg.Agents, _ = v.(map[string]any)
		case "skills":
			cfg.Skills = v
		case "hooks":
			cfg.Hooks = v
		case "features":
			cfg.Features, _ = v.(map[string]any)
		case "project_doc_fallback_filenames", "project_doc_max_bytes":
			cfg.ProjectDocKeys = true
		default:
			if !codexKnownKeys[k] {
				cfg.Extra = append(cfg.Extra, k)
			}
		}
	}

	c.mapMCP(cfg.MCPServers, scope, cwd, res)
	c.mapApproval(cfg.ApprovalPolicy, scope, res)
	c.mapAgents(cfg.Agents, scope, cwd, opts, res)
	c.mapSkills(cfg.Skills, dir, scope, cwd, opts, res)

	if cfg.SandboxMode != "" {
		res.unmap(scope, `sandbox_mode = "`+cfg.SandboxMode+`"`,
			"Sandbox models differ. Review `sandbox` keys in settings.json if you relied on this.")
	}
	if cfg.WebSearch != nil {
		res.unmap(scope, "web_search = "+strconv.FormatBool(*cfg.WebSearch), "gocode enables WebSearch via permissions; no global toggle.")
	}
	if cfg.ProjectDocKeys {
		res.unmap(scope, "project_doc_* settings", "gocode hardcodes gocode.md / AGENTS.md discovery.")
	}
	if cfg.Hooks != nil {
		res.unmap(scope, "hooks",
			"Hook event names differ between Codex and gocode. Re-add via the `hooks` key in settings.json.")
	}
	if len(cfg.Features) > 0 {
		keys := sortedKeys(cfg.Features)
		res.unmap(scope, "[features] ("+strings.Join(keys, ", ")+")",
			"Product-specific toggles with no gocode equivalent.")
	}
	sort.Strings(cfg.Extra)
	for _, k := range cfg.Extra {
		res.unmap(scope, k, "Unrecognised config.toml key.")
	}
}

// mapMCP maps [mcp_servers.*] entries.
func (codexImporter) mapMCP(servers map[string]any, scope Scope, cwd string, res *ImportResult) {
	for _, name := range sortedKeys(servers) {
		entry, ok := servers[name].(map[string]any)
		if !ok {
			res.unmap(scope, `MCP server "`+name+`"`,
				"Entry in config.toml has an unexpected shape. Review it manually.")
			continue
		}
		command, _ := entry["command"].(string)
		url, _ := entry["url"].(string)
		if url == "" && command == "" {
			res.unmap(scope, `MCP server "`+name+`"`,
				"Has neither a url nor a command — nothing to import. Review it manually.")
			continue
		}
		safe := safeName(name)
		var cfg mcp.ServerConfig
		if url != "" {
			headers := map[string]string{}
			for k, v := range toStringMap(entry["http_headers"]) {
				headers[k] = v
			}
			if bearer, _ := entry["bearer_token_env_var"].(string); bearer != "" {
				headers["Authorization"] = "Bearer ${" + bearer + "}"
			}
			cfg = mcp.ServerConfig{Type: mcp.TransportHTTP, URL: url}
			if len(headers) > 0 {
				cfg.Headers = headers
			}
		} else {
			cfg = mcp.ServerConfig{
				Type:    mcp.TransportStdio,
				Command: command,
				Args:    toStringSlice(entry["args"]),
				Env:     toStringMap(entry["env"]),
			}
		}
		item := ImportItem{
			ID:             "codex:" + string(scope) + ":mcp:" + name,
			Kind:           KindMCP,
			Scope:          scope,
			Label:          `MCP server "` + name + `"`,
			Description:    firstNonEmpty(url, command),
			Fingerprint:    jsonFingerprint(cfg),
			MCP:            &cfg,
			MCPTargetScope: scope,
			applyKind:      applyMCP,
			msgName:        safe,
		}
		item.TargetPath = mcpTargetPath(scope, cwd, safe)
		if scope == ScopeProject {
			item.Warning = "Repo-authored MCP server — connecting runs its command or sends requests to its url. Review the config before importing."
		}
		res.add(item)
	}
}

// mapApproval maps approval_policy → permissions.defaultMode.
func (codexImporter) mapApproval(policy string, scope Scope, res *ImportResult) {
	if policy == "" {
		return
	}
	mode, warning := codexApprovalMode(policy)
	if scope == ScopeProject && mode == "auto" {
		res.unmap(scope, "Permission mode ("+policy+")",
			"Maps to `defaultMode: auto`, which repo-level settings cannot grant in gocode (and the ignored value would shadow your user-level permission mode). Adopt it in your user settings instead if you want it.")
		return
	}
	res.add(ImportItem{
		ID:          "codex:" + string(scope) + ":approval",
		Kind:        KindSetting,
		Scope:       scope,
		Label:       "Permission mode (" + policy + " → " + mode + ")",
		Fingerprint: mode,
		Warning:     warning,
		Setting:     &SettingPatch{PermissionsDefaultMode: mode},
		applyKind:   applySetting,
	})
}

// codexApprovalMode maps a Codex approval policy to a Gocode permission mode and
// its warning.
func codexApprovalMode(policy string) (mode, warning string) {
	escalate := func(m, why string) (string, string) {
		return m, "approval_policy → `" + m + "` escalates the permission mode (" + why + "). Unchecked by default — review before importing."
	}
	switch policy {
	case "suggest", "untrusted", "on-request":
		return "default", "Overwrites your current permission mode with `default`. Unchecked by default — check to adopt the Codex setting."
	case "auto-edit":
		return escalate("acceptEdits", "edits auto-applied without per-write prompts")
	case "on-failure", "full-auto":
		return escalate("auto", "gocode decides what runs without asking")
	case "never":
		return escalate("auto", "Codex `never` relies on Codex's own sandbox; gocode has no equivalent")
	default:
		return escalate("auto", "unrecognised source policy")
	}
}

// mapAgents maps [agents.*] → subagent markdown files.
func (codexImporter) mapAgents(agents map[string]any, scope Scope, cwd string, opts Options, res *ImportResult) {
	for _, name := range sortedKeys(agents) {
		entry, ok := agents[name].(map[string]any)
		if !ok {
			res.unmap(scope, `Subagent "`+name+`"`,
				"Entry in config.toml has an unexpected shape. Review it manually.")
			continue
		}
		description, _ := entry["description"].(string)
		instructions, _ := entry["instructions"].(string)
		tools := toStringSlice(entry["tools"])
		safe := safeName(name)
		desc := description
		if desc == "" {
			desc = "Subagent imported from Codex (" + safe + ")."
		}
		body := "---\n" +
			emitFrontmatter([]fmField{{"name", safe}, {"description", emDash(desc)}}) +
			"---\n\n" + instructions + "\n"

		var dir string
		if scope == ScopeUser {
			dir = filepath.Join(opts.gocodeDir(), "agents")
		} else {
			dir = filepath.Join(cwd, ".gocode", "agents")
		}
		item := ImportItem{
			ID:          "codex:" + string(scope) + ":agent:" + name,
			Kind:        KindSubagent,
			Scope:       scope,
			Label:       `Subagent "` + name + `"`,
			Description: description,
			Fingerprint: body,
			Content:     body,
			TargetPath:  filepath.Join(dir, safe+".md"),
			applyKind:   applyNewFile,
			msgName:     safe,
			containIn:   projectRoot(scope, cwd),
		}
		if len(tools) > 0 {
			item.Warning = "Codex tool restrictions dropped (tool names differ); the imported agent has access to all gocode tools. Review before enabling."
		}
		// The shell-exec gate decides mappability (applied at scan time).
		if hasShellExecMarker(body) {
			res.unmap(scope, `Subagent "`+name+`"`,
				safe+": body contains a ```! or `` !`…` `` shell-exec marker (inert in Codex, live in gocode) — port it manually")
			continue
		}
		if reason := validateArgPlaceholders(body, nil); reason != "" {
			res.unmap(scope, `Subagent "`+name+`"`, safe+": "+reason)
			continue
		}
		res.add(item)
	}
}

// mapSkills maps [[skills.config]] entries → skill directory copies.
func (codexImporter) mapSkills(skills any, dir string, scope Scope, cwd string, opts Options, res *ImportResult) {
	entries, ok := codexSkillEntries(skills)
	if !ok {
		res.unmap(scope, "[skills]",
			"Has an unexpected shape (expected `[[skills.config]]` entries). Review it manually.")
		return
	}
	for _, e := range entries {
		path, _ := e["path"].(string)
		if path == "" {
			res.unmap(scope, "[[skills.config]] entry", "Missing or non-string `path`. Review it manually.")
			continue
		}
		resolved := containedJoin(dir, path)
		if resolved == "" {
			res.unmap(scope, `skill path "`+path+`"`,
				"Path escapes the Codex config directory — skipped for safety.")
			continue
		}
		safe := safeName(filepath.Base(resolved))
		var target string
		if scope == ScopeUser {
			target = filepath.Join(opts.gocodeDir(), "skills", safe)
		} else {
			target = filepath.Join(cwd, ".gocode", "skills", safe)
		}
		res.add(ImportItem{
			ID:          "codex:" + string(scope) + ":skill:" + path,
			Kind:        KindSkill,
			Scope:       scope,
			Label:       `Skill "` + safe + `"`,
			Description: resolved,
			Fingerprint: path,
			SourcePath:  resolved,
			TargetPath:  target,
			Warning:     "Skill directories are copied as-is — bundled scripts and body instructions become active in gocode. Unchecked by default; review SKILL.md and its directory before importing.",
			applyKind:   applySkillDir,
			msgName:     safe,
			containIn:   projectRoot(scope, cwd),
			skillRoot:   dir,
			skillPath:   path,
			skillDir:    target,
			prepareSkill: func(raw string) (string, string) {
				// Codex treats SKILL.md as plain text: anything gocode would
				// interpret (shell-exec markers, argument re-pairing, YAML
				// frontmatter) is refused rather than silently activated.
				if hasShellExecMarker(raw) {
					return "", safe + ": SKILL.md contains a ```! or `` !`…` `` shell-exec marker (inert in Codex, live in gocode) — copy it manually"
				}
				if reason := validateArgPlaceholders(raw, nil); reason != "" {
					return "", safe + ": " + reason
				}
				if reLeadingFence.MatchString(raw) {
					return "", safe + ": SKILL.md starts with `---` — Codex treats SKILL.md as plain text, so any YAML frontmatter is gocode-only and would take effect on import. Copy the skill manually after reviewing what each key enables."
				}
				return raw, ""
			},
		})
	}
}

// scanPrompts maps prompts/*.md → user commands.
func (codexImporter) scanPrompts(home string, opts Options, res *ImportResult) {
	dir := filepath.Join(home, "prompts")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range ents {
		if !de.Type().IsRegular() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		base := strings.TrimSuffix(name, ".md")
		src := filepath.Join(dir, name)
		target := filepath.Join(opts.gocodeDir(), "commands", safeName(base)+".md")
		body, err := readFileLimited(src)
		if err != nil {
			res.unmap(ScopeUser, "Command /"+base, "Could not read. Review it manually.")
			continue
		}
		if hasShellExecMarker(body) {
			res.unmap(ScopeUser, "Command /"+base,
				"Contains a `` !`…` `` or ```! shell-exec marker (inert in Codex, live in gocode). Port it manually.")
			continue
		}
		if reason := validateArgPlaceholders(body, nil); reason != "" {
			res.unmap(ScopeUser, "Command /"+base, reason)
			continue
		}
		content := strings.TrimRight(body, "\n") + "\n"
		if strings.HasPrefix(strings.TrimLeft(body, " \t"), "---") {
			content = "\n" + content
		}
		res.add(ImportItem{
			ID:          "codex:user:prompt:" + name,
			Kind:        KindCommand,
			Scope:       ScopeUser,
			Label:       "Command /" + base,
			Description: "→ " + target,
			Fingerprint: body,
			Content:     content,
			SourcePath:  src,
			TargetPath:  target,
			applyKind:   applyCopyFile,
			msgName:     base,
		})
	}
}

// scanInstructions maps AGENTS.md / AGENTS.override.md → gocode.md.
func (codexImporter) scanInstructions(home, cwd string, opts Options, res *ImportResult) {
	type src struct {
		id, path, target, label string
		scope                   Scope
	}
	var list []src
	gocode := opts.configDir()
	if opts.Scope != ScopeProject {
		list = append(list,
			src{"codex:user:instructions", filepath.Join(home, "AGENTS.md"), filepath.Join(gocode, "gocode.md"), "AGENTS.md", ScopeUser},
			src{"codex:user:override", filepath.Join(home, "AGENTS.override.md"), filepath.Join(gocode, "gocode.md"), "AGENTS.override.md", ScopeUser},
		)
	}
	if opts.Scope != ScopeUser {
		list = append(list,
			src{"codex:project:instructions", filepath.Join(cwd, "AGENTS.md"), filepath.Join(cwd, "gocode.md"), "AGENTS.md", ScopeProject},
			src{"codex:project:override", filepath.Join(cwd, "AGENTS.override.md"), filepath.Join(cwd, "gocode.local.md"), "AGENTS.override.md", ScopeProject},
		)
	}
	for _, s := range list {
		if !fileExists(s.path) {
			continue
		}
		var content string
		if s.scope == ScopeUser {
			b, err := readFileLimited(s.path)
			if err != nil {
				res.unmap(ScopeUser, s.label, "Could not read. Review it manually.")
				continue
			}
			content = b
		}
		res.add(ImportItem{
			ID:                s.id,
			Kind:              KindInstructions,
			Scope:             s.scope,
			Label:             s.label,
			Fingerprint:       firstNonEmpty(content, s.path),
			Content:           content,
			SourcePath:        s.path,
			TargetPath:        s.target,
			AppendInstruction: true,
			applyKind:         applyAppend,
			marker:            s.id,
			containIn:         projectRoot(s.scope, cwd),
		})
	}
}

// reLeadingFence matches a document that opens with a YAML fence.
var reLeadingFence = regexp.MustCompile(`^\s*---`)

// projectRoot is the containment root for project-scope writes ("" for user).
func projectRoot(scope Scope, cwd string) string {
	if scope == ScopeProject {
		return cwd
	}
	return ""
}

// codexSkillEntries accepts {config:[...]} or an empty table.
func codexSkillEntries(skills any) ([]map[string]any, bool) {
	if skills == nil {
		return nil, true
	}
	m, ok := skills.(map[string]any)
	if !ok {
		return nil, false
	}
	if cfg, ok := m["config"]; ok {
		arr, ok := cfg.([]any)
		if !ok {
			return nil, false
		}
		var out []map[string]any
		for _, e := range arr {
			if em, ok := e.(map[string]any); ok {
				out = append(out, em)
			}
		}
		return out, true
	}
	if len(m) == 0 {
		return nil, true
	}
	return nil, false
}
