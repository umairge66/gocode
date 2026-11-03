package projectmemory

import "sync"

// Nested produces the memory that becomes relevant once the agent touches a
// file: the conditional rules whose `paths:` globs match it, and the
// gocode.md / gocode.local.md / unconditional rules of the directories between
// the working directory and the file. Each source is handed out at most once
// per session; the sources already in the startup block count as handed out.
//
// It is safe for concurrent use (tools run in parallel).
type Nested struct {
	cfg Config
	// startup is the startup memory the tracker was seeded with.
	startup []string

	mu     sync.Mutex
	loaded map[string]bool
}

// NewNested returns a tracker over cfg. loaded lists the sources already in
// context (normally Result.Sources of the startup Load).
func NewNested(cfg Config, loaded []string) *Nested {
	n := &Nested{cfg: cfg, startup: append([]string(nil), loaded...), loaded: map[string]bool{}}
	for _, p := range loaded {
		n.loaded[normalizePath(p)] = true
	}
	return n
}

// ForAgent returns a fresh tracker for one subagent: the same memory
// configuration, rooted at cwd ("" keeps the session's), seeded only with the
// startup memory the subagent itself carries (withStartup false for an agent
// whose definition omits the project memory). Rules the parent has already
// been shown are NOT counted as loaded: each agent gets the path-scoped rules
// for the files it touches, once per agent.
func (n *Nested) ForAgent(cwd string, withStartup bool) *Nested {
	if n == nil {
		return nil
	}
	cfg := n.cfg
	if cwd != "" {
		cfg.Cwd = cwd
	}
	var seed []string
	if withStartup {
		seed = n.startup
	}
	return NewNested(cfg, seed)
}

// ForFile returns the not-yet-loaded memory sources relevant to target (an
// absolute path), in injection order, and marks them loaded. The touched file
// itself is never returned.
func (n *Nested) ForFile(target string) []Entry {
	if n == nil {
		return nil
	}
	target = normalizePath(target)
	if target == "" {
		return nil
	}
	cwd := normalizePath(n.cfg.Cwd)
	l := newLoader(n.cfg)

	var found []Entry
	// Conditional Managed and User rules, relative to cwd.
	if n.cfg.ManagedDir != "" {
		found = append(found, l.conditionalRules(managedRulesDir(n.cfg.ManagedDir), KindManaged, target, cwd)...)
	}
	if n.cfg.ConfigDir != "" && !n.cfg.SkipUser {
		found = append(found, l.conditionalRules(userRulesDir(n.cfg.ConfigDir), KindUser, target, cwd)...)
	}
	// Memory files and rules for each directory strictly below cwd on the way
	// to the file.
	for _, dir := range nestedDirs(target, cwd) {
		if n.cfg.SkipProject {
			if !n.cfg.SkipLocal {
				found = append(found, l.collect(join(dir, localFileName), KindLocal, "", 0, false)...)
			}
			continue
		}
		found = append(found, l.collect(join(dir, projectFileName), KindProject, "", 0, false)...)
		found = append(found, l.collect(join(dir, nestedFileName), KindProject, "", 0, false)...)
		if !n.cfg.SkipLocal {
			found = append(found, l.collect(join(dir, localFileName), KindLocal, "", 0, false)...)
		}
		// The unconditional pass runs on a copy of the processed set so it
		// does not hide the conditional rules from the pass after it.
		rules := projectRulesDir(dir)
		saved := l.processed
		l.processed = copySet(saved)
		found = append(found, l.loadRulesDir(rules, KindProject, false)...)
		uncond := l.processed
		l.processed = saved
		found = append(found, l.conditionalRules(rules, KindProject, target, dir)...)
		for p := range uncond {
			l.processed[p] = true
		}
	}
	// Conditional project rules of cwd and its ancestors, each matched
	// relative to the directory that owns the rules.
	for _, dir := range cwdLevelDirs(cwd) {
		if n.cfg.SkipProject {
			break
		}
		found = append(found, l.conditionalRules(projectRulesDir(dir), KindProject, target, dir)...)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Entry
	for _, e := range found {
		if e.Path == target || n.loaded[e.Path] {
			continue
		}
		n.loaded[e.Path] = true
		out = append(out, e)
	}
	return out
}

// conditionalRules returns the conditional rules under rulesDir whose globs
// match target relative to base.
func (l *loader) conditionalRules(rulesDir string, kind Kind, target, base string) []Entry {
	rel, ok := relUnder(base, target)
	if !ok {
		return nil
	}
	var out []Entry
	for _, e := range l.loadRulesDir(rulesDir, kind, true) {
		if len(e.Globs) > 0 && globsMatch(e.Globs, rel) {
			out = append(out, e)
		}
	}
	return out
}

// RenderNested renders one nested source as the model sees it.
func RenderNested(e Entry) string { return nestedFrame(e) }

// nestedDirs lists the directories strictly below cwd that contain target,
// outermost first. A target outside cwd yields none.
func nestedDirs(target, cwd string) []string {
	if cwd == "" {
		return nil
	}
	var dirs []string
	for q := dirOf(target); q != cwd; {
		if within(q, cwd) {
			dirs = append([]string{q}, dirs...)
		}
		parent := dirOf(q)
		if parent == q {
			break
		}
		q = parent
	}
	return dirs
}

// cwdLevelDirs lists cwd and its ancestors below the filesystem root,
// outermost first.
func cwdLevelDirs(cwd string) []string {
	if cwd == "" {
		return nil
	}
	var dirs []string
	for w := cwd; ; {
		parent := dirOf(w)
		if parent == w {
			break
		}
		dirs = append([]string{w}, dirs...)
		w = parent
	}
	return dirs
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
