package toolreg

import (
	"strings"

	"github.com/umairge66/gocode/internal/hooks"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// Mode is the agent's current permission mode. The enum is
// ["default","acceptEdits","plan","auto","bypassPermissions","dontAsk"] (plus
// the transient UI alias "bubble"), ordered by privilege.
type Mode string

const (
	// ModeDefault is standard behavior: unmatched calls escalate to ask.
	ModeDefault Mode = "default"
	// ModeAcceptEdits auto-allows file-editing tools; other calls still ask.
	ModeAcceptEdits Mode = "acceptEdits"
	// ModePlan is analysis-only: non-read-only tools are held for approval
	// (the plan write floor; denied when no prompter is attached).
	ModePlan Mode = "plan"
	// ModeAuto runs an unmatched call when the auto mode safety classifier
	// allows it (see modeDefault and internal/automode); safe read-only tools
	// and edits inside the working directories skip the classifier.
	ModeAuto Mode = "auto"
	// ModeBypassPermissions auto-allows everything that is not explicitly
	// denied by a deny rule.
	ModeBypassPermissions Mode = "bypassPermissions"
	// ModeDontAsk denies any call that would otherwise require asking.
	ModeDontAsk Mode = "dontAsk"
)

// modePrivilege is the privilege ordering (higher = more permissive). It is
// used by AtLeast / rule-cap logic.
var modePrivilege = map[Mode]int{
	ModePlan:              0,
	ModeDefault:           1,
	ModeDontAsk:           1,
	ModeAcceptEdits:       2,
	ModeAuto:              3,
	ModeBypassPermissions: 4,
}

// Privilege returns the mode's privilege level (0 for an unknown mode, so
// unknowns fold to default-ish).
func (m Mode) Privilege() int { return modePrivilege[m] }

// Behavior is the decision outcome: allow, ask or deny. It is the
// permission `behavior` discriminator, kept distinct from hooks.Behavior (which
// additionally has "none") so a Decision always carries a concrete verdict.
type Behavior string

const (
	BehaviorAllow Behavior = "allow"
	BehaviorAsk   Behavior = "ask"
	BehaviorDeny  Behavior = "deny"
)

// Reason categories carried on a Decision (decisionReason.type).
const (
	ReasonRule        = "rule"        // matched a settings allow/ask/deny rule
	ReasonMode        = "mode"        // the permission mode decided
	ReasonSafetyCheck = "safetyCheck" // a path-guard safety refusal
	ReasonHook        = "hook"        // a PreToolUse hook decided
	ReasonOther       = "other"       // default / classifier fallback
	// ReasonWorkingDir: a read outside the working directories asks
	// (decisionReason {type:"workingDir"}).
	ReasonWorkingDir = "workingDir"
	// ReasonUserInteraction: the tool's call is itself a question to the user
	// (RequiresUserInteraction); the tool's own dialog answers the ask.
	ReasonUserInteraction = "requiresUserInteraction"
)

// Decision is the computed permission outcome for a tool call.
type Decision struct {
	Behavior Behavior
	// ReasonType is the decisionReason category (one of the Reason* constants).
	ReasonType string
	// Reason is the human-readable message shown to the user / model.
	Reason string
	// Rule is the matched rule, when ReasonType == ReasonRule.
	Rule *Rule
	// Mode is the deciding mode, when ReasonType == ReasonMode.
	Mode Mode
	// UpdatedInput, when non-nil, replaces the tool input (a PreToolUse hook's
	// updatedInput carried through).
	UpdatedInput []byte
	// Classify marks an auto mode "ask" that the auto mode safety classifier
	// decides (the "classify" mode default and the auto-mode ask branch).
	// The dispatcher runs the classifier; without one the
	// call is asked (interactive) or denied, never silently allowed.
	Classify bool
}

// RuleBehavior is the behavior a settings rule grants.
type RuleBehavior string

const (
	RuleAllow RuleBehavior = "allow"
	RuleDeny  RuleBehavior = "deny"
	RuleAsk   RuleBehavior = "ask"
)

// Rule is one compiled settings permission rule: a behavior, the tool-name it
// is keyed under, and an optional content pattern (the X and Y of "X(Y)").
type Rule struct {
	Behavior RuleBehavior
	ToolName string
	// Content is the rule body — the Y of "X(Y)". "" (no parentheses) and "*"
	// (and the empty rawContent "X()") mean "the whole tool": any call of
	// ToolName matches regardless of its RuleContent (rawContent "" or "*"
	// parses to {toolName} with no ruleContent).
	//
	// Otherwise Content is a pattern matched against the call's RuleContent:
	//   - a "domain:<host>" pattern (WebFetch) uses the domain matcher
	//     (DomainRuleMatches: case-insensitive, "*.base" strict-subdomain, inline
	//     "*" globs per label);
	//   - any other pattern uses the "*" glob matcher (globMatch: a multi-"*"
	//     wildcard where "*" spans any run of characters).
	//
	// NOTE on content layering: the *primary* content match of a path tool
	// (gitignore-style globs on filePath) and of Bash (the command
	// split/prefix engine, ruleContentField:"command") belongs to each tool's
	// own permission check, NOT the generic settings-rule matcher. The generic
	// glob lives here and path tools are routed through the pathguard
	// RuleEngine (see Decider.PathGuard). A Bash command rule may be a bare glob
	// (e.g. "git status*") or the Bash tool's own colon prefix form
	// "git status:*", which is matched by bashPrefixRuleMatches (word-boundary
	// prefix, conservative about compound commands). The cross-tool "match-all"
	// deny/ask propagation (a Read(//**) deny also covering Grep/Glob/Edit/Write,
	// an Edit(//**) deny also covering Write/NotebookEdit) is a
	// file-tool/pathguard content-layer concern and is NOT modeled here.
	Content string
	// Source records where the rule came from (settings/cliArg/session/...);
	// unused by matching but preserved for diagnostics.
	Source string
}

// matches reports whether this rule applies to a call.
//
// The match is two-stage: first the rule's tool-name must select the call's
// tool (toolNameMatches), then
// — for a content-bearing rule — the rule's Content pattern must match the
// call's RuleContent.
func (r Rule) matches(call Call) bool {
	name := string(call.Tool)
	full := call.Rule
	if full == "" {
		full = name
	}
	if !r.toolNameMatches(name, full) {
		return false
	}
	switch r.Content {
	case "", "*":
		return true
	}
	// "domain:<host>" content (WebFetch rules such as WebFetch(domain:example.com)
	// or WebFetch(domain:*.example.com)) uses the domain matcher.
	if strings.HasPrefix(r.Content, domainRulePrefix) && strings.HasPrefix(call.RuleContent, domainRulePrefix) {
		return DomainRuleMatches(r.Content, call.RuleContent)
	}
	// The Bash tool's legacy prefix grammar "Bash(<prefix>:*)" (e.g.
	// "Bash(git status:*)", the form used in settings and in a slash command's
	// allowed-tools frontmatter).
	if call.Tool == protocol.ToolBash {
		// "<prefix>:*" and "<prefix> *" are both command prefixes.
		for _, suf := range []string{bashPrefixRuleSuffix, " *"} {
			if strings.HasSuffix(r.Content, suf) {
				return bashPrefixRuleMatches(strings.TrimSuffix(r.Content, suf), call.RuleContent, r.Behavior)
			}
		}
		if strings.Contains(r.Content, "*") {
			return bashGlobRuleMatches(r.Content, call.RuleContent, r.Behavior)
		}
		// An exact deny / ask rule also sees the normalized spellings of the
		// command.
		if r.Behavior != RuleAllow {
			for _, c := range bashRuleCandidates(call.RuleContent) {
				if c == strings.TrimSpace(r.Content) {
					return true
				}
			}
			return false
		}
	}
	// Any other pattern: the "*" glob (multi-"*" wildcard).
	return globMatch(r.Content, call.RuleContent)
}

// bashGlobRuleMatches applies a wildcard Bash rule (e.g. "ls*", "npm run *test")
// with the same compound-command safety as bashPrefixRuleMatches: a "*" must
// never let an ALLOW rule approve chained / substituted / redirected commands,
// so an allow glob only covers a simple command (one with no shell operators,
// unless the rule itself spells those operators out). A DENY/ASK glob matches if
// the whole command or ANY operator-separated segment matches, so it still
// catches `ls && rm -rf x`. Without this, a saved "Bash(ls*)" approved
// `ls; rm -rf /`.
func bashGlobRuleMatches(pattern, cmd string, behavior RuleBehavior) bool {
	if behavior == RuleAllow {
		if strings.ContainsAny(cmd, bashShellOperators) && !strings.ContainsAny(pattern, bashShellOperators) {
			return false
		}
		return globMatch(pattern, strings.TrimSpace(cmd))
	}
	// Deny / ask: the whole command, every segment and every normalized
	// spelling (bashRuleCandidates), also as "xargs <pattern>".
	for _, c := range bashRuleCandidates(cmd) {
		if globMatch(pattern, c) || globMatch("xargs "+pattern, c) {
			return true
		}
	}
	return false
}

// bashPrefixRuleSuffix marks the Bash tool's legacy prefix rule content.
const bashPrefixRuleSuffix = ":*"

// bashShellOperators are the characters that let one Bash command string chain,
// substitute or redirect into further commands.
const bashShellOperators = "|&;<>$`()\n\r"

// bashPrefixRuleMatches reports whether a "<prefix>:*" Bash rule covers the
// command cmd. A prefix matches the command itself or the command followed by
// arguments (word boundary: `git status` and `git status --short`, but not
// `git statusx`).
//
// A full Bash rule check splits a compound command into its subcommands and
// checks each one. That split engine is approximated conservatively, by the
// rule's behavior:
//
//   - an ALLOW rule covers only a simple command (no shell operators anywhere),
//     so `Bash(git status:*)` never allows `git status; rm -rf x`;
//   - a DENY/ASK rule covers a command when ANY operator-separated segment
//     starts with the prefix, so `Bash(rm:*)` still catches `ls && rm -rf x`.
func bashPrefixRuleMatches(prefix, cmd string, behavior RuleBehavior) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false
	}
	covers := func(seg string) bool {
		seg = strings.TrimSpace(seg)
		return seg == prefix || strings.HasPrefix(seg, prefix+" ") || strings.HasPrefix(seg, prefix+"\t")
	}
	if behavior == RuleAllow {
		if strings.ContainsAny(cmd, bashShellOperators) {
			return false
		}
		return covers(cmd)
	}
	// Deny / ask: the rule sees the real command — every operator-separated
	// segment and every normalized spelling of it (leading env assignments,
	// wrapper commands, path-qualified binaries, quoting and sh -c scripts
	// stripped; see bashRuleCandidates) — compared with runs of blanks folded,
	// and also as "xargs <prefix>".
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	np := norm(prefix)
	for _, c := range bashRuleCandidates(cmd) {
		c = norm(c)
		if c == np || strings.HasPrefix(c, np+" ") || c == "xargs "+np || strings.HasPrefix(c, "xargs "+np+" ") {
			return true
		}
	}
	return false
}

