package cli

import (
	"context"
	"reflect"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/agenttool"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
)

// This file wires a skill's frontmatter (allowed-tools, disallowed-tools,
// model, effort) to the live session for the skill's scope.
//
// Scopes:
//
//   - INLINE skill: allowed-tools, model and effort become permission layers of
//     the running query and so last for the REST OF THE CURRENT TURN; the
//     skill's disallowed-tools join the session deny rules until the next user
//     message starts a turn. Both end at the same point here: the end of the
//     model turn (conversation.DeferTurnEnd). Subagents spawned during that
//     turn inherit the deny rules but not the allow rules.
//   - FORKED skill: the same lists are scoped to the forked subagent's
//     dispatcher and die with it; model/effort are the subagent's.
//     The agents the fork spawns (and theirs) inherit its allow AND deny rules
//     unchanged — never more (a child's permission state derives from the
//     fork's).
//   - INLINE skill invoked BY A SUBAGENT: as for the main conversation, but on
//     that subagent — its dispatcher, its own model caller — for the rest of
//     its run (subagent.TurnScope).
//
// Temporary rules are pushed as a layer on the dispatcher's ScopedRules —
// consulted by the Decider on top of the shared rule store but never added to
// it — and removed when the scope ends. Nothing here writes settings.

// skillRuleSource tags a skill's temporary rules (the "command" rule source).
const skillRuleSource = "command"

// compileSkillRules turns allowed-/disallowed-tools strings into rules.
func compileSkillRules(allow, deny []string) toolreg.RuleSet {
	var out toolreg.RuleSet
	for _, spec := range allow {
		if r, ok := parsePermissionRule(spec, toolreg.RuleAllow, skillRuleSource); ok {
			out = append(out, r)
		}
	}
	for _, spec := range deny {
		if r, ok := parsePermissionRule(spec, toolreg.RuleDeny, skillRuleSource); ok {
			out = append(out, r)
		}
	}
	return out
}

// liveModelCaller is the retargetable surface of the session model caller
// (provider.ModelCaller).
type liveModelCaller interface {
	CurrentModel() string
	SetModel(model string)
}

// liveEffortCaller is the effort surface of the session model caller.
type liveEffortCaller interface {
	CurrentEffort() string
	SetEffort(level string)
}

// turnSkillScope activates an inline skill on the MAIN conversation for the
// rest of the current turn and restores everything when the turn ends.
type turnSkillScope struct {
	rt       *sessionRuntime
	provider string
	// gate checks a skill's model override (skillmodel.go); its mode is the
	// main dispatcher's.
	gate skillModelGate
}

// ActivateInline implements agenttool.SkillScope.
func (s *turnSkillScope) ActivateInline(_ context.Context, a agenttool.SkillActivation) (string, error) {
	conv, d := s.rt.conv, s.rt.dispatcher
	if rules := compileSkillRules(s.rt.skillGrants(d, a.Skill, a.AllowedTools), a.DisallowedTools); len(rules) > 0 {
		conv.DeferTurnEnd(d.ScopedRules().Push(rules))
	}
	g := s.gate
	g.provider = s.provider
	if g.mode == nil && d != nil {
		g.mode = d.CurrentMode
	}
	return applySkillModel(conv.Model, g, a, conv.DeferTurnEnd), nil
}

