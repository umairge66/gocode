package agenttool

import (
	"context"
	"errors"
	"fmt"
	"github.com/umairge66/gocode/internal/agent"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/umairge66/gocode/internal/envtrust"
	"github.com/umairge66/gocode/internal/git"
)

// Agent tool spawn options: worktree
// isolation, the spawn-time cwd, addressable names, the agent.spawn hooks and
// integer effort budgets.

// isolationWorktree is the only isolation mode offered. Hosted ("remote")
// isolation is not supported.
const isolationWorktree = "worktree"

// descIsolation describes the isolation parameter (worktree mode only).
const descIsolation = `Isolation mode. "worktree" creates a temporary git worktree so the agent works on an isolated copy of the repo.`

// descName describes the Agent tool's name parameter, including the
// SendMessage sentence.
const descName = "Name for the spawned agent. Makes it addressable via SendMessage({to: name}) while running."

// worktreeUsageNote is the tool-prompt usage line for worktree isolation.
const worktreeUsageNote = "- With `isolation: \"worktree\"`, the worktree is automatically cleaned up if the agent makes no changes; otherwise the path and branch are returned in the result."

// --- name -------------------------------------------------------------------

// agentNameRe is the allowed agent-name shape.
var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// agentIDShapeRe is the agent-id shape.
var agentIDShapeRe = regexp.MustCompile(`^a(?:[\w-]{1,63}-)?[0-9a-f]{16}$`)

// reservedAgentNames are the addresses SendMessage already routes.
var reservedAgentNames = map[string]bool{"main": true, "team-lead": true, "user": true, "system": true}

// normalizeAgentName applies NFKC, drops control/format
// characters (whitespace kept), trim, lower-case, whitespace runs -> "-".
func normalizeAgentName(s string) string {
	s = norm.NFKC.String(s)
	s = strings.Map(func(r rune) rune {
		if (unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cs, r)) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), "-")
}

// validateAgentName applies the name schema's refinements.
func validateAgentName(name string) error {
	if !agentNameRe.MatchString(name) {
		return errors.New("name must start with a letter or digit and contain only letters, digits, underscores, or hyphens (max 64 chars)")
	}
	if name == "main" {
		return errors.New(`"main" is reserved — SendMessage routes it to the main conversation`)
	}
	if n := normalizeAgentName(name); reservedAgentNames[n] || agentIDShapeRe.MatchString(n) {
		return errors.New(`name must not be a reserved name ("main", "team-lead", "user" or "system", in any spelling) or have the shape of an agent id — those already address an agent directly`)
	}
	return nil
}

// --- cwd --------------------------------------------------------------------

// validateSpawnCwd checks the call's cwd: it must be a non-empty local path
// with no ".." segment whose every part can be examined, and an absolute path
// naming a directory ("Absolute path to run the agent in").
func validateSpawnCwd(cwd string) error {
	fail := func(why string) error {
		return fmt.Errorf("Cannot run the agent in another directory: %s. Start it without cwd.", why)
	}
	if strings.TrimSpace(cwd) == "" {
		return fail("cwd must be a string holding a path")
	}
	for _, seg := range strings.FieldsFunc(cwd, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fail(`cwd must be a local path with no ".." in it, whose every part can be examined`)
		}
	}
	if !filepath.IsAbs(cwd) {
		return fail("cwd must be an absolute path")
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return fail(`cwd must be a local path with no ".." in it, whose every part can be examined`)
	}
	return nil
}

// --- effort -----------------------------------------------------------------

// resolveEffortValue maps an agent definition's effort to a level the
// providers accept: a named level passes through; an integer budget runs at
// "high", the level used for an integer effort on a model that takes only
// named levels.
func resolveEffortValue(raw string) string {
	e := strings.ToLower(strings.TrimSpace(raw))
	if contains(effortChoices, e) {
		return e
	}
	if _, err := strconv.Atoi(e); err == nil && e != "" {
		return "high"
	}
	return ""
}

// --- agent.spawn hooks --------------------------------------------------------

// SpawnHookInput is what an agent.spawn hook sees about a pending spawn.
type SpawnHookInput struct {
	Prompt       string
	Description  string
	SubagentType string
	Model        string
	Background   bool
	Name         string
	Cwd          string
	Isolation    string
	ParentID     string
	Depth        int
}

// SpawnHookDecision is a hook's answer. A non-empty Deny refuses the spawn.
// Rewrite, when set, changes the spawn's prompt, description or model (the
// fields an agent.spawn hook may rewrite whose effect is applied; the
// hooks-module engine checks a rewrite against the site's pinned keys and
// refuses the rest). Deny wins over any rewrite.
type SpawnHookDecision struct {
	Deny    string
	Rewrite *SpawnRewrite
}