// toolNameMatches reports whether this rule's ToolName selects a tool whose
// plain name is `name` and whose full rule name is `full`. A name matches by:
//
//   - exact string equality against the plain name or the full rule name
//     — this is how a fully-qualified MCP rule "mcp__srv__tool" matches;
//   - a glob tool name (the rule's ToolName contains "*") matched against the
//     full rule name;
//   - MCP coverage (mcpRuleCovers): a server-level or tool-glob MCP rule.
//
// Tool-name and MCP-name matching is CASE-SENSITIVE: the name normalizer only
// maps non-[A-Za-z0-9_-] characters to "_" and preserves case;
// normalization happens when names are *built*, so by
// the time a name reaches this matcher both sides are already literal.
func (r Rule) toolNameMatches(name, full string) bool {
	if r.ToolName == name || r.ToolName == full {
		return true
	}
	if strings.Contains(r.ToolName, "*") && globMatch(r.ToolName, full) {
		return true
	}
	return mcpRuleCovers(r.ToolName, full)
}

// domainRulePrefix prefixes the WebFetch rule content: `domain:<hostname>`.
const domainRulePrefix = "domain:"

// mcpRulePrefix is the model-facing MCP tool name prefix ("mcp__").
const mcpRulePrefix = "mcp__"

// parseMCPName splits an "mcp__<server>__<tool>" name into its parts: the
// string must start "mcp__" and carry a non-empty server segment. hasTool
// distinguishes a bare server rule ("mcp__srv", no tool part) from one with a
// (possibly empty or "__"-
// bearing) tool part ("mcp__srv__a__b" -> tool "a__b").
func parseMCPName(s string) (server, tool string, hasTool, ok bool) {
	if !strings.HasPrefix(s, mcpRulePrefix) {
		return "", "", false, false
	}
	parts := strings.Split(s, "__")
	if len(parts) < 2 || parts[0] != "mcp" || parts[1] == "" {
		return "", "", false, false
	}
	server = parts[1]
	if len(parts) > 2 {
		tool = strings.Join(parts[2:], "__")
		hasTool = true
	}
	return server, tool, hasTool, true
}

