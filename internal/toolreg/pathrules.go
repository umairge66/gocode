package toolreg

import (
	"os"
	"strings"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// File-path rule matching for the file tools.
//
// A file tool's rule content is never compared with the requested
// path as strings. Each file tool's permission check groups the rules of one
// "kind" — "read" (the Read tool's rules) for Read, Glob and Grep, "edit" (the
// Edit tool's rules) for Edit, Write, MultiEdit and NotebookEdit — compiles
// their contents into gitignore patterns under a root ("./", "~/", "//", "/"),
// and matches every spelling of the requested path (the cleaned absolute path
// and its symlink-resolved landing): a deny or ask rule hitting ANY spelling
// applies; an allow rule must cover EVERY spelling.
//
// Besides that per-kind grouping, a whole-filesystem deny / ask rule
// ("Read(//**)", "Edit(//**)") also reaches the sibling tools listed in the
// propagation map: a Read match-all deny covers Read, Grep, Glob,
// Edit and Write; a Read match-all ask covers Read, Grep and Glob; an Edit
// match-all deny / ask covers Edit, Write and NotebookEdit.

// pathRuleKind returns the canonical tool whose rules govern a file tool's
// path ("Read" for the read tools, "Edit" for the edit tools), or "" for a
// tool that is not a file-path tool.
func pathRuleKind(t protocol.Tool) protocol.Tool {
	switch t {
	case protocol.ToolRead, protocol.ToolGlob, protocol.ToolGrep, protocol.ToolLSP:
		return protocol.ToolRead
	case protocol.ToolEdit, protocol.ToolWrite, protocol.ToolMultiEdit, protocol.ToolNotebookEdit:
		return protocol.ToolEdit
	}
	return ""
}

// matchAllReach is the propagation map: the tools a
// whole-filesystem deny / ask rule of a given tool also reaches.
var matchAllReach = map[protocol.Tool]map[RuleBehavior][]protocol.Tool{
	protocol.ToolRead: {
		RuleDeny: {protocol.ToolRead, protocol.ToolGrep, protocol.ToolGlob, protocol.ToolLSP, protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolWrite},
		RuleAsk:  {protocol.ToolRead, protocol.ToolGrep, protocol.ToolGlob, protocol.ToolLSP},
	},
	protocol.ToolEdit: {
		RuleDeny: {protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolWrite, protocol.ToolNotebookEdit},
		RuleAsk:  {protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolWrite, protocol.ToolNotebookEdit},
	},
}

// spellingCache computes a call's path spellings once per decision.
type spellingCache struct {
	done      bool
	spellings []string
}

// roots returns the path-rule roots: PathRules, else the session's (Files),
// else the process working directory and home.
func (d *Decider) roots() pathguard.PathRuleRoots {
	if d.PathRules != nil {
		return *d.PathRules
	}
	if d.Files != nil {
		return d.Files.RuleRoots()
	}
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return pathguard.PathRuleRoots{Cwd: wd, Home: home}
}

// pathSpellings returns the spellings a path rule is judged on: the session
// walk's (requested, link targets, landing) when Files is set, else the
// cleaned absolute path and its symlink-resolved spelling.
func (d *Decider) pathSpellings(p string) []string {
	r := d.roots()
	abs := pathguard.AbsRequestedPath(p, r.Cwd, r.Home)
	if abs == "" {
		return nil
	}
	if d.Files != nil {
		sp := d.Files.ResolvePath(abs).Spellings
		if len(sp) > 0 {
			if sp[0] != abs {
				sp = append([]string{abs}, sp...)
			}
			return sp
		}
	}
	return pathguard.PathSpellingsFor(abs, r.Cwd, r.Home)
}

// MatchRule returns the first rule of behavior b that applies to call, like
// RuleSet.Match, except that for a file tool carrying a path a
// content-bearing rule is matched as a file-path rule (see the file comment)
// instead of as a string glob.
func (d *Decider) MatchRule(rules RuleSet, call Call, b RuleBehavior) *Rule {
	return d.matchRule(rules, call, &spellingCache{}, b)
}

func (d *Decider) matchRule(rules RuleSet, call Call, sp *spellingCache, b RuleBehavior) *Rule {
	kind := pathRuleKind(call.Tool)
	if kind == "" || call.Path == "" {
		return rules.Match(call, b)
	}
	name := string(call.Tool)
	full := call.Rule
	if full == "" {
		full = name
	}
	roots := d.roots()
	for i := range rules {
		r := rules[i]
		if r.Behavior != b {
			continue
		}
		if r.Content == "" || r.Content == "*" {
			if r.toolNameMatches(name, full) {
				return &r
			}
			continue
		}
		if !pathRuleApplies(r, call.Tool, kind) {
			continue
		}
		if !sp.done {
			sp.spellings, sp.done = d.pathSpellings(call.Path), true
		}
		if pathRuleCovers(roots, r, sp.spellings) {
			return &r
		}
	}
	return nil
}

// pathRuleApplies reports whether content-bearing rule r governs the path of
// a call of tool t (whose kind is kind): a rule of the kind's canonical tool
// (Read / Edit), a rule of the tool's own name, or a whole-filesystem deny /
// ask rule whose propagation reaches t.
func pathRuleApplies(r Rule, t, kind protocol.Tool) bool {
	if r.ToolName == string(kind) || r.ToolName == string(t) {
		return true
	}
	if r.Behavior == RuleAllow || !pathguard.IsMatchAllPathRule(r.Content) {
		return false
	}
	for _, reach := range matchAllReach[protocol.Tool(r.ToolName)][r.Behavior] {
		if reach == t {
			return true
		}
	}
	return false
}

// pathRuleCovers applies one rule to the spellings: a deny / ask rule
// matching any spelling applies; an allow rule must match every spelling.
func pathRuleCovers(roots pathguard.PathRuleRoots, r Rule, spellings []string) bool {
	if len(spellings) == 0 {
		return false
	}
	allow := r.Behavior == RuleAllow
	for _, s := range spellings {
		m := roots.PathRuleMatches(r.Content, r.Source, s, allow)
		if allow && !m {
			return false
		}
		if !allow && m {
			return true
		}
	}
	return allow
}

// ReadDenied reports whether a Read deny rule covers path p (any spelling).
// Glob and Grep use it to drop results a Read deny rule covers, so a search
// never reveals a denied file (search exclusions plus a per-result check).
func (d *Decider) ReadDenied(rules RuleSet, p string) bool {
	has := false
	for _, r := range rules {
		if r.Behavior == RuleDeny && r.Content != "" && r.Content != "*" && r.ToolName == string(protocol.ToolRead) {
			has = true
			break
		}
	}
	if !has {
		return false
	}
	roots := d.roots()
	spellings := pathguard.PathSpellingsFor(p, roots.Cwd, roots.Home)
	for _, r := range rules {
		if r.Behavior != RuleDeny || r.Content == "" || r.Content == "*" || r.ToolName != string(protocol.ToolRead) {
			continue
		}
		if pathRuleCovers(roots, r, spellings) {
			return true
		}
	}
	return false
}

// ReadOutsideBlocked reports whether permissions.blockReadsOutsideWorkingDirectories
// refuses a read of p (used to filter Glob / Grep results that resolve, through
// a link, outside the working directories).
func (d *Decider) ReadOutsideBlocked(p string) bool {
	if d.Files == nil || !d.Files.BlockReadsOutsideWorkingDirectories {
		return false
	}
	roots := d.roots()
	sp := pathguard.PathSpellingsFor(p, roots.Cwd, roots.Home)
	return d.Files.ReadsBlockedOutside(pathguard.PathResolution{Requested: strings.TrimSpace(p), Spellings: sp}) != nil
}