// SpawnRewrite is an agent.spawn hook's rewrite of a spawn. A nil field keeps
// the spawn's own value.
type SpawnRewrite struct {
	Prompt      *string
	Description *string
	Model       *string
}

// SpawnHook is one agent.spawn check, run before the subagent (and any
// worktree) is created.
type SpawnHook func(ctx context.Context, in SpawnHookInput) SpawnHookDecision

// runSpawnHooks runs the hooks in order; the first denial wins.
//
// Each hook sees the spawn as the hooks before it rewrote it; the returned
// input is the spawn to run.
func runSpawnHooks(ctx context.Context, hooks []SpawnHook, in SpawnHookInput) (SpawnHookInput, error) {
	for _, h := range hooks {
		if h == nil {
			continue
		}
		d := h(ctx, in)
		if d.Deny != "" {
			return in, fmt.Errorf("Subagent spawn denied by a plugin: %s", d.Deny)
		}
		if rw := d.Rewrite; rw != nil {
			if rw.Prompt != nil && strings.TrimSpace(*rw.Prompt) != "" {
				in.Prompt = *rw.Prompt
			}
			if rw.Description != nil {
				in.Description = *rw.Description
			}
			if rw.Model != nil {
				in.Model = *rw.Model
			}
		}
	}
	return in, nil
}

// rewrittenCaller swaps a spawn's model caller for fresh, built on a hook's
// rewritten model, keeping the spawn's effort and a background launch's
// start signal.
func rewrittenCaller(old, fresh agent.ModelCaller, effort string) agent.ModelCaller {
	if fresh == nil {
		return old
	}
	applyEffort(fresh, effort)
	if s, ok := old.(*signalingCaller); ok {
		return &signalingCaller{inner: fresh, onCall: s.onCall}
	}
	return fresh
}

// --- worktrees ----------------------------------------------------------------

// WorktreeManager creates and finishes a subagent's isolation worktree.
type WorktreeManager interface {
	// Create makes the worktree for a new agent.
	Create(ctx context.Context) (*git.AgentWorktree, error)
	// Finish removes the worktree when the agent left it unchanged and
	// returns "", ""; otherwise it keeps it (unlocked) and returns its path
	// and branch for the result.
	Finish(ctx context.Context, wt *git.AgentWorktree) (path, branch string)
}

// GitWorktrees is the git-backed WorktreeManager.
type GitWorktrees struct {
	Runner *git.Runner
	// Cwd returns the directory whose repository the worktree is made from.
	Cwd func() string
}

// NewGitWorktrees returns a GitWorktrees over the process environment and cwd.
func NewGitWorktrees() *GitWorktrees {
	env := envtrust.Env{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return &GitWorktrees{Runner: git.NewRunner("", env)}
}

func (g *GitWorktrees) cwd() string {
	if g.Cwd != nil {
		if c := g.Cwd(); c != "" {
			return c
		}
	}
	c, _ := os.Getwd()
	return c
}

// Create implements WorktreeManager.
func (g *GitWorktrees) Create(ctx context.Context) (*git.AgentWorktree, error) {
	id, err := git.NewAgentID()
	if err != nil {
		return nil, err
	}
	return g.Runner.CreateAgentWorktree(ctx, g.cwd(), git.AgentWorktreeName(id))
}

// Finish implements WorktreeManager.
func (g *GitWorktrees) Finish(ctx context.Context, wt *git.AgentWorktree) (string, string) {
	if wt == nil {
		return "", ""
	}
	ctx = context.WithoutCancel(ctx)
	if wt.HeadCommit != "" && !g.Runner.AgentWorktreeChanged(ctx, wt.Path, wt.HeadCommit) {
		if ok, _ := g.Runner.RemoveAgentWorktree(ctx, *wt); ok {
			return "", ""
		}
	}
	if wt.GitRoot != "" {
		g.Runner.UnlockWorktree(ctx, wt.Path, wt.GitRoot)
	}
	return wt.Path, wt.Branch
}

// worktreePromptNote tells a non-fork subagent where it works. The subagent
// prompt has no environment section, so the fork note's wording is reused for
// that one fact.
func worktreePromptNote(path string) string {
	return "You are operating in an isolated git worktree at " + path + " — same repository, same relative file structure, separate working copy. Work only inside it: use paths under " + path + ". Your changes stay in this worktree and will not affect the parent's files."
}

// cwdPromptNote names a spawn-time cwd for the subagent.
func cwdPromptNote(cwd string) string {
	return "Your working directory is " + cwd + "."
}