// mcpRuleCovers reports whether a content-less MCP rule name covers an MCP tool
// whose full name is `toolName`. The server segment must match exactly
// (case-sensitive); then:
//
//   - "mcp__srv" (no tool part) or "mcp__srv__*" covers EVERY tool of srv;
//   - "mcp__srv__foo*" (a glob tool part) glob-matches the tool part;
//   - a non-glob exact "mcp__srv__tool" is NOT covered here — it is instead
//     matched by the plain string-equality branch of
//     toolNameMatches.
//
// The server is compared only EXACTLY (it is not globbed); a rule
// that globs the server such as "mcp__sr*__tool" is still handled, because it
// contains "*" and so matches via the glob branch of toolNameMatches on the
// full name.
func mcpRuleCovers(ruleName, toolName string) bool {
	rs, rt, rHasTool, ok := parseMCPName(ruleName)
	if !ok {
		return false
	}
	ts, tt, tHasTool, ok := parseMCPName(toolName)
	if !ok {
		return false
	}
	if rs != ts {
		return false
	}
	if !rHasTool || rt == "*" {
		return true
	}
	if strings.Contains(rt, "*") {
		return tHasTool && globMatch(rt, tt)
	}
	return false
}

// globMatch is the "*" wildcard matcher:
// a pattern of literal runs separated by "*", where each "*" matches any run of
// characters (including none). The leading and trailing literals are anchored
// to the start/end; interior literals are found left-to-right in between. A
// pattern with no "*" is an exact comparison.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) {
		return false
	}
	if !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	lo, hi := len(first), len(s)-len(last)
	for _, mid := range parts[1 : len(parts)-1] {
		if mid == "" {
			continue
		}
		idx := strings.Index(s[lo:hi], mid)
		if idx < 0 {
			return false
		}
		lo += idx + len(mid)
	}
	return true
}

