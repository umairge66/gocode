package configimport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
)

// Gemini importer: brings Google Gemini CLI config into Gocode.
//
// Source layout (user scope rooted at ~/.gemini, project scope at <cwd>):
//
//	settings.json      — JSONC settings: mcpServers, contextFileName, …
//	GEMINI.md          — memory/instructions          → gocode.md
//	commands/*.toml    — slash-command prompts (TOML)  → <gocode root>/commands/*.md
//
// Mapping into Gocode:
//
//	mcpServers.<name>  → an mcpServers entry (http if httpUrl, sse if url, else stdio)
//	commands/<n>.toml  → <gocode root>/commands/<n>.md, translating Gemini's
//	                     {{args}} → $ARGUMENTS, !{cmd} → !`cmd`, @{path} → @path
//	GEMINI.md          → appended to gocode.md
//
// Gemini's !{…} shell blocks are translated to Gocode's `` !`cmd` `` only when
// the translation is provably equivalent; otherwise the command is left for
// manual porting. Translated shell-exec commands are marked allowed-tools:[Bash,
// PowerShell] + disable-model-invocation so only the user can run them, as in
// Gemini.

type geminiImporter struct{}

func (geminiImporter) ID() Source          { return SourceGemini }
func (geminiImporter) DisplayName() string { return "Google Gemini CLI" }

// geminiHome returns the Gemini config dir.
func geminiHome(opts Options) string { return opts.sourceHome(".gemini") }

func (geminiImporter) Detect(opts Options) (bool, error) {
	if opts.detectRedirected() {
		return true, nil
	}
	home := geminiHome(opts)
	cwd := opts.cwd()
	if fileExists(filepath.Join(home, "settings.json")) ||
		fileExists(filepath.Join(home, "GEMINI.md")) ||
		fileExists(filepath.Join(home, "commands")) ||
		fileExists(filepath.Join(cwd, "GEMINI.md")) ||
		fileExists(filepath.Join(cwd, ".gemini")) {
		return true, nil
	}
	return false, nil
}

func (g geminiImporter) Import(opts Options) (ImportResult, error) {
	res := ImportResult{SourceID: SourceGemini, DisplayName: g.DisplayName()}
	home := geminiHome(opts)
	cwd := opts.cwd()

	if opts.userScopeRedirected() {
		res.unmap(ScopeUser, "Gemini user-scope config", redirectedReason)
		opts.Scope = ScopeProject
	}
	if opts.scopeAllows(ScopeUser) {
		g.scanSettings(home, opts, &res)
		g.scanCommands(home, opts, &res)
	}
	projGemini := filepath.Join(cwd, ".gemini")
	if opts.scopeAllows(ScopeProject) && projGemini != home &&
		containedNoSymlink(cwd, projGemini) != "" && fileExists(filepath.Join(projGemini, "settings.json")) {
		res.unmap(ScopeProject, ".gemini/settings.json", "Project-level Gemini settings are not auto-imported yet. Review it manually.")
	}
	g.scanInstructions(home, cwd, opts, &res)
	g.scanProjectExtras(cwd, opts, &res)
	return res, nil
}