// applySkillModel switches caller to the skill's model and effort and registers
// their restoration with deferEnd. It returns the model the rest of the turn
// runs on when a model override was applied ("" otherwise): the skill's model,
// or the current one when the availableModels allowlist kept it (the Skill
// tool reports the session model as `model`). A model
// unsupported in auto mode drops the override entirely. A caller
// without the live model/effort surface is left untouched.
func applySkillModel(caller agent.ModelCaller, g skillModelGate, a agenttool.SkillActivation, deferEnd func(func())) string {
	active := ""
	if a.Model != "" {
		if mc, ok := caller.(liveModelCaller); ok {
			prev := mc.CurrentModel()
			switch next, outcome := g.skillModel(a.Model, prev); outcome {
			case skillModelSwitched:
				mc.SetModel(next)
				deferEnd(func() { mc.SetModel(prev) })
				active = next
			case skillModelKept:
				// Kept by the allowlist: the turn runs on the session model.
				active = prev
			}
		}
	}
	// A named level, or an integer thinking budget run at "high" (the provider
	// wires take only named levels).
	if level := skillEffortLevel(a.Effort); level != "" {
		if ec, ok := caller.(liveEffortCaller); ok {
			prev := ec.CurrentEffort()
			if level != prev {
				ec.SetEffort(level)
				deferEnd(func() { ec.SetEffort(prev) })
			}
		}
	}
	return active
}

// resolveSkillModel maps a skill's `model` to a provider model id with the
// same alias table the Agent tool uses ("sonnet"/"opus"/… → ids; a full id is
// passed through; anything unusable keeps the current model).
func resolveSkillModel(prov, requested, current string) string {
	return resolveSubagentModel(prov, requested, current)
}

// subagentSkillScope activates an inline skill invoked BY A SUBAGENT on that
// subagent for the rest of its current run (its "turn": the spawn, or one
// Resume follow-up): the allowed/disallowed tools are layered on the
// subagent's own dispatcher, and the model/effort switch the subagent's OWN
// model caller (reached through the subagent.TurnScope the running turn puts on
// the tool-call context). Everything is undone when the run ends.
type subagentSkillScope struct {
	d        *toolrun.Dispatcher
	provider string
	// gate checks a skill's model override; its mode is d's (the subagent's).
	gate skillModelGate
	// grant filters the skill's allowed-tools (allowManagedPermissionRulesOnly,
	// toolgrants.go); nil applies them as written.
	grant func(d *toolrun.Dispatcher, skill string, tools []string) []string
	// mainCaller returns the main conversation's caller: a subagent sharing it
	// (a test/fake caller the factory passes through) is never retargeted from
	// here, since that would switch the main session too.
	mainCaller func() agent.ModelCaller
}

// ActivateInline implements agenttool.SkillScope.
func (s *subagentSkillScope) ActivateInline(ctx context.Context, a agenttool.SkillActivation) (string, error) {
	turn := subagent.TurnScopeFrom(ctx)
	allowed := a.AllowedTools
	if s.grant != nil {
		allowed = s.grant(s.d, a.Skill, allowed)
	}
	if rules := compileSkillRules(allowed, a.DisallowedTools); len(rules) > 0 {
		remove := s.d.ScopedRules().Push(rules)
		if turn != nil {
			turn.DeferTurnEnd(remove)
		}
	}
	if turn == nil {
		return "", nil
	}
	mc := unwrapModelCaller(turn.Model())
	if mc == nil || (s.mainCaller != nil && sameCaller(mc, unwrapModelCaller(s.mainCaller()))) {
		return "", nil
	}
	g := s.gate
	g.provider = s.provider
	if g.mode == nil {
		g.mode = s.d.CurrentMode
	}
	return applySkillModel(mc, g, a, turn.DeferTurnEnd), nil
}

// modelCallerUnwrapper is a model-caller wrapper that exposes the caller it
// wraps (agenttool's background launch wrapper).
type modelCallerUnwrapper interface{ Unwrap() agent.ModelCaller }

// unwrapModelCaller strips wrappers down to the transport caller.
func unwrapModelCaller(mc agent.ModelCaller) agent.ModelCaller {
	for i := 0; i < 8 && mc != nil; i++ {
		u, ok := mc.(modelCallerUnwrapper)
		if !ok {
			break
		}
		mc = u.Unwrap()
	}
	return mc
}