// segmentMatch is the separator-aware glob:
// both strings are walked segment by segment, split on any byte in `seps`; the
// separator bytes must line up and each segment pair must globMatch. Unlike
// globMatch, a "*" here does NOT cross a separator. Used by the WebFetch domain
// matcher with seps=".:".
func segmentMatch(pattern, s, seps string) bool {
	pi, si := 0, 0
	for {
		pe := indexAnyFrom(pattern, seps, pi)
		se := indexAnyFrom(s, seps, si)
		var pc, sc byte
		if pe < len(pattern) {
			pc = pattern[pe]
		}
		if se < len(s) {
			sc = s[se]
		}
		if pc != sc {
			return false
		}
		if !globMatch(pattern[pi:pe], s[si:se]) {
			return false
		}
		if pe == len(pattern) {
			return true
		}
		pi, si = pe+1, se+1
	}
}

// indexAnyFrom returns the index of the first byte of `chars` at or after `from`
// in s, or len(s) if none.
func indexAnyFrom(s, chars string, from int) int {
	for i := from; i < len(s); i++ {
		if strings.IndexByte(chars, s[i]) >= 0 {
			return i
		}
	}
	return len(s)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// normalizeDomain normalizes a bare host (the part after "domain:"):
// lowercase, then strip a run of trailing dots when the byte before the run is
// neither "*" nor "." (keeping an optional ":<port>" suffix). IDN uppercasing
// of non-ASCII bytes is a no-op for ASCII hosts and is not performed
// (documented limitation).
func normalizeDomain(host string) string {
	host = strings.ToLower(host)
	port := ""
	if i := strings.LastIndex(host, ":"); i >= 0 && isDigits(host[i+1:]) {
		port, host = host[i:], host[:i]
	}
	j := len(host)
	for j > 0 && host[j-1] == '.' {
		j--
	}
	if j < len(host) && j > 0 && host[j-1] != '*' {
		host = host[:j]
	}
	return host + port
}

// DomainRuleMatches reports whether a "domain:" rule pattern matches a
// "domain:<host>" call content:
//
//   - matching is case-insensitive (hosts are lowercased by normalizeDomain);
//   - "domain:*" matches every host;
//   - a "domain:*.base" pattern matches STRICT subdomains of base (the wildcard
//     must consume at least base's label count plus one more label, and the
//     consumed prefix must be a clean, non-empty, "."-free-of-leading/".."-free,
//     ":"-free label run) — it does not match base itself;
//   - any other pattern is a per-label glob match (segmentMatch on ".:"),
//     which reduces to an exact comparison when the pattern has no "*".
func DomainRuleMatches(pattern, content string) bool {
	if !strings.HasPrefix(pattern, domainRulePrefix) || !strings.HasPrefix(content, domainRulePrefix) {
		return false
	}
	ph := normalizeDomain(strings.TrimPrefix(pattern, domainRulePrefix))
	ch := normalizeDomain(strings.TrimPrefix(content, domainRulePrefix))
	if ph == "*" {
		return true
	}
	const seps = ".:"
	if !strings.HasPrefix(ph, "*.") {
		return segmentMatch(ph, ch, seps)
	}
	base := ph[2:]
	labels := 1
	for i := 0; i < len(base); i++ {
		if base[i] == '.' || base[i] == ':' {
			labels++
		}
	}
	s := len(ch)
	for labels > 0 && s > 0 {
		s--
		if ch[s] == '.' || ch[s] == ':' {
			labels--
		}
	}
	prefix := ch[:s+1]
	if labels != 0 || strings.HasPrefix(prefix, ".") || strings.Contains(prefix, "..") || strings.Contains(prefix, ":") {
		return false
	}
	return segmentMatch(base, ch[s+1:], seps)
}

// RuleSet is the ordered settings rule list. The first rule of the requested
// behavior that matches a call decides (deny is always consulted before ask
// before allow).
type RuleSet []Rule

// Match returns the first rule of behavior b that matches the call, or nil.
func (rs RuleSet) Match(call Call, b RuleBehavior) *Rule {
	for i := range rs {
		if rs[i].Behavior == b && rs[i].matches(call) {
			r := rs[i]
			return &r
		}
	}
	return nil
}

// Call describes a single tool invocation for the permission engine.
type Call struct {
	// Tool is the tool kind being invoked.
	Tool protocol.Tool
	// Rule overrides the rule tool-name to match against (e.g. an MCP tool's
	// namespaced prefix). Empty means use Tool.
	Rule string
	// RuleContent is the content a settings rule is matched against: the Bash
	// command, the path, etc. Empty matches only whole-tool rules.
	RuleContent string
	// Path is the resolved filesystem path for a path-based tool, consulted by
	// the pathguard RuleEngine. Empty skips the path-guard consult.
	Path string
	// Op is the filesystem operation for a path-based tool.
	Op pathguard.Operation
	// PathCanonical reports whether Path was successfully canonicalized
	// (symlinks resolved), as reported by pathguard RuleEngine.Evaluate.
	PathCanonical bool
}

// Decider orchestrates the permission decision. It holds the tool Registry
// (for descriptor metadata) and an optional pathguard RuleEngine consulted for
// path-based tools.
type Decider struct {
	Registry *Registry
	// PathGuard, when set, is consulted for a Call carrying a Path (outside
	// bypass mode): a refusal is a safety-check deny. It is the seam onto
	// internal/pathguard (pathguard.RuleEngine, e.g. RuleSetEngine).
	PathGuard pathguard.RuleEngine
	// Files, when set, carries the session-specific file-write checks the
	// edit tools' own permission check applies:
	// the internal hard-deny and allowlist, the settings-file /
	// sensitive-file write safety and the acceptEdits working-directory
	// fast path. Nil keeps the generic flow.
	Files *pathguard.SessionFiles
	// PathRules, when set, are the roots file-path rule contents resolve
	// against ("./x", "~/x", "/x", "//x"). Nil derives them from Files, or
	// from the process working directory and home when Files is nil too.
	PathRules *pathguard.PathRuleRoots
	// ReadableDirs are extra absolute directories that read-only file tools
	// may read like the working directories (e.g. the directory holding this
	// session's saved oversized tool results).
	ReadableDirs []string
}

// Decide computes the allow/ask/deny Decision for a tool call under the current
// mode and settings rules, then merges a PreToolUse hook result (when non-nil).
// The precedence is:
//
//  1. DENY settings rule — wins over everything, including bypass.
//  2. path-guard safety refusal (path tools, non-bypass) — safety-check deny.
//     For an edit tool's write with Files set, the session file checks run
//     around this step (1a: hard denies in every mode; 2a: ask rule, session
//     allowlist, unsafe-target safety ask, plan hold,
//     acceptEdits-within-working-dirs).
//  3. plan-mode write floor — a non-read-only tool in plan mode is HELD for
//     approval (behavior:"ask"), not hard-denied: the interactive prompt
//     lets the user approve the action and exit plan mode. With no prompter
//     (-p) the "ask" resolves to a safe denial downstream (resolveAsk).
//  4. ASK settings rule — escalate to a prompt.
//  5. bypass / acceptEdits mode allow — bypass allows all remaining; acceptEdits
//     auto-allows edit tools.
//  6. ALLOW settings rule.
//  7. mode default — dontAsk->deny, auto->classify (an ask with
//     Classify set, decided by the dispatcher's classifier), else ask.
//
// Then mergeHook applies the hook's verdict by strictness (deny>ask>allow), so
// a PreToolUse hook deny overrides a base allow.
func (d *Decider) Decide(call Call, mode Mode, rules RuleSet, hook *hooks.Decision) Decision {
	base := d.decideBase(call, mode, rules)
	return mergeHook(base, hook)
}

func (d *Decider) decideBase(call Call, mode Mode, rules RuleSet) Decision {
	desc, _ := d.descriptor(call.Tool)
	sp := &spellingCache{}

	// 1. DENY settings rule — highest precedence (enforced even under bypass).
	if r := d.matchRule(rules, call, sp, RuleDeny); r != nil {
		return Decision{
			Behavior:   BehaviorDeny,
			ReasonType: ReasonRule,
			Reason:     "Permission to use " + string(call.Tool) + " has been denied by a deny rule.",
			Rule:       r,
		}
	}

	// 1a. Session file checks for an edit tool's write. The
	//     internal hard denies (host credentials, seed-admin, settings-review
	//     store, a bg fork's adopt.json, a diskless session) are returned from
	//     the tool's own check, so they hold in every mode including bypass
	//     (a tool-check deny is returned before the bypass gate). They are
	//     checked before an ask rule here, which is at least as strict as
	//     consulting a path ask rule first.
	fileWrite := d.Files != nil && call.Path != "" && desc.EditTool && call.Op.Mutating()
	var spellings []string
	var internal pathguard.FileVerdict
	if fileWrite {
		res := d.Files.ResolvePath(call.Path)
		spellings = res.Spellings
		internal = d.Files.CheckInternalWrite(call.Path, spellings, mode == ModePlan)
		if internal.Behavior == pathguard.FileDeny {
			return Decision{Behavior: BehaviorDeny, ReasonType: ReasonSafetyCheck, Reason: internal.Message}
		}
		// A write whose walk could not settle where it leads (a link or
		// directory that could not be examined — or a probe that timed out on
		// a stalled mount) is refused, unless the allowlist above already
		// allowed it.
		if res.Unresolved && internal.Behavior != pathguard.FileAllow {
			return Decision{Behavior: BehaviorDeny, ReasonType: ReasonOther, Reason: pathguard.MsgUnresolvedPath("write to", call.Path)}
		}
	}

	// 1b. The read check's network screen for Read / Glob / Grep. The
	//     permission walk never steps onto a
	//     network share or automounter; a path it cannot settle is refused in
	//     every mode (a tool-check deny returns before the bypass
	//     gate), and a path that is — or leads through a link onto — a UNC
	//     share, a /net automount path, the /Network browse surface or a
	//     kernel-redirected /.vol-style path outside the trusted network
	//     directories asks as a safety check, ahead of ask and
	//     allow rules and of the read-only auto-allow. Under bypass that ask is
	//     skipped like the other safety asks without a bypass-immune circuit
	//     breaker.
	if d.Files != nil && call.Path != "" && !call.Op.Mutating() && networkScreenedRead(call.Tool) {
		res := d.Files.ResolvePath(call.Path)
		// permissions.blockReadsOutsideWorkingDirectories: a read with any
		// spelling (requested or symlink-resolved) outside the working
		// directories is refused as a safety check in every mode, bypass
		// included — it is a tool-check deny, returned before the bypass
		// gate. It is judged before the unresolved-walk refusal.
		if dec := d.Files.ReadsBlockedOutside(res); dec != nil {
			reason := pathguard.ReasonReadsOutsideBlocked
			if dec.Reason != nil && dec.Reason.Reason != "" {
				reason = dec.Reason.Reason
			}
			return Decision{Behavior: BehaviorDeny, ReasonType: ReasonSafetyCheck, Reason: reason}
		}
		if res.Unresolved {
			return Decision{Behavior: BehaviorDeny, ReasonType: ReasonOther, Reason: pathguard.MsgUnresolvedPath("read from", call.Path)}
		}
		if mode != ModeBypassPermissions {
			if rs := d.Files.CheckReadNetworkSafety(call.Path, res.Spellings, ""); !rs.Safe {
				rt := ReasonOther
				if rs.SafetyCheck {
					rt = ReasonSafetyCheck
				}
				return Decision{Behavior: BehaviorAsk, ReasonType: rt, Reason: rs.Message}
			}
		}
	}

	// 2. Path-guard safety refusal for path-based tools. Bypass skips path
	//    safety (the resolver never refuses under bypass).
	if call.Path != "" && d.PathGuard != nil && mode != ModeBypassPermissions {
		if dec := d.PathGuard.Evaluate(call.Path, call.Op, call.PathCanonical); dec != nil && !dec.Allowed {
			reason := "Path " + call.Path + " is not permitted."
			if dec.Reason != nil && dec.Reason.Reason != "" {
				reason = dec.Reason.Reason
			}
			return Decision{Behavior: BehaviorDeny, ReasonType: ReasonSafetyCheck, Reason: reason}
		}
	}

	// 2a. The rest of the edit tool's write check, in order: a path ask
	//     rule asks; the session allowlist (the session's plan file,
	//     agent-memory .md files, …) allows; an unsafe target (a gocode settings file, a
	//     project .gocode command/agent/skill, a sensitive file such as .bashrc
	//     or anything under .git) asks as a safetyCheck that no allow rule or
	//     mode short-circuits; plan mode then holds the write ("Cannot write to
	//     <path> while in plan mode."); acceptEdits auto-allows only inside the
	//     working directories and otherwise needs an allow rule or a prompt.
	//     bypassPermissions skips this block: none of these asks carries a
	//     bypass-immune circuit breaker.
	if fileWrite && mode != ModeBypassPermissions {
		if r := d.matchRule(rules, call, sp, RuleAsk); r != nil {
			return Decision{
				Behavior:   BehaviorAsk,
				ReasonType: ReasonRule,
				Reason:     string(call.Tool) + " requires approval (ask rule).",
				Rule:       r,
			}
		}
		if internal.Behavior == pathguard.FileAllow {
			return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: internal.Message}
		}
		if ws := d.Files.CheckWriteSafety(call.Path, spellings); !ws.Safe {
			return Decision{Behavior: BehaviorAsk, ReasonType: ReasonSafetyCheck, Reason: ws.Message}
		}
		if mode == ModePlan {
			return Decision{
				Behavior:   BehaviorAsk,
				ReasonType: ReasonMode,
				Reason:     "Cannot write to " + pathguard.EscapeControl(call.Path) + " while in plan mode.",
				Mode:       ModePlan,
			}
		}
		// Auto mode first simulates acceptEdits ("Skipping auto mode
		// classifier for <tool>: would be allowed in acceptEdits mode"): an edit
		// inside the working directories, or one an allow rule grants, runs
		// without the classifier; any other edit goes to the classifier.
		if mode == ModeAcceptEdits || mode == ModeAuto {
			if len(d.Files.WorkingDirs()) == 0 || d.Files.WithinWorkingDirs(spellings) {
				if mode == ModeAuto {
					return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: "auto mode: would be allowed in acceptEdits mode", Mode: ModeAuto}
				}
				return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: "accept edits mode", Mode: ModeAcceptEdits}
			}
			if r := d.matchRule(rules, call, sp, RuleAllow); r != nil {
				return Decision{
					Behavior:   BehaviorAllow,
					ReasonType: ReasonRule,
					Reason:     string(call.Tool) + " is allowed by an allow rule.",
					Rule:       r,
				}
			}
			if mode == ModeAuto {
				return Decision{Behavior: BehaviorAsk, ReasonType: ReasonMode, Reason: pathguard.MsgWriteNotGranted(call.Path), Mode: ModeAuto, Classify: true}
			}
			return Decision{Behavior: BehaviorAsk, ReasonType: ReasonOther, Reason: pathguard.MsgWriteNotGranted(call.Path), Mode: ModeAcceptEdits}
		}
	}

	// 2b. A tool whose call is a question to the user always asks (after a
	//     deny rule); its own dialog is the ask, so the interactive host shows
	//     the question rather than a permission prompt. It is never held by the
	//     plan floor or sent to the auto mode classifier. dontAsk denies it
	//     (via the mode default below); bypass lets it through to the tool.
	if desc.RequiresUserInteraction && mode != ModeBypassPermissions && mode != ModeDontAsk {
		dec := Decision{Behavior: BehaviorAsk, ReasonType: ReasonUserInteraction, Reason: string(call.Tool) + " asks the user directly.", Mode: mode}
		if r := d.matchRule(rules, call, sp, RuleAsk); r != nil {
			dec.Rule = r
		}
		return dec
	}

	// 3. Plan-mode write floor: a non-read-only tool in plan mode is HELD for
	//    approval, not hard-denied. It returns behavior:"ask" with
	//    "Cannot call <tool> while in plan mode." — the plan "holds the call" and
	//    the interactive prompt lets the user approve it (and exit plan mode).
	//    When no prompter is wired (-p / headless) resolveAsk turns this "ask"
	//    into a safe denial carrying a plan-specific message, so a
	//    non-interactive run never silently executes a write. A DENY rule and the
	//    path-guard (steps 1–2) run first, so a tool already blocked stays denied.
	//    A tool whose own check self-allows (AutoAllow without DeferInAutoPlan,
	//    e.g. TodoWrite) is not held: the floor only applies to a call its tool
	//    check leaves undecided.
	selfAllows := desc.AutoAllow && !desc.DeferInAutoPlan
	if mode == ModePlan && !selfAllows && !desc.IsReadOnlyFor(call.RuleContent) {
		return Decision{
			Behavior:   BehaviorAsk,
			ReasonType: ReasonMode,
			Reason:     "Cannot call " + string(call.Tool) + " while in plan mode.",
			Mode:       ModePlan,
		}
	}

	// 4. ASK settings rule.
	if r := d.matchRule(rules, call, sp, RuleAsk); r != nil {
		return Decision{
			Behavior:   BehaviorAsk,
			ReasonType: ReasonRule,
			Reason:     string(call.Tool) + " requires approval (ask rule).",
			Rule:       r,
		}
	}

	// 5. Bypass / acceptEdits mode allow.
	if mode == ModeBypassPermissions && !desc.ExcludedFromBypass {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: "bypass permissions mode", Mode: ModeBypassPermissions}
	}
	if mode == ModeAcceptEdits && desc.EditTool {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: "accept edits mode", Mode: ModeAcceptEdits}
	}
	// Auto mode's acceptEdits simulation for an edit tool the session file
	// checks did not see (no session files, or no path).
	if mode == ModeAuto && desc.EditTool && !fileWrite {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: "auto mode: would be allowed in acceptEdits mode", Mode: ModeAuto}
	}
	// Agent/Skill self-allow via their own checkPermissions. Agent defers in
	// auto/plan (DeferInAutoPlan): in auto the call falls to
	// modeDefault (the auto mode classifier); in plan the read-only Agent is simply not blocked by the
	// plan floor and falls to the normal flow. Skill self-allows in every mode.
	if desc.AutoAllow && !(desc.DeferInAutoPlan && (mode == ModeAuto || mode == ModePlan)) {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: string(call.Tool) + " is allowed by the tool's own permission check."}
	}

	// 6. ALLOW settings rule.
	if r := d.matchRule(rules, call, sp, RuleAllow); r != nil {
		return Decision{
			Behavior:   BehaviorAllow,
			ReasonType: ReasonRule,
			Reason:     string(call.Tool) + " is allowed by an allow rule.",
			Rule:       r,
		}
	}

	// A read-only Bash command is allowed by the Bash tool's own permission
	// check once no deny / ask / allow rule decided — in every mode, as the
	// tool check answers before the mode default — provided every path it
	// reads lies inside the working directories (or a Read allow rule covers
	// it); a Read deny rule on a path it reads denies; any other path falls
	// through to the mode default (ask).
	if call.Tool == protocol.ToolBash && desc.ReadOnlyFor != nil {
		if dec, ok := d.bashReadOnlyDecision(call, rules); ok {
			return dec
		}
	}

	// Read / Glob / Grep: the read tool's own check (after the deny,
	// blockReadsOutsideWorkingDirectories, network-screen and ask-rule steps
	// consulted above, and the allow-rule lookup of step 6): a read inside the
	// working directories (the launch directory plus --add-dir /
	// permissions.additionalDirectories) is allowed in every mode; an Edit
	// allow rule covering the path implies read; anything else asks
	// with a workingDir reason, whose prompt offers a session Read(<dir>/**)
	// rule. Plan mode asks like default (the check maps plan to default).
	// The mode then shapes that ask below: dontAsk denies, bypass
	// already allowed (step 5), and auto allows it — Read/Glob/Grep are on
	// auto mode's safe allowlist and never reach the classifier.
	if networkScreenedRead(call.Tool) {
		if dec, ok := d.readToolDecision(call, mode, rules, sp); ok {
			return dec
		}
	}

	// The browser READ tools (read_page, get_page_text, tabs_context, screenshot,
	// find) only observe browser state (no navigation, no tab mutation, no
	// input), so they lean auto-allow in the normal interactive modes. The
	// browser ACTION tools (navigate, tabs_create, tabs_close, computer,
	// form_input, browser_batch) are deliberately NOT here — they act on
	// logged-in sites and fall through to the ask gate like Bash. A deny/ask
	// settings rule on any of these still wins (it was consulted in steps 1
	// and 4).
	switch call.Tool {
	case protocol.ToolBrowserReadPage, protocol.ToolBrowserGetPageText,
		protocol.ToolBrowserTabsContext, protocol.ToolBrowserScreenshot,
		protocol.ToolBrowserFind:
		switch mode {
		case ModeDefault, ModeAcceptEdits, ModePlan, ModeAuto:
			return Decision{Behavior: BehaviorAllow, ReasonType: ReasonOther, Reason: string(call.Tool) + " is a read-only tool."}
		}
	}

	// Auto mode's safe allowlist: tools that only read, plan or track work
	// skip the classifier (Read, Grep, Glob, LSP, ToolSearch,
	// the MCP resource readers, TodoWrite, the Task tools, Enter/ExitPlanMode).
	if mode == ModeAuto && autoModeAllowlisted(call.Tool) {
		return Decision{Behavior: BehaviorAllow, ReasonType: ReasonMode, Reason: string(call.Tool) + " is on the auto mode safe allowlist.", Mode: ModeAuto}
	}

	// 7. Mode default: dontAsk->deny, auto->classify, else ask.
	return modeDefault(mode, call.Tool)
}