// scanSettings maps settings.json mcpServers.
func (geminiImporter) scanSettings(home string, opts Options, res *ImportResult) {
	path := filepath.Join(home, "settings.json")
	raw, err := readFileLimited(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		res.unmap(ScopeUser, "~/.gemini/settings.json", "Could not read. Review your MCP servers there manually.")
		return
	}
	var settings map[string]any
	if err := parseJSONC(raw, &settings); err != nil {
		res.unmap(ScopeUser, "~/.gemini/settings.json", "Could not parse (unexpected structure). Review your MCP servers there manually.")
		return
	}
	servers := asNamedTable(settings["mcpServers"])
	if servers == nil {
		if settings["mcpServers"] != nil {
			res.unmap(ScopeUser, "~/.gemini/settings.json",
				"Has an unexpected shape in ~/.gemini/settings.json (expected an object of named entries). Review it manually.")
		}
	}
	for _, name := range sortedKeys(servers) {
		entry, ok := servers[name].(map[string]any)
		if !ok {
			res.unmap(ScopeUser, `MCP server "`+name+`"`, "Entry in ~/.gemini/settings.json has an unexpected shape. Review it manually.")
			continue
		}
		httpURL, _ := entry["httpUrl"].(string)
		url, _ := entry["url"].(string)
		command, _ := entry["command"].(string)
		if httpURL == "" && url == "" && command == "" {
			res.unmap(ScopeUser, `MCP server "`+name+`"`, "Has neither a url nor a command — nothing to import. Review it manually.")
			continue
		}
		safe := safeName(name)
		cfg := geminiMCPConfig(entry)
		res.add(ImportItem{
			ID:             "gemini:user:mcp:" + name,
			Kind:           KindMCP,
			Scope:          ScopeUser,
			Label:          `MCP server "` + name + `"`,
			Fingerprint:    jsonFingerprint(cfg),
			MCP:            &cfg,
			MCPTargetScope: ScopeUser,
			TargetPath:     mcpTargetPath(ScopeUser, opts.cwd(), safe),
			applyKind:      applyMCP,
			msgName:        safe,
		})
	}
	// Report unknown top-level keys (minus mcpServers/model).
	known := map[string]bool{"mcpServers": true, "model": true}
	var extra []string
	for k := range settings {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		res.unmap(ScopeUser, "settings.json keys: "+strings.Join(sortStrings(extra), ", "),
			"No direct gocode equivalent, or cosmetic. Review manually if you relied on them.")
	}
}

// geminiMCPConfig maps a Gemini server entry to a Gocode ServerConfig.
func geminiMCPConfig(entry map[string]any) mcp.ServerConfig {
	var cfg mcp.ServerConfig
	httpURL, _ := entry["httpUrl"].(string)
	url, _ := entry["url"].(string)
	switch {
	case httpURL != "":
		cfg = mcp.ServerConfig{Type: mcp.TransportHTTP, URL: httpURL, Headers: toStringMap(entry["headers"])}
	case url != "":
		cfg = mcp.ServerConfig{Type: mcp.TransportSSE, URL: url, Headers: toStringMap(entry["headers"])}
	default:
		command, _ := entry["command"].(string)
		cfg = mcp.ServerConfig{Type: mcp.TransportStdio, Command: command, Args: toStringSlice(entry["args"]), Env: toStringMap(entry["env"])}
	}
	if t := asInt(entry["timeout"]); t > 0 {
		cfg.Timeout = t
	}
	return cfg
}

// scanCommands maps commands/*.toml → user commands.
func (geminiImporter) scanCommands(home string, opts Options, res *ImportResult) {
	dir := filepath.Join(home, "commands")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var namespaced []string
	for _, de := range ents {
		if de.IsDir() {
			namespaced = append(namespaced, de.Name())
		}
	}
	if len(namespaced) > 0 {
		res.unmap(ScopeUser, "namespaced commands ("+strings.Join(namespaced, ", ")+")",
			"Subdirectory-organized commands are not auto-imported yet.")
	}
	for _, de := range ents {
		if !de.Type().IsRegular() || !strings.HasSuffix(de.Name(), ".toml") {
			continue
		}
		base := strings.TrimSuffix(de.Name(), ".toml")
		safe := safeName(base)
		src := filepath.Join(dir, de.Name())
		target := filepath.Join(opts.gocodeDir(), "commands", safe+".md")
		raw, err := readFileLimited(src)
		if err != nil {
			res.unmap(ScopeUser, "Command /"+safe, "Could not parse as TOML. Review it manually.")
			continue
		}
		parsed, err := parseTOML(raw)
		if err != nil {
			res.unmap(ScopeUser, "Command /"+safe, "Could not parse as TOML. Review it manually.")
			continue
		}
		prompt, ok := parsed["prompt"].(string)
		if !ok {
			res.unmap(ScopeUser, "Command /"+safe,
				"Doesn't match the expected command format (needs a 'prompt' string). Review it manually.")
			continue
		}
		description, _ := parsed["description"].(string)
		tr := translateGeminiPrompt(prompt)
		if tr.untranslatable != "" {
			res.unmap(ScopeUser, "Command /"+safe, tr.untranslatable)
			continue
		}
		var fields []fmField
		if description != "" {
			fields = append(fields, fmField{"description", emDash(description)})
		}
		if tr.hasShellExec {
			fields = append(fields,
				fmField{"allowed-tools", []string{"Bash", "PowerShell"}},
				fmField{"disable-model-invocation", true})
		}
		var head string
		if len(fields) > 0 {
			head = "---\n" + emitFrontmatter(fields) + "---\n\n"
		}
		sep := ""
		if head == "" && strings.HasPrefix(strings.TrimLeft(tr.body, " \t"), "---") {
			sep = "\n"
		}
		content := head + sep + tr.body + "\n"
		item := ImportItem{
			ID:          "gemini:user:command:" + base,
			Kind:        KindCommand,
			Scope:       ScopeUser,
			Label:       "Command /" + safe,
			Description: "→ " + target,
			Fingerprint: jsonFingerprint([]any{tr.body, description, tr.hasShellExec}),
			Content:     content,
			SourcePath:  src,
			TargetPath:  target,
			applyKind:   applyNewFile,
			msgName:     safe,
		}
		if tr.hasShellExec {
			item.Warning = "Uses `!{cmd}` shell exec — import grants the command `allowed-tools: [Bash, PowerShell]` (any shell command, not just the named ones) and marks it `disable-model-invocation` so, like the Gemini original, only you can run it via `/name`. Unchecked by default; review before importing."
		}
		res.add(item)
	}
}