// sameCaller reports whether a and b are the same caller (false for
// non-comparable dynamic types, which cannot be shared by identity anyway).
func sameCaller(a, b agent.ModelCaller) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// subagentSkillTool returns the Skill tool a subagent's dispatcher d carries:
// inline skills it invokes are scoped to it (subagentSkillScope), and skills it
// forks spawn under d's inherited rule scopes, so a fork started deep inside a
// forked skill's agent tree still runs under that skill's rules. depth is the
// subagent's own nesting depth: its forks run at depth+1 and are refused once
// it is at the cap, exactly like its Agent spawns.
func (r *sessionRuntime) subagentSkillTool(d *toolrun.Dispatcher, root *agenttool.AgentTool, depth int) *agenttool.SkillTool {
	sc := &subagentSkillScope{
		d:          d,
		grant:      r.skillGrants,
		mainCaller: func() agent.ModelCaller { return r.conv.Model },
	}
	if ts, ok := r.skillTool.Scope.(*turnSkillScope); ok {
		sc.provider = ts.provider
		sc.gate = ts.gate
		sc.gate.mode = nil // the subagent's own mode (d), set at activation
	}
	st := r.skillTool.WithScope(sc)
	if r.skillShell != nil {
		// Blocks of a skill the subagent invokes are gated on its own
		// dispatcher (they run in the invoking agent's context).
		st.ExpandShell = r.skillShell(d)
	}
	// A background fork it starts freezes ITS command-scoped deny rules.
	st.CallerDenyRules = func() []string { return commandDenyRules(d) }
	if st.Agent != nil {
		at := root.ForChild(depth, "", "")
		r.bindSpawnScopes(at, d, root, depth+1)
		st.Agent = at
	}
	return st
}

// commandDenyRules returns the deny rules of d's temporary rule scopes — its
// own layers and every scope it inherited — as rule strings: the "command"
// deny rules (skill / command disallowed-tools; settings and session rules are
// other sources and are not frozen).
func commandDenyRules(d *toolrun.Dispatcher) []string {
	if d == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, sc := range d.ScopesForChild() {
		for _, rule := range sc.Rules.Snapshot(true) {
			if rule.Source != skillRuleSource {
				continue
			}
			spec := rule.ToolName
			if rule.Content != "" {
				spec += "(" + rule.Content + ")"
			}
			if !seen[spec] {
				seen[spec] = true
				out = append(out, spec)
			}
		}
	}
	return out
}

// forkModelAllowed is the availableModels check a forked skill's `model`
// passes (agenttool.AgentTool.ModelAllowed): the alias is resolved like the
// fork's model caller resolves it; an unusable spelling (which runs on the
// session model anyway) passes.
//
// A refused family alias that steps down within its family also passes: the
// subagent model factory resolves it to the newest
// allowed model of the family (modelPolicy.subagentModel).
func forkModelAllowed(prov string, g skillModelGate) func(string) bool {
	return func(m string) bool {
		resolved := resolveSubagentModel(prov, m, "")
		if resolved == "" {
			return true
		}
		if g.modelAllowed(resolved) {
			return true
		}
		if g.policy != nil {
			if p := g.policy(); p != nil && p.familyStepDown(m) != "" {
				return true
			}
		}
		return false
	}
}

