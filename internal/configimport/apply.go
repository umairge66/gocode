package configimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/mcp"
	"github.com/umairge66/gocode/internal/settingsload"
)

// Apply: the write half of the importer.
//
// Each mapped item carries an apply plan (applyKind + a few unexported fields
// set at scan time). Apply performs it: it returns a success message (or, for a dry run,
// the "would …" message) or a skip reason, and an error only for an unexpected
// I/O failure. Every check that guards a write re-runs at apply time against
// the live filesystem — the scan may be stale.
//
// Write targets are where the rest of gocode READS them:
//
//	instructions → <ConfigDir>/gocode.md (user) / <cwd>/gocode.md, gocode.local.md
//	               (project) — internal/projectmemory.Load
//	setting      → <ConfigDir>/settings.json (user) / <cwd>/.gocode/settings.json
//	               (project) — internal/settingsload
//	mcp          → mcpServers in <ConfigDir>/settings.json (user; mcphost reads the
//	               userSettings table) / <cwd>/.mcp.json (project; mcphost
//	               readProjectFiles, still subject to project-server approval)
//	command      → <GocodeDir>/commands/*.md, <cwd>/.gocode/commands — commands.LoadDir
//	skill        → <GocodeDir>/skills/<name>/, <cwd>/.gocode/skills — skills.Load
//	subagent     → <GocodeDir>/agents/*.md, <cwd>/.gocode/agents — agentsdef.Load

type applyKind int

const (
	applyNone     applyKind = iota
	applyAppend             // append to a memory file with an imported-from marker
	applyNewFile            // subagent / gemini command — write a new file, skip if present
	applyCopyFile           // codex prompt / cursor command — same, "copy" wording
	applySkillDir           // copy a skill directory, rewriting SKILL.md
	applyMCP                // add an MCP server entry
	applySetting            // merge a settings patch
)

// ApplyResult is the outcome of applying one item.
type ApplyResult struct {
	// Message is the human-readable result: "wrote `…`", "would add MCP server
	// …", or — when Skipped — the reason it was not applied.
	Message string
	// Skipped is true when the item was deliberately not applied (already
	// present, guard refused, …). Skips are not errors.
	Skipped bool
}

func done(msg string) (ApplyResult, error)    { return ApplyResult{Message: msg}, nil }
func skipped(msg string) (ApplyResult, error) { return ApplyResult{Message: msg, Skipped: true}, nil }

// Apply performs (or, with dryRun, describes) the write for one scanned item.
// opts must be the Options the item was scanned with.
func Apply(it ImportItem, opts Options, dryRun bool) (ApplyResult, error) {
	switch it.applyKind {
	case applyAppend:
		return applyMemoryAppend(it, dryRun)
	case applyNewFile, applyCopyFile:
		return applyWriteFile(it, dryRun)
	case applySkillDir:
		return applySkillCopy(it, dryRun)
	case applyMCP:
		return applyMCPServer(it, opts, dryRun)
	case applySetting:
		return applySettingPatch(it, opts, dryRun)
	}
	return ApplyResult{}, fmt.Errorf("item %q has no apply plan", it.ID)
}

// --- memory append ---------------------------------------------------------

var reRelativeAtImport = regexp.MustCompile(`(?:^|\s)@[^/~@\s\\]`)