// scanInstructions maps GEMINI.md (user + project) → gocode.md.
func (geminiImporter) scanInstructions(home, cwd string, opts Options, res *ImportResult) {
	type src struct {
		id, path, target string
		scope            Scope
	}
	gocode := opts.configDir()
	list := []src{
		{"gemini:user:instructions", filepath.Join(home, "GEMINI.md"), filepath.Join(gocode, "gocode.md"), ScopeUser},
		{"gemini:project:instructions", filepath.Join(cwd, "GEMINI.md"), filepath.Join(cwd, "gocode.md"), ScopeProject},
	}
	for _, s := range list {
		if opts.Scope != "" && opts.Scope != s.scope {
			continue
		}
		if !fileExists(s.path) {
			continue
		}
		var content string
		if s.scope == ScopeUser {
			b, err := readFileLimited(s.path)
			if err != nil {
				res.unmap(ScopeUser, "GEMINI.md", "Could not read. Review it manually.")
				continue
			}
			content = b
		}
		res.add(ImportItem{
			ID:                s.id,
			Kind:              KindInstructions,
			Scope:             s.scope,
			Label:             "GEMINI.md",
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

// scanProjectExtras reports project-level Gemini artifacts with no auto-import.
func (geminiImporter) scanProjectExtras(cwd string, opts Options, res *ImportResult) {
	if opts.Scope == ScopeUser {
		return
	}
	if fileExists(filepath.Join(cwd, ".gemini", "system.md")) {
		res.unmap(ScopeProject, ".gemini/system.md",
			"Gemini system.md replaces the system prompt; gocode output-styles augment it. Review and add as an output-style manually if wanted.")
	}
	if fileExists(filepath.Join(cwd, "gemini-extension.json")) {
		res.unmap(ScopeProject, "gemini-extension.json",
			"Gemini extensions map to gocode plugins. Not auto-converted yet.")
	}
}

// --- Gemini prompt translation ---------------------------------------------

type geminiTranslation struct {
	body           string
	hasShellExec   bool
	untranslatable string
}

var (
	reShellBlock = regexp.MustCompile(`!\{[^}]+\}`)
	reShellInner = regexp.MustCompile(`!\{([^}]+)\}`)
	rePathInner  = regexp.MustCompile(`@\{([^}]+)\}`)
	reTransMark  = regexp.MustCompile("!`[^`]*`")
)

// translateGeminiPrompt converts a Gemini command prompt into a Gocode command
// body, refusing (untranslatable) any form whose shell semantics cannot be
// preserved exactly.
func translateGeminiPrompt(prompt string) geminiTranslation {
	e := strings.ReplaceAll(prompt, "{{args}}", "$ARGUMENTS")
	blocks := reShellBlock.FindAllString(e, -1)

	for _, m := range reShellInner.FindAllStringSubmatch(e, -1) {
		inner := m[1]
		if strings.Contains(inner, "{") {
			return geminiTranslation{body: e, hasShellExec: true,
				untranslatable: "Its `!{…}` shell block contains nested braces, which this importer would truncate. Port it manually."}
		}
		if strings.Contains(inner, "`") {
			return geminiTranslation{body: e, hasShellExec: true,
				untranslatable: "Its `!{…}` shell block contains a backtick, which gocode's `` !`cmd` `` syntax can't represent. Port it manually."}
		}
	}
	for _, m := range rePathInner.FindAllStringSubmatch(e, -1) {
		if strings.Contains(m[1], "{") {
			return geminiTranslation{body: e,
				untranslatable: "Its `@{…}` path contains a nested brace, which this importer would truncate. Port it manually."}
		}
	}
	for _, loc := range reShellBlock.FindAllStringIndex(e, -1) {
		if loc[0] > 0 {
			prev := e[loc[0]-1]
			if !(prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == '\f' || prev == '\v') {
				return geminiTranslation{body: e, hasShellExec: true,
					untranslatable: "Its `!{…}` block isn't preceded by whitespace — gocode's `` !`cmd` `` marker requires it, so the shell exec would be silently lost. Port it manually."}
			}
		}
	}
	if strings.Contains(reShellBlock.ReplaceAllString(e, ""), "!`") {
		return geminiTranslation{body: e, hasShellExec: true,
			untranslatable: "It has a literal `` !` `` outside any `!{…}` block (inert in Gemini, live in gocode — and it may pair with a translated block's backtick to run something other than the block). Port it manually."}
	}

	var blockCmds []string
	for _, m := range reShellInner.FindAllStringSubmatch(e, -1) {
		if c := strings.TrimSpace(m[1]); c != "" {
			blockCmds = append(blockCmds, c)
		}
	}

	s := rePathInner.ReplaceAllStringFunc(e, func(m string) string {
		inner := rePathInner.FindStringSubmatch(m)[1]
		return "@" + strings.ReplaceAll(inner, " ", `\ `)
	})
	s = reShellInner.ReplaceAllString(s, "!`$1`")

	if strings.Contains(s, "```!") {
		return geminiTranslation{body: e, hasShellExec: true,
			untranslatable: "Its body contains a ```! fenced shell block (inert in Gemini, live in gocode). Port it manually."}
	}
	translated := reTransMark.FindAllString(s, -1)
	if len(translated) > len(blocks) {
		return geminiTranslation{body: e, hasShellExec: true,
			untranslatable: "Its translated body contains a `` !`cmd` `` shell-exec marker that wasn't a `!{…}` block in the Gemini prompt (inert there, live in gocode). Port it manually."}
	}

	parsedCmds := extractTranslatedCommands(s)
	if diff := subtract(parsedCmds, blockCmds); len(diff) > 0 {
		return geminiTranslation{body: e, hasShellExec: true,
			untranslatable: "Its translated body would execute a shell command that wasn't a `!{…}` block in the Gemini prompt (inert there, live in gocode). Port it manually."}
	}
	if diff := subtract(blockCmds, parsedCmds); len(diff) > 0 {
		return geminiTranslation{body: e, hasShellExec: true,
			untranslatable: "A `!{…}` shell block would be silently dropped by gocode's `` !`cmd` `` parsing after translation (its marker re-pairs with nearby backticks). Port it manually."}
	}
	if reason := validateArgPlaceholders(s, blockCmds); reason != "" {
		return geminiTranslation{body: e, hasShellExec: true, untranslatable: reason}
	}
	return geminiTranslation{body: s, hasShellExec: len(blockCmds) > 0}
}

// extractTranslatedCommands returns the commands inside the translated
// body's “ !`...` “ markers.
func extractTranslatedCommands(s string) []string {
	var out []string
	for _, m := range reTransMark.FindAllString(s, -1) {
		inner := strings.TrimSpace(strings.Trim(m, "!`"))
		if inner != "" {
			out = append(out, inner)
		}
	}
	return out
}

// subtract returns the multiset a minus b.
func subtract(a, b []string) []string {
	rem := append([]string(nil), b...)
	var out []string
	for _, x := range a {
		idx := -1
		for i, y := range rem {
			if y == x {
				idx = i
				break
			}
		}
		if idx == -1 {
			out = append(out, x)
		} else {
			rem = append(rem[:idx], rem[idx+1:]...)
		}
	}
	return out
}

func asNamedTable(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	}
	return 0
}

func sortStrings(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