// networkScreenedRead reports the read tools whose own permission check
// includes the network screen (Read, Glob, Grep and LSP — the `getPath` tools).
func networkScreenedRead(t protocol.Tool) bool {
	switch t {
	case protocol.ToolRead, protocol.ToolGlob, protocol.ToolGrep, protocol.ToolLSP:
		return true
	}
	return false
}

// modeDefault is the fallback verdict when no rule matched and no mode
// short-circuit applied: dontAsk denies, auto routes to
// the AI safety classifier (an ask carrying Classify), anything else asks.
func modeDefault(mode Mode, tool protocol.Tool) Decision {
	switch mode {
	case ModeDontAsk:
		return Decision{Behavior: BehaviorDeny, ReasonType: ReasonMode, Reason: string(tool) + " was not granted and this session does not ask.", Mode: ModeDontAsk}
	case ModeBypassPermissions:
		// Reached only for a bypass-excluded tool; still falls through to ask.
		return Decision{Behavior: BehaviorAsk, ReasonType: ReasonMode, Reason: string(tool) + " requires approval.", Mode: mode}
	case ModeAuto:
		// The auto mode safety classifier decides (the dispatcher runs it).
		// Deny / ask rules, the path checks, the plan floor and
		// blockReadsOutsideWorkingDirectories were all applied first.
		return Decision{Behavior: BehaviorAsk, ReasonType: ReasonMode, Reason: string(tool) + " requires the auto mode classifier's review.", Mode: ModeAuto, Classify: true}
	default:
		return Decision{Behavior: BehaviorAsk, ReasonType: ReasonOther, Reason: string(tool) + " requires approval.", Mode: mode}
	}
}