func applyMemoryAppend(it ImportItem, dryRun bool) (ApplyResult, error) {
	target, src := it.TargetPath, it.SourcePath
	tb := filepath.Base(target)
	if it.containIn != "" {
		if containedNoSymlink(it.containIn, target) == "" {
			return skipped(tb + ": target is (or is under) a symlink — refusing project-scope write")
		}
		if containedNoSymlink(it.containIn, src) == "" {
			return skipped(tb + ": source is (or is under) a symlink — refusing project-scope read")
		}
	}
	marker := "<!-- imported-from: " + it.marker + " -->"
	existing := ""
	if b, err := readFileLimited(target); err == nil {
		existing = b
		if strings.Contains(existing, marker) {
			return skipped(tb + ": already imported (marker present; not re-synced)")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ApplyResult{}, err
	}
	if dryRun {
		return done("would append " + filepath.Base(src) + " → `" + target + "`")
	}
	body := it.Content
	if body == "" {
		b, err := readFileLimited(src)
		if err != nil {
			return ApplyResult{}, err
		}
		body = b
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ApplyResult{}, err
	}
	prefix := ""
	if existing != "" {
		prefix = existing
		if !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		prefix += "\n"
	}
	if err := writeFileAtomic(target, prefix+marker+"\n"+strings.TrimRight(body, " \t\r\n")+"\n"); err != nil {
		return ApplyResult{}, err
	}
	note := ""
	if filepath.Dir(src) != filepath.Dir(target) && reRelativeAtImport.MatchString(body) {
		note = " — note: relative @imports in this file now resolve against the new location; convert them to absolute paths if they stop working"
	}
	return done("appended " + filepath.Base(src) + " → `" + target + "`" + note)
}

// --- new file (subagent, commands) ------------------------------------------

func applyWriteFile(it ImportItem, dryRun bool) (ApplyResult, error) {
	target := it.TargetPath
	if it.containIn != "" && containedNoSymlink(it.containIn, target) == "" {
		return skipped(it.msgName + ": target is under a symlink — refusing project-scope write")
	}
	if it.Kind == KindSubagent {
		// Re-check the shell-exec gates at write time.
		if hasShellExecMarker(it.Content) {
			return skipped(it.msgName + ": body contains a ```! or `` !`…` `` shell-exec marker (inert in Codex, live in gocode) — port it manually")
		}
		if reason := validateArgPlaceholders(it.Content, nil); reason != "" {
			return skipped(it.msgName + ": " + reason)
		}
	}
	existing := it.existsPath
	if existing == "" {
		existing = target
	}
	if fileExists(existing) {
		return skipped(it.msgName + ": `" + existing + "` already exists")
	}
	if dryRun {
		if it.applyKind == applyCopyFile {
			return done("would copy `" + it.SourcePath + "` → `" + target + "`")
		}
		return done("would write `" + target + "`" + it.msgSuffix)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ApplyResult{}, err
	}
	if err := writeFileExclusive(target, it.Content); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return skipped(it.msgName + ": `" + target + "` already exists")
		}
		return ApplyResult{}, err
	}
	if it.applyKind == applyCopyFile {
		return done("copied → `" + target + "`")
	}
	return done("wrote `" + target + "`" + it.msgSuffix)
}

// --- skill directory copy --------------------------------------------------

// pluginShapedDirs are skill subdirectories gocode would adopt as a plugin.
var pluginShapedDirs = map[string]bool{
	".gocode-plugin": true, "agents": true, "output-styles": true, "themes": true,
	"hooks": true, "monitors": true, "workflows": true,
}

func applySkillCopy(it ImportItem, dryRun bool) (ApplyResult, error) {
	n := it.msgName
	// Resolve the source directory with a containment guard: a
	// project-scope source must not cross a symlink; a user-scope source must
	// really live under the tool's directory.
	var real string
	if it.Scope == ScopeProject {
		real = containedNoSymlink(it.skillRoot, it.skillPath)
	} else {
		real = containedReal(it.skillRoot, it.skillPath)
	}
	if real == "" {
		return skipped(n + ": path escapes `" + it.skillRoot + "` (symlink) or is missing")
	}
	res, err := copySkillDir(it, real, dryRun)
	if err != nil || res.Skipped {
		return res, err
	}
	res.Message += it.msgSuffix
	return res, nil
}

