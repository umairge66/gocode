package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/settingsload"
)

// `gocode purge [path]` (and the deprecated `gocode project purge [path]`):
// delete all local gocode state for one project, or for every project with
// --all — transcripts (and their memory/), per-session task lists, debug logs
// and file-edit history, the project's entry in the state file
// (<gocode root>/.gocode.json), and the
// project's prompts in history.jsonl.
//
// Scope and safety: every path the plan deletes lies under the gocode data
// directory (the gocode root, internal/confighome — the tree that holds
// projects/, tasks/, debug/, file-history/ and history.jsonl). removeUnder
// re-checks that before each delete, after resolving symlinks in the parent,
// and refuses anything else. The state-file project entry is edited, never
// deleted. Nothing is deleted without the y/N confirmation unless --yes is
// given; --dry-run only prints the plan.
//
// history.jsonl is written directly and never leaves staging copies; the
// rewrite here is an atomic temp-file rename.

// purgeDeprecatedAlias / purgeDescription are the user-facing strings.
const (
	purgeDeprecatedAlias = "`gocode project purge` is now `gocode purge`"
	purgeDescription     = "Delete all gocode state for a project (transcripts, tasks, file history, config entry)"
)

// purgeItem is one entry of a purge plan.
type purgeItem struct {
	Path       string
	Kind       string // dir | file | config-key | history-lines
	Reason     string
	matchPaths []string // history-lines: the project paths whose lines go
}

// purgeEnv carries the resolved locations.
type purgeEnv struct {
	env          *Env
	dataDir      string // the gocode root
	globalConfig string // <gocode root>/.gocode.json
	stdin        *bufio.Reader
}

func newPurgeEnv(env *Env) (*purgeEnv, error) {
	paths := resolveSessionPaths(env)
	dataDir := sessionConfigHome(env, paths)
	if dataDir == "" {
		return nil, errors.New("Could not resolve the gocode data directory (set HOME, XDG_CONFIG_HOME or GOCODE_CONFIG_DIR).")
	}
	dataDir = filepath.Clean(dataDir)
	global := confighome.StatePath(dataDir)
	return &purgeEnv{env: env, dataDir: dataDir, globalConfig: global, stdin: newStdinReader(env)}, nil
}