// bindSpawnScopes makes the agents at spawns build their dispatchers (at
// depth) under the temporary rule scopes the spawning dispatcher d passes on
// (toolrun.Dispatcher.ScopesForChild, read at spawn time), and drops from their
// tool roster every tool a scope denies outright.
func (r *sessionRuntime) bindSpawnScopes(at *agenttool.AgentTool, d *toolrun.Dispatcher, root *agenttool.AgentTool, depth int) {
	at.NewDispatcher = func(a []string) agent.ToolDispatcher {
		return r.childDispatcherFrom(d.ScopesForChild(), root, depth, a)
	}
	at.ToolSchemas = func(a []string) []agent.ToolSchema {
		return r.childDispatcherFrom(d.ScopesForChild(), root, depth, a).Schemas()
	}
	at.NewScopedDispatcher = func(a []string, sc agenttool.ChildRuleScope) agent.ToolDispatcher {
		return r.scopedChildDispatcherFrom(d.ScopesForChild(), root, depth, a, sc)
	}
	at.NewDispatcherIn = func(a []string, sc agenttool.ChildRuleScope, cwd string) agent.ToolDispatcher {
		return r.scopedChildDispatcherFrom(d.ScopesForChild(), root, depth, a, sc, toolrun.WithWorkingDir(cwd))
	}
	at.ToolNames = withoutDeniedTools(at.ToolNames, d.ScopesForChild())
	// The live MCP tools a spawn's roster takes in go through the same filter.
	if live := at.MCPToolNames; live != nil {
		at.MCPToolNames = func() []string { return withoutDeniedTools(live(), d.ScopesForChild()) }
	}
}

// withoutDeniedTools drops from names every tool a deny rule in scopes names
// without content ("Write", "Bash(*)"): a forked skill's disallowed tools are
// removed from the fork's tool pool, and the agents it spawns draw their tools
// from that pool.
func withoutDeniedTools(names []string, scopes []toolrun.InheritedScope) []string {
	drop := map[string]bool{}
	for _, sc := range scopes {
		for _, rule := range sc.Rules.Snapshot(true) {
			if rule.Content == "" || rule.Content == "*" {
				drop[rule.ToolName] = true
			}
		}
	}
	if len(drop) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !drop[n] {
			out = append(out, n)
		}
	}
	return out
}

// scopedChildDispatcher builds a forked skill's subagent dispatcher (spawned
// from the main conversation) with the skill's temporary allow/deny rules.
func (r *sessionRuntime) scopedChildDispatcher(root *agenttool.AgentTool, allowed []string, sc agenttool.ChildRuleScope) agent.ToolDispatcher {
	return r.scopedChildDispatcherFrom(r.dispatcher.ScopesForChild(), root, 1, allowed, sc)
}

// scopedChildDispatcherFrom builds a forked skill's subagent dispatcher: it
// inherits the spawner's scopes like any child, and the skill's
// allowed-/disallowed-tools form one more scope that the fork reads in full
// AND passes unchanged to every agent it spawns (the rules ride on the fork's
// own permission state, which its children derive theirs from).
// The fork never gains more than the skill grants; deny rules (the skill's and
// every inherited one) keep their precedence.
func (r *sessionRuntime) scopedChildDispatcherFrom(scopes []toolrun.InheritedScope, root *agenttool.AgentTool, depth int, allowed []string, sc agenttool.ChildRuleScope, extra ...toolrun.Option) agent.ToolDispatcher {
	if rules := compileSkillRules(r.skillGrants(r.dispatcher, sc.Origin, sc.AllowRules), sc.DenyRules); len(rules) > 0 {
		fork := toolrun.NewScopedRules()
		fork.Push(rules)
		scopes = append(append([]toolrun.InheritedScope(nil), scopes...), toolrun.InheritedScope{Rules: fork})
	}
	return r.childDispatcherFrom(scopes, root, depth, allowed, extra...)
}

// skillGrants filters a skill's allowed-tools through the session's
// allowManagedPermissionRulesOnly gate (toolgrants.go): d is the dispatcher of
// the agent the skill applies to.
func (r *sessionRuntime) skillGrants(d *toolrun.Dispatcher, skill string, tools []string) []string {
	if len(tools) > 0 && r.print != nil && r.print.grants.projectSkill(skill) {
		return nil // untrusted folder: its skills grant nothing (trust.go)
	}
	if d == nil || !d.ManagedRulesOnly || len(tools) == 0 {
		return tools
	}
	if r.print == nil {
		return nil
	}
	return r.print.grants.grantSkill(true, skill, tools)
}