func copySkillDir(it ImportItem, real string, dryRun bool) (ApplyResult, error) {
	n, target := it.msgName, it.skillDir
	if isReservedSkillsDir(n) {
		return skipped(n + `: "` + n + `" is a reserved skills directory name (the gocode.ai skills-sync root) and would never load — rename the skill directory and import it manually`)
	}
	skillMd := filepath.Join(real, "SKILL.md")
	st, err := os.Lstat(skillMd)
	if err != nil {
		return skipped(n + ": no SKILL.md at `" + real + "`")
	}
	if !st.Mode().IsRegular() {
		return skipped(n + ": SKILL.md is a symlink — copy the skill manually")
	}
	if st.Size() > SkillMaxBytes {
		return skipped(n + ": SKILL.md is " + strconv.FormatInt(st.Size(), 10) + " bytes — gocode skips skills over " + strconv.Itoa(SkillMaxBytes) + " bytes, so the copy would never load")
	}
	raw, err := readFileLimited(skillMd)
	if err != nil {
		return skipped(n + ": SKILL.md could not be read")
	}
	out, skip := it.prepareSkill(raw)
	if skip != "" {
		return skipped(skip)
	}
	if len(out) > SkillMaxBytes {
		return skipped(n + ": SKILL.md would be " + strconv.Itoa(len(out)) + " bytes after utf-8 re-encoding — gocode skips skills over " + strconv.Itoa(SkillMaxBytes) + " bytes, so the copy would never load")
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return skipped(n + ": skill directory could not be read")
	}
	var plugin []string
	variants := 0
	for _, e := range ents {
		folded := normalizeDirName(e.Name())
		if pluginShapedDirs[folded] {
			plugin = append(plugin, "`"+e.Name()+"/`")
		}
		if folded == "skill.md" {
			variants++
		}
	}
	if len(plugin) > 0 {
		return skipped(n + ": skill directory contains " + strings.Join(plugin, ", ") + " which gocode would adopt as a plugin (lifecycle hooks, monitors, MCP servers) — copy it manually after reviewing those")
	}
	if variants > 1 {
		return skipped(n + ": skill directory has multiple SKILL.md case/encoding variants — copy the skill manually")
	}
	if it.containIn != "" && containedNoSymlink(it.containIn, target) == "" {
		return skipped(n + ": target is under a symlink — refusing project-scope write")
	}
	if fileExists(target) {
		return skipped(n + ": `" + target + "` already exists")
	}
	if dryRun {
		return done("would copy `" + real + "` → `" + target + "`")
	}
	err = copyTree(real, target, func(name string) bool { return normalizeDirName(name) == "skill.md" })
	if err == nil {
		err = os.WriteFile(filepath.Join(target, "SKILL.md"), []byte(out), 0o644)
	}
	if err != nil {
		_ = os.RemoveAll(target)
		return ApplyResult{}, err
	}
	return done("copied skill → `" + target + "`")
}

// copyTree copies regular files and directories from src to dst (symlinks and
// special files are not followed or copied), skipping top-level names for
// which skip returns true.
func copyTree(src, dst string, skip func(string) bool) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if skip != nil && skip(e.Name()) {
			continue
		}
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case e.IsDir():
			if err := copyTree(s, d, nil); err != nil {
				return err
			}
		case e.Type().IsRegular():
			if err := copyFile(s, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(src); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(dst, b, mode)
}

// --- MCP servers -----------------------------------------------------------

var reMCPName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// userSettingsPath / projectSettingsPath are the settings files the settings
// loader reads for the user and project sources.
func (o Options) userSettingsPath() string { return filepath.Join(o.configDir(), "settings.json") }
func (o Options) projectSettingsPath() string {
	return filepath.Join(o.cwd(), ".gocode", "settings.json")
}

