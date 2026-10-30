package toolrun

import (
	"sync"

	"github.com/umairge66/gocode/internal/toolreg"
)

// This file holds TEMPORARY permission-rule layers: allow/deny rules that are
// active only for a bounded scope (an inline skill's turn, a forked skill's
// subagent) and are then removed. They are layered on top of the dispatcher's
// live rules (the shared rule store) at decision time WITHOUT ever being added
// to that store, so they can never be persisted to settings, leak into a later
// turn, or be seen by another skill once their scope ends.
//
// # Behavior
//
// A skill's frontmatter acts as "permission layers": allowed-tools are folded
// into the allow rules and disallowed-tools into the deny rules each time the
// permission context is read. The layers live only for the running turn, so
// they vanish when it ends. When a subagent is derived the allowed-tools
// layers are STRIPPED while disallowed-tools are KEPT — a subagent never
// inherits a skill's auto-approvals, but it does inherit its prohibitions. A
// dispatcher reads its own layers in full and may additionally inherit a
// parent's layers as a deny-only view (see WithInheritedScopedRules). A forked
// skill's rules are the exception: they ride on the fork's permission state
// itself, so they are a full inherited scope passed unchanged to every agent
// the fork spawns (see ScopesForChild).
//
// Deny rules keep their usual precedence (the Decider checks deny first), and
// every allow rule goes through the normal toolreg matching, so the Bash
// allow-rule safety (an allow rule never approves a chained / compound command)
// applies to these rules exactly as to settings rules.

// ScopedRules is a concurrency-safe set of temporary rule layers. Each Push
// returns a remove func that drops exactly that layer; the zero value is not
// usable (use NewScopedRules).
type ScopedRules struct {
	mu     sync.RWMutex
	next   uint64
	order  []uint64
	layers map[uint64]toolreg.RuleSet
}

// NewScopedRules returns an empty layer set.
func NewScopedRules() *ScopedRules {
	return &ScopedRules{layers: map[uint64]toolreg.RuleSet{}}
}

// Push adds a layer of temporary rules and returns the func that removes it.
// The rules are copied; calling remove more than once is harmless. An empty
// layer is not recorded (remove is a no-op).
func (s *ScopedRules) Push(rules toolreg.RuleSet) (remove func()) {
	if s == nil || len(rules) == 0 {
		return func() {}
	}
	cp := make(toolreg.RuleSet, len(rules))
	copy(cp, rules)
	s.mu.Lock()
	s.next++
	id := s.next
	s.layers[id] = cp
	s.order = append(s.order, id)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.layers, id)
			for i, v := range s.order {
				if v == id {
					s.order = append(s.order[:i:i], s.order[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
		})
	}
}

// Snapshot returns the active layered rules in push order. With denyOnly, only
// deny rules are returned (the subagent-inherited view).
func (s *ScopedRules) Snapshot(denyOnly bool) toolreg.RuleSet {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out toolreg.RuleSet
	for _, id := range s.order {
		for _, r := range s.layers[id] {
			if denyOnly && r.Behavior != toolreg.RuleDeny {
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

// Len reports the number of active layers.
func (s *ScopedRules) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// InheritedScope is a layer set a dispatcher reads without owning it.
// DenyOnly restricts the view to the set's deny rules.
type InheritedScope struct {
	Rules    *ScopedRules
	DenyOnly bool
}

// WithInheritedScopedRules gives a (subagent) dispatcher a deny-only view of a
// parent's temporary layers: a parent skill's disallowed-tools reach its
// subagents, its allowed-tools do not.
func WithInheritedScopedRules(s *ScopedRules) Option {
	return WithInheritedScopes(InheritedScope{Rules: s, DenyOnly: true})
}

// WithInheritedScopes adds layer sets the dispatcher reads but does not own.
// A full (not DenyOnly) scope is how a forked skill's allowed-/disallowed-tools
// reach the fork's dispatcher AND every agent the fork spawns (see
// ScopesForChild).
func WithInheritedScopes(scopes ...InheritedScope) Option {
	return func(d *Dispatcher) {
		for _, sc := range scopes {
			if sc.Rules != nil {
				d.inherited = append(d.inherited, sc)
			}
		}
	}
}

// ScopesForChild returns the temporary layers a subagent spawned by this
// dispatcher inherits: every scope this dispatcher inherited, with the SAME
// view (a deny-only view is never upgraded), plus this dispatcher's own layers
// as a deny-only view.
//
// Security: a child's temporary allow rules are always a subset of its
// parent's (a full scope is passed on unchanged; own layers pass deny-only),
// and deny rules keep their precedence in the Decider.
func (d *Dispatcher) ScopesForChild() []InheritedScope {
	own := d.ScopedRules() // created now so layers pushed later still reach the child
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	out := make([]InheritedScope, 0, len(d.inherited)+1)
	out = append(out, d.inherited...)
	return append(out, InheritedScope{Rules: own, DenyOnly: true})
}

// ScopedRules returns the dispatcher's own temporary-rule layer set, creating it
// on first use.
func (d *Dispatcher) ScopedRules() *ScopedRules {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.scoped == nil {
		d.scoped = NewScopedRules()
	}
	return d.scoped
}

// PersistentRules returns the dispatcher's live rule set WITHOUT any temporary
// layers: what a newly built dispatcher should be seeded with, and the only
// rules that may ever be written back anywhere.
func (d *Dispatcher) PersistentRules() toolreg.RuleSet {
	if d.shared != nil {
		return d.shared.Snapshot()
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	if len(d.Rules) == 0 {
		return nil
	}
	out := make(toolreg.RuleSet, len(d.Rules))
	copy(out, d.Rules)
	return out
}

// scopedSnapshot returns the temporary rules this dispatcher currently layers on
// top of its live rules: its own layers in full, then each inherited scope in
// its view (deny-only or full).
func (d *Dispatcher) scopedSnapshot() toolreg.RuleSet {
	d.stateMu.RLock()
	own, inh := d.scoped, d.inherited
	d.stateMu.RUnlock()
	out := own.Snapshot(false)
	for _, sc := range inh {
		if sc.Rules == own {
			continue
		}
		out = append(out, sc.Rules.Snapshot(sc.DenyOnly)...)
	}
	return out
}
