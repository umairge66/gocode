package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/projectmemory"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/ui"
)

// /memory: pick one of the gocode.md memory files this session reads — the
// user file, the project file, and every other file loaded at startup (a
// .gocode/gocode.md, gocode.local.md, rules, @-imports) — and open it in the
// external editor. The user and project files are offered even when they do
// not exist yet; choosing one creates it empty first. --setting-sources gates
// the list as it gates the session's memory loading (buildSystemPrompt): a
// file of a source the session does not read is not offered.

// memoryFile is one row of the /memory picker.
type memoryFile struct {
	path   string
	label  string
	detail string
}

// memoryFiles lists the memory files for the picker: the user file, the
// project file, then the other loaded files in load order (imports indented
// under the file that imports them). Managed (organization) files are not
// listed: they are not the user's to edit.
func memoryFiles(env *Env) []memoryFile {
	paths := resolveSessionPaths(env)
	skipUser := !settingSourceEnabled(env, settingspolicy.SourceUser)
	skipProject := !settingSourceEnabled(env, settingspolicy.SourceProject)
	var userPath, projectPath string
	if paths.configDir != "" && !skipUser {
		userPath = filepath.Join(paths.configDir, "gocode.md")
	}
	if paths.cwd != "" && !skipProject {
		projectPath = filepath.Join(paths.cwd, "gocode.md")
	}
	var entries []projectmemory.Entry
	if !envutil.EnvTruthy(envGet(env)(disableMemoryEnv)) {
		entries = projectmemory.Load(projectmemory.Config{
			Cwd:         paths.cwd,
			Home:        paths.home,
			ConfigDir:   paths.configDir,
			FS:          fsfacade.OSFS{},
			SkipUser:    skipUser,
			SkipProject: skipProject,
			SkipLocal:   !settingSourceEnabled(env, settingspolicy.SourceLocal),
		}).Entries
	}

	var out []memoryFile
	seen := map[string]bool{}
	add := func(f memoryFile) {
		key := filepath.Clean(f.path)
		if f.path == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	}
	if userPath != "" {
		add(memoryFile{path: userPath, label: "User memory", detail: "Saved in " + displayPath(env, userPath)})
	}
	if projectPath != "" {
		verb := "Saved in"
		if paths.cwd != "" && isGitRepo(paths.cwd) {
			verb = "Checked in at"
		}
		add(memoryFile{path: projectPath, label: "Project memory", detail: verb + " ./gocode.md"})
	}
	depth := map[string]int{}
	for _, e := range entries {
		if e.Kind == projectmemory.KindManaged {
			continue
		}
		p := filepath.FromSlash(e.Path)
		label := displayPath(env, p)
		detail := ""
		switch e.Kind {
		case projectmemory.KindLocal:
			detail = "Local (not checked in)"
		case projectmemory.KindUser:
			detail = "User"
		case projectmemory.KindProject:
			detail = "Project"
		}
		if e.Parent != "" {
			d := depth[e.Parent] + 1
			depth[e.Path] = d
			label = strings.Repeat("  ", d-1) + "└ " + label
			detail = "@-imported"
		}
		add(memoryFile{path: p, label: label, detail: detail})
	}
	return out
}

// displayPath shortens a path for display: "~/…" under the home directory,
// "./…" under the working directory, whichever is shorter.
func displayPath(env *Env, p string) string {
	paths := resolveSessionPaths(env)
	var home, rel string
	if paths.home != "" {
		if r, err := filepath.Rel(paths.home, p); err == nil && !strings.HasPrefix(r, "..") {
			home = filepath.ToSlash(filepath.Join("~", r))
		}
	}
	if paths.cwd != "" {
		if r, err := filepath.Rel(paths.cwd, p); err == nil && !strings.HasPrefix(r, "..") {
			rel = "./" + filepath.ToSlash(r)
		}
	}
	switch {
	case home != "" && rel != "":
		if len(home) <= len(rel) {
			return home
		}
		return rel
	case home != "":
		return home
	case rel != "":
		return rel
	}
	return p
}

// memory runs /memory: the picker, then the editor on the chosen file.
func (d *interactiveDialogs) memory() {
	files := memoryFiles(d.env)
	items := make([]ui.ListItem, len(files))
	for i, f := range files {
		items[i] = ui.ListItem{Label: f.label, Value: f.path, Detail: f.detail}
	}
	var body []string
	if envutil.EnvTruthy(envGet(d.env)(disableMemoryEnv)) {
		body = append(body, "gocode.md files aren't loaded into this session ("+disableMemoryEnv+" is set). You can still edit them.")
	}
	body = append(body, "Choose a memory file to open in your editor.")
	r, ok := d.openAndWait(context.Background(), ui.ListDialog("Memory", items, 12, body...))
	if !ok || r.cancelled {
		d.sendMsg(ui.AppendSystemMsg{Text: "Cancelled memory editing"})
		return
	}
	path := firstChosen(r.values)
	if path == "" {
		d.sendMsg(ui.AppendSystemMsg{Text: "Cancelled memory editing"})
		return
	}
	d.sendMsg(ui.AppendSystemMsg{Text: d.openMemoryFile(path)})
}

// openMemoryFile creates path when missing and opens it in the editor,
// returning the message to show.
func (d *interactiveDialogs) openMemoryFile(path string) string {
	shown := displayPath(d.env, path)
	if err := ensureFile(path); err != nil {
		return "Couldn't open " + shown + ": " + err.Error()
	}
	c, err := d.openInEditor(path)
	if err != nil {
		return "Couldn't open " + shown + " in an editor. If no editor is configured, set $EDITOR or $VISUAL, then run /memory again."
	}
	return "Opened " + shown + "\n\nMemory changes take effect in the next session.\n\n" + editorHint(c)
}

// ensureFile creates an empty file at path (and its directory) unless it
// already exists.
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return f.Close()
}