// descriptor returns the registered descriptor for a tool, or the default
// descriptor (read-only false, concurrency-safe false) when the tool
// is unknown.
func (d *Decider) descriptor(tool protocol.Tool) (ToolDescriptor, bool) {
	if d.Registry != nil {
		if desc, ok := d.Registry.Lookup(tool); ok {
			return desc, true
		}
	}
	return ToolDescriptor{Name: tool, ReadOnly: false, ConcurrencySafe: false}, false
}

// mergeHook applies a PreToolUse hook's verdict onto the base decision by
// strictness: deny > ask > allow. A hook with no opinion (nil or BehaviorNone)
// leaves the base untouched; a hook updatedInput is always carried through.
func mergeHook(base Decision, hook *hooks.Decision) Decision {
	if hook == nil {
		return base
	}
	if hook.UpdatedInput != nil {
		base.UpdatedInput = hook.UpdatedInput
	}
	hb, ok := hookBehavior(hook.Behavior)
	if !ok {
		return base // BehaviorNone / unknown: no opinion.
	}
	if strictness(hb) <= strictness(base.Behavior) {
		return base // base is at least as strict; keep it.
	}
	// Hook is stricter: it decides.
	out := Decision{Behavior: hb, ReasonType: ReasonHook, Reason: hook.Reason, UpdatedInput: base.UpdatedInput}
	if out.Reason == "" {
		out.Reason = "A PreToolUse hook " + string(hb) + "ed this tool call."
	}
	return out
}

// hookBehavior maps a hooks.Behavior onto a toolreg Behavior. BehaviorNone (and
// anything unrecognized) reports ok=false.
func hookBehavior(b hooks.Behavior) (Behavior, bool) {
	switch b {
	case hooks.BehaviorAllow:
		return BehaviorAllow, true
	case hooks.BehaviorAsk:
		return BehaviorAsk, true
	case hooks.BehaviorDeny:
		return BehaviorDeny, true
	default:
		return "", false
	}
}

// strictness orders the behaviors: deny (2) > ask (1) > allow (0).
func strictness(b Behavior) int {
	switch b {
	case BehaviorDeny:
		return 2
	case BehaviorAsk:
		return 1
	default:
		return 0
	}
}