func applyMCPServer(it ImportItem, opts Options, dryRun bool) (ApplyResult, error) {
	name := it.msgName
	scope := it.MCPTargetScope
	if scope != ScopeProject {
		scope = ScopeUser
	}
	if it.MCP == nil {
		return ApplyResult{}, fmt.Errorf("MCP item %q has no server config", it.ID)
	}
	cwd := opts.cwd()
	mcpPath := filepath.Join(cwd, ".mcp.json")
	if scope == ScopeProject && containedNoSymlink(cwd, mcpPath) == "" {
		return skipped(name + ": .mcp.json is (or is under) a symlink — refusing project-scope write")
	}
	var existing map[string]any
	var err error
	if scope == ScopeUser {
		existing, err = readSettingsServers(opts.userSettingsPath())
	} else {
		existing, err = readMCPJSONServers(mcpPath)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	if _, ok := existing[name]; ok {
		return skipped(name + ": MCP server already exists in " + string(scope) + " config")
	}
	if dryRun {
		return done("would add MCP server " + name + " (" + string(it.Scope) + ")" + it.msgSuffix)
	}
	if !reMCPName.MatchString(name) {
		return ApplyResult{}, fmt.Errorf("Invalid name %s. Names can only contain letters, numbers, hyphens, and underscores.", name)
	}
	entry, err := serverEntry(*it.MCP)
	if err != nil {
		return ApplyResult{}, err
	}
	insert := func(obj map[string]any) (bool, error) {
		servers, ok := obj["mcpServers"].(map[string]any)
		if obj["mcpServers"] != nil && !ok {
			return false, errors.New("mcpServers is not an object; refusing to overwrite it")
		}
		if servers == nil {
			servers = map[string]any{}
		}
		if _, dup := servers[name]; dup {
			return false, nil // raced in since the check: never overwrite
		}
		servers[name] = entry
		obj["mcpServers"] = servers
		return true, nil
	}
	if scope == ScopeUser {
		err = settingsload.UpdateFile(opts.userSettingsPath(), insert)
	} else {
		var obj map[string]any
		obj, err = readJSONObject(mcpPath, ".mcp.json")
		if err == nil {
			var changed bool
			changed, err = insert(obj)
			if err == nil && changed {
				err = settingsload.WriteJSONObjectFile(mcpPath, obj)
			}
		}
	}
	if err != nil {
		return ApplyResult{}, err
	}
	return done("added MCP server " + name + " (" + string(scope) + ")" + it.msgSuffix)
}

// serverEntry renders a ServerConfig as the JSON object stored in config.
func serverEntry(cfg mcp.ServerConfig) (map[string]any, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// readSettingsServers returns the mcpServers table of a settings file (absent
// file → empty).
func readSettingsServers(path string) (map[string]any, error) {
	var servers map[string]any
	err := settingsload.UpdateFile(path, func(obj map[string]any) (bool, error) {
		servers, _ = obj["mcpServers"].(map[string]any)
		return false, nil
	})
	return servers, err
}

// readMCPJSONServers returns the mcpServers table of a .mcp.json (absent → empty).
func readMCPJSONServers(path string) (map[string]any, error) {
	obj, err := readJSONObject(path, ".mcp.json")
	if err != nil {
		return nil, err
	}
	if obj["mcpServers"] == nil {
		return nil, nil
	}
	servers, ok := obj["mcpServers"].(map[string]any)
	if !ok {
		return nil, errors.New(".mcp.json is malformed (not valid JSON, or mcpServers is not an object)")
	}
	return servers, nil
}

func readJSONObject(path, label string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, errors.New(label + " is malformed (not valid JSON, or mcpServers is not an object)")
	}
	return obj, nil
}

// --- settings --------------------------------------------------------------

func applySettingPatch(it ImportItem, opts Options, dryRun bool) (ApplyResult, error) {
	if it.Setting == nil {
		return ApplyResult{}, fmt.Errorf("setting item %q has no patch", it.ID)
	}
	mode := it.Setting.PermissionsDefaultMode
	if dryRun {
		return done("would set permissions.defaultMode=" + mode)
	}
	path, source := opts.userSettingsPath(), "userSettings"
	if it.Scope == ScopeProject {
		cwd := opts.cwd()
		if containedNoSymlink(cwd, opts.projectSettingsPath()) == "" {
			return skipped(".gocode/settings.json is (or is under) a symlink — refusing project-scope write")
		}
		path, source = opts.projectSettingsPath(), "projectSettings"
	}
	patch := map[string]any{"permissions": map[string]any{"defaultMode": mode}}
	err := settingsload.UpdateFile(path, func(obj map[string]any) (bool, error) {
		deepMerge(obj, patch)
		return true, nil
	})
	if err != nil {
		return ApplyResult{}, err
	}
	return done("set permissions.defaultMode=" + mode + " in " + source)
}

// deepMerge merges src into dst recursively (objects merge, everything else is
// replaced), preserving keys src does not mention.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				deepMerge(dv, sv)
				continue
			}
			cp := map[string]any{}
			deepMerge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// --- small file helpers ----------------------------------------------------

// writeFileAtomic replaces path's contents via a sibling temp file + rename.
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".import-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(name, st.Mode().Perm())
	} else {
		_ = os.Chmod(name, 0o644)
	}
	return os.Rename(name, path)
}

// writeFileExclusive creates path, failing with fs.ErrExist if it exists (so a
// file that appeared since the existence check is never clobbered).
func writeFileExclusive(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