// runPurgeCmd parses `purge [path]` and runs it. deprecatedAlias marks the
// `project purge` spelling, which warns first.
func runPurgeCmd(env *Env, t []string, deprecatedAlias bool) int {
	var args []string
	if deprecatedAlias {
		args = argsAfter(t, "project", "purge")
	} else {
		args = argsAfter(t, "purge")
	}
	o, err := scanOpts(args, map[string]string{
		"--dry-run": "dryRun", "-y": "yes", "--yes": "yes",
		"-i": "interactive", "--interactive": "interactive", "--all": "all",
	}, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if !deprecatedAlias && len(o.pos) > 1 {
		fmt.Fprintf(env.Stderr, "error: too many arguments for 'purge'. Expected 1 argument but got %d.\n", len(o.pos))
		return 1
	}
	if deprecatedAlias {
		fmt.Fprintln(env.Stderr, purgeDeprecatedAlias+". The old name still works for now.")
	}
	path := ""
	if len(o.pos) > 0 {
		path = o.pos[0]
	}
	pe, err := newPurgeEnv(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	return pe.run(path, o.has("dryRun"), o.has("yes"), o.has("interactive"), o.has("all"))
}

// run plans the purge, confirms it and carries it out.
func (p *purgeEnv) run(path string, dryRun, yes, interactive, all bool) int {
	env := p.env
	if all {
		if path != "" {
			fmt.Fprintln(env.Stderr, "Cannot specify both a path and --all.")
			return 1
		}
		if interactive {
			fmt.Fprintln(env.Stderr, "Cannot use -i/--interactive with --all.")
			return 1
		}
		items, warnings := p.planAll()
		if len(items) == 0 {
			fmt.Fprintf(env.Stderr, "No gocode project state found under %s.\n", p.dataDir)
			return 1
		}
		p.printPlan("all projects", items, warnings)
		if dryRun {
			fmt.Fprintf(env.Stdout, "Dry run: %d item(s) would be deleted.\n", len(items))
			return 0
		}
		if !yes && !confirmYesNoFrom(env, p.stdin, fmt.Sprintf("Delete %d item(s) for ALL projects? This cannot be undone.", len(items))) {
			fmt.Fprintln(env.Stderr, "Aborted.")
			return 1
		}
		var failed []purgeFailure
		for _, it := range items {
			if f := p.apply(it); f != nil {
				failed = append(failed, *f)
			}
		}
		return p.report(failed, fmt.Sprintf("Purged %d item(s) across all projects.", len(items)))
	}

	var target string
	if path != "" {
		target = p.absPath(path)
	} else {
		picked, ok := p.pickProject()
		if !ok {
			fmt.Fprintln(env.Stderr, "Aborted.")
			return 1
		}
		target = picked
	}
	items, warnings := p.planProject(target)
	if len(items) == 0 {
		fmt.Fprintf(env.Stderr, "No gocode project state found for %s under %s.\n", target, p.dataDir)
		return 1
	}
	p.printPlan(target, items, warnings)
	if dryRun {
		fmt.Fprintf(env.Stdout, "Dry run: %d item(s) would be deleted.\n", len(items))
		return 0
	}
	if interactive {
		deleted, all := 0, false
		var failed []purgeFailure
		for i, it := range items {
			choice := "delete"
			if !all {
				choice = p.askItem(i+1, len(items), it)
			}
			switch choice {
			case "abort":
				fmt.Fprintf(env.Stderr, "Aborted. %d item(s) deleted.\n", deleted)
				return 1
			case "skip":
				continue
			case "all":
				all = true
			}
			if f := p.apply(it); f != nil {
				failed = append(failed, *f)
				continue
			}
			deleted++
		}
		msg := fmt.Sprintf("Purged %d/%d item(s) for %s.", deleted, len(items), target)
		if deleted == 0 && len(failed) == 0 {
			fmt.Fprintln(env.Stdout, msg)
			return 0
		}
		return p.report(failed, msg)
	}
	if !yes && !confirmYesNoFrom(env, p.stdin, fmt.Sprintf("Delete %d item(s) for %s? This cannot be undone.", len(items), target)) {
		fmt.Fprintln(env.Stderr, "Aborted.")
		return 1
	}
	var failed []purgeFailure
	for _, it := range items {
		if f := p.apply(it); f != nil {
			failed = append(failed, *f)
		}
	}
	return p.report(failed, fmt.Sprintf("Purged %d item(s) for %s.", len(items), target))
}

// absPath resolves a user-supplied path against the working directory.
func (p *purgeEnv) absPath(path string) string {
	if !filepath.IsAbs(path) {
		if cwd := sessionCwd(p.env); cwd != "" {
			path = filepath.Join(cwd, path)
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// pickProject chooses the current directory or a project known to the state
// file: it prints a numbered list and reads the choice (Enter = the
// current directory; EOF cancels).
func (p *purgeEnv) pickProject() (string, bool) {
	cwd := p.absPath(".")
	options := []string{cwd}
	seen := map[string]bool{cwd: true}
	for _, k := range p.configProjects() {
		if !seen[k] {
			seen[k] = true
			options = append(options, k)
		}
	}
	fmt.Fprintln(p.env.Stdout, "Select a project to purge:")
	for i, o := range options {
		suffix := ""
		if i == 0 {
			suffix = " (current directory)"
		}
		fmt.Fprintf(p.env.Stdout, "  %d. %s%s\n", i+1, o, suffix)
	}
	fmt.Fprintf(p.env.Stdout, "Enter a number [1]: ")
	line, ok := readAnswer(p.stdin)
	if !ok {
		fmt.Fprintln(p.env.Stdout)
		return "", false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return options[0], true
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return "", false
	}
	return options[n-1], true
}

// askItem is the -i per-item prompt (Delete / Skip / Delete this and all
// remaining / Abort; the first option is the default).
func (p *purgeEnv) askItem(n, total int, it purgeItem) string {
	fmt.Fprintf(p.env.Stdout, "[%d/%d] %s\n  1. Delete  2. Skip  3. Delete this and all remaining  4. Abort\nChoice [1]: ", n, total, purgeItemLine(it))
	line, ok := readAnswer(p.stdin)
	if !ok {
		fmt.Fprintln(p.env.Stdout)
		return "abort"
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "1", "d", "delete":
		return "delete"
	case "2", "s", "skip":
		return "skip"
	case "3", "a", "all":
		return "all"
	default:
		return "abort"
	}
}

// planProject plans everything stored for one project path.
func (p *purgeEnv) planProject(target string) (items []purgeItem, warnings []string) {
	match := []string{target}
	if real, err := filepath.EvalSymlinks(target); err == nil && real != target {
		match = append(match, real)
	}
	store := sessionstore.New(p.dataDir)
	projectsRoot := filepath.Join(p.dataDir, "projects")

	// The exact project directories, then sibling directories that share the
	// escaped prefix (a long path's hashed name, a path that differs only in
	// characters the escaping folds) whose transcripts were recorded under the
	// target.
	var dirs []string
	have := map[string]bool{}
	for _, m := range match {
		d := store.ProjectDir(m)
		if isDir(d) && !have[d] {
			have[d] = true
			dirs = append(dirs, d)
		}
	}
	if entries, err := os.ReadDir(projectsRoot); err == nil {
		for _, e := range entries {
			d := filepath.Join(projectsRoot, e.Name())
			if !e.IsDir() || have[d] {
				continue
			}
			prefixed := false
			for _, m := range match {
				if strings.HasPrefix(e.Name(), sessionstore.EscapeCwd(m)+"-") {
					prefixed = true
				}
			}
			if prefixed && transcriptsRecordedUnder(d, match) {
				have[d] = true
				dirs = append(dirs, d)
			}
		}
	}

	sessions := map[string]bool{}
	var sessionIDs []string
	for _, d := range dirs {
		for _, id := range transcriptSessionIDs(d) {
			if !sessions[id] {
				sessions[id] = true
				sessionIDs = append(sessionIDs, id)
			}
		}
	}
	for _, id := range sessionIDs {
		if d := filepath.Join(p.dataDir, "tasks", id); exists(d) {
			items = append(items, purgeItem{Path: d, Kind: "dir", Reason: "tasks for session " + id})
		}
		if f := filepath.Join(p.dataDir, "debug", id+".txt"); exists(f) {
			items = append(items, purgeItem{Path: f, Kind: "file", Reason: "debug log for session " + id})
		}
		if d := filepath.Join(p.dataDir, "file-history", id); exists(d) {
			items = append(items, purgeItem{Path: d, Kind: "dir", Reason: "file edit history for session " + id})
		}
	}
	for _, d := range dirs {
		items = append(items, purgeItem{Path: d, Kind: "dir", Reason: "project transcripts (.jsonl) and memory/"})
	}

	norm := map[string]bool{}
	for _, m := range match {
		norm[trimTrailingSlashes(m)] = true
	}
	for _, k := range p.configProjects() {
		if norm[trimTrailingSlashes(k)] {
			items = append(items, purgeItem{Path: k, Kind: "config-key", Reason: "project entry in the gocode state file .gocode.json (trust, history, MCP servers)"})
		}
	}

	hist := filepath.Join(p.dataDir, "history.jsonl")
	if n := countHistoryLines(hist, match); n > 0 {
		items = append(items, purgeItem{Path: hist, Kind: "history-lines", Reason: fmt.Sprintf("%d prompt(s) typed in this project", n), matchPaths: match})
	}
	if exists(filepath.Join(p.dataDir, "shell-snapshots")) {
		warnings = append(warnings, "shell-snapshots/ are not project-scoped and will not be touched")
	}
	if b := filepath.Join(p.dataDir, "backups"); exists(b) {
		warnings = append(warnings, fmt.Sprintf("backups/ may still contain this project entry in old .gocode.json snapshots (%s); at most 5 are kept and they rotate out automatically", b))
	}
	return items, warnings
}

// planAll plans every project's state.
func (p *purgeEnv) planAll() (items []purgeItem, warnings []string) {
	for _, d := range [][2]string{
		{"projects", "all project transcripts (.jsonl) and memory/"},
		{"tasks", "all session task lists"},
		{"debug", "all session debug logs"},
		{"file-history", "all session file edit history"},
	} {
		if path := filepath.Join(p.dataDir, d[0]); exists(path) {
			items = append(items, purgeItem{Path: path, Kind: "dir", Reason: d[1]})
		}
	}
	if f := filepath.Join(p.dataDir, "history.jsonl"); exists(f) {
		items = append(items, purgeItem{Path: f, Kind: "file", Reason: "prompt history across all projects"})
	}
	for _, k := range p.configProjects() {
		items = append(items, purgeItem{Path: k, Kind: "config-key", Reason: "project entry in the gocode state file .gocode.json (trust, history, MCP servers)"})
	}
	if exists(filepath.Join(p.dataDir, "shell-snapshots")) {
		warnings = append(warnings, "shell-snapshots/ are not project-scoped and will not be touched")
	}
	if b := filepath.Join(p.dataDir, "backups"); exists(b) {
		warnings = append(warnings, fmt.Sprintf("backups/ may still contain project entries in old .gocode.json snapshots (%s); at most 5 are kept and they rotate out automatically", b))
	}
	return items, warnings
}

// printPlan prints the plan and its warnings.
func (p *purgeEnv) printPlan(label string, items []purgeItem, warnings []string) {
	fmt.Fprintf(p.env.Stdout, "\nPurge plan for %s:\n\n", label)
	for _, it := range items {
		fmt.Fprintf(p.env.Stdout, "  %s\n", purgeItemLine(it))
	}
	if len(warnings) > 0 {
		fmt.Fprintln(p.env.Stdout)
		for _, w := range warnings {
			fmt.Fprintln(p.env.Stderr, w)
		}
	}
}

func purgeItemLine(it purgeItem) string {
	var head string
	switch it.Kind {
	case "config-key":
		head = fmt.Sprintf("config: projects[%q]", it.Path)
	case "history-lines":
		head = "filter: " + it.Path
	default:
		head = it.Kind + ":    " + it.Path
	}
	return head + "\n           " + it.Reason
}

// purgeFailure is one failed item.
type purgeFailure struct {
	Message string
	Code    string
}

// apply carries out one plan item.
func (p *purgeEnv) apply(it purgeItem) *purgeFailure {
	switch it.Kind {
	case "config-key":
		if err := p.removeConfigProject(it.Path); err != nil {
			return &purgeFailure{Message: fmt.Sprintf("Failed to remove projects[%q] from .gocode.json — is your config directory writable?", it.Path), Code: "config_write_failed"}
		}
		return nil
	case "history-lines":
		if err := p.filterHistory(it.Path, it.matchPaths); err != nil {
			return &purgeFailure{Message: "history.jsonl was not rewritten — " + err.Error(), Code: "history_rewrite_refused"}
		}
		return nil
	case "file", "dir":
		if err := removeUnder(p.dataDir, it.Path); err != nil {
			return &purgeFailure{Message: fmt.Sprintf("Could not delete %s — %v", it.Path, err), Code: "delete_failed"}
		}
		return nil
	}
	return &purgeFailure{Message: "unknown item kind " + it.Kind, Code: "delete_failed"}
}

// report prints the outcome and returns the exit code.
func (p *purgeEnv) report(failed []purgeFailure, success string) int {
	if len(failed) == 0 {
		fmt.Fprintln(p.env.Stdout, success)
		return 0
	}
	msgs := make([]string, len(failed))
	deleteFailed := false
	for i, f := range failed {
		msgs[i] = f.Message
		if strings.HasPrefix(f.Code, "delete_failed") {
			deleteFailed = true
		}
	}
	out := fmt.Sprintf("%d item(s) failed:\n  %s", len(failed), strings.Join(msgs, "\n  "))
	if deleteFailed {
		out += "\nWhat could not be deleted is still on disk: fix the cause and run the command again, or delete those paths by hand."
	}
	fmt.Fprintln(p.env.Stderr, out)
	return 1
}

// errOutsideDataDir is the refusal for a path that is not inside the data dir.
var errOutsideDataDir = errors.New("refusing to delete a path outside the gocode data directory")

// removeUnder deletes path (recursively for a directory) only when it lies
// strictly inside root. The parent directory is resolved through symlinks and
// re-checked against the resolved root, so a symlinked component cannot steer
// the delete outside the data directory; a symlink at the final component is
// removed as a link, never followed.
func removeUnder(root, path string) error {
	if !pathStrictlyUnder(root, path) {
		return errOutsideDataDir
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	target := filepath.Join(realParent, filepath.Base(path))
	if !pathStrictlyUnder(realRoot, target) {
		return errOutsideDataDir
	}
	return os.RemoveAll(target)
}

// pathStrictlyUnder reports whether path is inside root (and not root itself).
func pathStrictlyUnder(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// configProjects lists the keys of the "projects" map in the state file
// .
func (p *purgeEnv) configProjects() []string {
	obj := readJSONObject(p.globalConfig)
	projects, _ := obj["projects"].(map[string]any)
	keys := make([]string, 0, len(projects))
	for k := range projects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// removeConfigProject drops one projects[key] entry from the state file,
// rewriting the file atomically with every other key kept.
func (p *purgeEnv) removeConfigProject(key string) error {
	if p.globalConfig == "" {
		return errors.New("no global config path")
	}
	return settingsload.UpdateFile(p.globalConfig, func(obj map[string]any) (bool, error) {
		projects, ok := obj["projects"].(map[string]any)
		if !ok {
			return false, nil
		}
		if _, has := projects[key]; !has {
			return false, nil
		}
		delete(projects, key)
		return true, nil
	})
}

// filterHistory rewrites history.jsonl without the lines whose "project" is
// one of paths (or below it), preserving the file mode.
func (p *purgeEnv) filterHistory(path string, paths []string) error {
	if !pathStrictlyUnder(p.dataDir, path) {
		return errOutsideDataDir
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("history.jsonl is a symlink or not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var kept bytes.Buffer
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		if historyLineMatches(strings.TrimRight(line, "\r\n"), paths) {
			continue
		}
		kept.WriteString(line)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history.jsonl.tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(kept.Bytes())
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmpName, info.Mode().Perm())
	}
	if werr == nil {
		werr = os.Rename(tmpName, path)
	}
	if werr != nil {
		_ = os.Remove(tmpName)
	}
	return werr
}

// countHistoryLines counts the history.jsonl lines for the project.
func countHistoryLines(path string, paths []string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if historyLineMatches(strings.TrimRight(line, "\r"), paths) {
			n++
		}
	}
	return n
}

// historyLineMatches reports a JSON line whose "project" equals one of
// paths or lies below it.
func historyLineMatches(line string, paths []string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	var rec struct {
		Project *string `json:"project"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Project == nil {
		return false
	}
	return pathWithin(*rec.Project, paths)
}

func pathWithin(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// transcriptsRecordedUnder reports whether the first `cwd` found in the first
// 50 lines of the directory's transcripts decides.
func transcriptsRecordedUnder(dir string, paths []string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if cwd, ok := firstTranscriptCwd(filepath.Join(dir, name)); ok {
			return pathWithin(cwd, paths)
		}
	}
	return false
}

func firstTranscriptCwd(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for i := 0; i < 50 && sc.Scan(); i++ {
		var rec struct {
			Cwd *string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Cwd != nil {
			return *rec.Cwd, true
		}
	}
	return "", false
}

// transcriptSessionIDs lists the session ids (uuid-named .jsonl files) in a
// project directory.
func transcriptSessionIDs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok {
			continue
		}
		if _, valid := ids.ValidateUUID(name); valid {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func trimTrailingSlashes(p string) string {
	t := strings.TrimRight(p, "/\\")
	if t == "" {
		return "/"
	}
	return t
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readJSONObject reads a JSON object file; any problem yields an empty map.
func readJSONObject(path string) map[string]any {
	if path == "" {
		return map[string]any{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return map[string]any{}
	}
	return obj
}

// runProjectCmd routes the hidden `project` group: only `project purge`
// exists, a deprecated alias of `purge`.
func runProjectCmd(env *Env, t []string) int {
	args := argsAfter(t, "project")
	if len(args) > 0 && args[0] == "purge" {
		return runPurgeCmd(env, t, true)
	}
	fmt.Fprintf(env.Stderr, "%s\n\nUsage:\n  gocode project purge [options] [path]\n\n  purge [path]  %s. %s\n", purgeDeprecatedAlias, purgeDeprecatedAlias, purgeDescription)
	return 1
}
