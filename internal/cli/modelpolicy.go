package cli

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// This file is the single implementation of the availableModels /
// enforceAvailableModels / deniedModels model policy. Every place a model is
// chosen checks it here: the /model picker and `/model <id>`, --model and the
// persisted "model" setting, --fallback-model / settings fallbackModel,
// subagent model overrides and a skill's `model`.
//
// Model ids are compared on their normalized key (provider.ModelKey:
// lower-case, provider prefix and `[1m]` stripped, "." → "-"), so an
// OpenRouter id "anthropic/claude-opus-4.5" matches the entry
// "claude-opus-4-5"; model ids use the real "claude-" prefix; aliases resolve
// through the provider's alias table (subagentModelAliases). There is no
// server-side entitlement overlay and no gateway probe (provider API keys
// only). The policy's debug-log notices are kept on the policy
// (notices) and are not printed. No telemetry.

// modelPolicy is the effective model policy of a session.
type modelPolicy struct {
	// provider is the session provider; aliases resolve through its table.
	provider string

	// availableSet / available are the cascaded availableModels. The key is
	// replaced, never unioned, across the
	// sources, so the highest-priority source that sets it wins. An empty set
	// list allows nothing.
	availableSet bool
	available    []string
	// enforce is the cascaded enforceAvailableModels.
	enforce bool
	// overrides is the cascaded modelOverrides,
	// canonical model id → provider-specific id.
	overrides map[string]string

	// policy is the composed policySettings; nil
	// when no admin policy applies.
	policy map[string]any
	// policyFailed: an admin policy source exists but failed to load and no
	// surviving tier carries the policy. Every allowlist check then
	// fails closed.
	policyFailed bool
	// policyDegraded: an admin policy source failed to load but a surviving
	// tier loaded.
	policyDegraded bool

	mu      sync.Mutex
	notices []string
}

// loadModelPolicy reads the model policy from the session's effective settings
// sources. The policySettings source is the composed admin policy
// (registry/MDM tiers, managed-settings.json and drop-ins, HKCU fallback).
func loadModelPolicy(env *Env, prov string) *modelPolicy {
	return modelPolicyFrom(settingsload.EffectiveSources(fsfacade.OSFS{}, settingsContext(env), settingsload.CanonicalSourceOrder), prov)
}

// modelPolicyFrom builds the policy from loaded sources (low → high priority).
func modelPolicyFrom(ordered []settingsload.OrderedResult, prov string) *modelPolicy {
	p := &modelPolicy{provider: prov}
	for _, or := range ordered {
		if or.Source == settingspolicy.SourcePolicy {
			switch or.Result.LoadState {
			case settingspolicy.LoadLoaded:
				p.policy = or.Result.Settings
				if p.policy == nil {
					p.policy = map[string]any{}
				}
				for _, e := range or.Result.Errors {
					if e.Severity != "warning" {
						p.policyDegraded = true
					}
				}
			case settingspolicy.LoadAbsent:
			default:
				// A policy source exists but failed to load (and no admin
				// tier survived).
				p.policyFailed = true
			}
		}
		if or.Result.LoadState != settingspolicy.LoadLoaded {
			continue
		}
		s := or.Result.Settings
		if raw, ok := s["availableModels"]; ok {
			p.availableSet, p.available = true, stringList(raw)
		}
		if v, ok := s["enforceAvailableModels"].(bool); ok {
			p.enforce = v
		}
		if m, ok := s["modelOverrides"].(map[string]any); ok {
			if p.overrides == nil {
				p.overrides = map[string]string{}
			}
			for k, v := range m {
				if sv, ok := v.(string); ok {
					p.overrides[k] = sv
				}
			}
		}
	}
	return p
}

// stringList returns the non-empty string entries of a settings array.
func stringList(raw any) []string {
	arr, _ := raw.([]any)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// note records a policy notice once (deduplicated, for the debug log).
func (p *modelPolicy) note(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.notices {
		if n == msg {
			return
		}
	}
	p.notices = append(p.notices, msg)
}

// Notices returns the policy notices recorded so far.
func (p *modelPolicy) Notices() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.notices...)
}

// --- policySettings accessors ------------------------------------------------

func (p *modelPolicy) policyAvailable() ([]string, bool) {
	if p.policy == nil {
		return nil, false
	}
	raw, ok := p.policy["availableModels"]
	if !ok {
		return nil, false
	}
	return stringList(raw), true
}

func (p *modelPolicy) policyOverrides() (map[string]string, bool) {
	if p.policy == nil {
		return nil, false
	}
	m, ok := p.policy["modelOverrides"].(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, true
}

// exactMatch reports policySettings.availableModelsMatch === "exact".
func (p *modelPolicy) exactMatch() bool {
	return p.policy != nil && p.policy["availableModelsMatch"] == "exact"
}

// --- names -------------------------------------------------------------------

// modelAliases are the alias spellings; modelFamilies the
// family aliases an entry may name.
var (
	modelAliases  = []string{"sonnet", "opus", "haiku", "fable", "best", "opusplan"}
	modelFamilies = []string{"sonnet", "opus", "haiku", "fable"}
)

func isModelAlias(s string) bool  { return contains(modelAliases, s) }
func isModelFamily(s string) bool { return contains(modelFamilies, s) }

// stripContext1M drops one trailing `[1m]`.
func stripContext1M(s string) string {
	if len(s) >= 4 && strings.EqualFold(s[len(s)-4:], "[1m]") {
		return s[:len(s)-4]
	}
	return s
}

// modelKey is the comparison form of a model id or entry:
// stripContext1M(trim(lower)), then the provider key normalization (see the
// file comment).
// A bare alias stays as is.
func modelKey(s string) string {
	n := strings.TrimSpace(stripContext1M(strings.ToLower(strings.TrimSpace(s))))
	if n == "" || isModelAlias(n) || n == "default" {
		return n
	}
	return provider.ModelKey(n)
}

// resolveAlias resolves an alias: the provider id it names, "" when the
// provider has none (openai) or s is no alias. "best" names the newest opus,
// "opusplan" resolves to sonnet outside plan mode.
func (p *modelPolicy) resolveAlias(s string) string {
	n := modelKey(s)
	switch n {
	case "best":
		n = "opus"
	case "opusplan":
		n = "sonnet"
	}
	if !isModelFamily(n) {
		return ""
	}
	prov := p.provider
	if prov == "" {
		prov = providerOpenRouter
	}
	return subagentModelAliases[prov][n]
}

// wordContains is `to`: n occurs in e bounded by non-alphanumerics.
func wordContains(e, n string) bool {
	if n == "" {
		return false
	}
	for i := strings.Index(e, n); i != -1; {
		end := i + len(n)
		before := i == 0 || !isAlnum(e[i-1])
		after := end == len(e) || !isAlnum(e[end])
		if before && after {
			return true
		}
		j := strings.Index(e[i+1:], n)
		if j == -1 {
			break
		}
		i += 1 + j
	}
	return false
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// hasSegmentPrefix reports that e starts with n followed by the end or "-".
// This is the version-prefix rule: "claude-opus-4" allows "claude-opus-4-5"
// and "claude-opus-4-5-20251101", never "claude-opus-45".
func hasSegmentPrefix(e, n string) bool {
	return strings.HasPrefix(e, n) && (len(e) == len(n) || e[len(n)] == '-')
}

// familyNarrowed reports that a family entry is narrowed (ignored) when
// another, non-family entry names that family followed by the end or "-" —
// e.g. ["opus", "opus-4-5"] allows only Opus 4.5, not every opus.
func familyNarrowed(family string, entries []string) bool {
	for _, r := range entries {
		if isModelFamily(r) {
			continue
		}
		i := strings.Index(r, family)
		if i == -1 {
			continue
		}
		end := i + len(family)
		if end == len(r) || r[end] == '-' {
			return true
		}
	}
	return false
}

// --- the model-id parser -------------------------------------------

// parsedModelID is the structured form of a Claude model id.
type parsedModelID struct {
	family   string
	major    int
	minor    int
	hasMinor bool
	legacy   bool // version-first spelling ("claude-3-5-sonnet")
	base     string
	trailer  string
	hasTrail bool
}

// releaseTail matches what may follow the version without making it a
// different model (a release date, -fast/-latest, a -vN revision).
var releaseTail = regexp.MustCompile(`^(?:-fast|-latest)?(?:-v\d{1,3}@\d{8}|[-@]\d{8})?(?:-v\d{1,3}(?::\d{1,3})?)?$`)

// readVersionNum reads 1–2 digits at s[i:] not followed by another digit
// (the `\d{1,2}(?!\d)` of a regexp).
func readVersionNum(s string, i int) (int, int, bool) {
	j := i
	for j < len(s) && j-i < 2 && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i || (j < len(s) && s[j] >= '0' && s[j] <= '9') {
		return 0, i, false
	}
	n := 0
	for _, c := range s[i:j] {
		n = n*10 + int(c-'0')
	}
	return n, j, true
}

// readWord reads [a-z]+ at s[i:].
func readWord(s string, i int) (string, int) {
	j := i
	for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
		j++
	}
	return s[i:j], j
}

// parseModelID parses a model key: nil (false) for anything that is not a
// Claude model id.
func parseModelID(s string) (parsedModelID, bool) {
	n := modelKey(s)
	const pfx = "claude-"
	if !strings.HasPrefix(n, pfx) {
		return parsedModelID{}, false
	}
	var id parsedModelID
	i := len(pfx)
	if fam, j := readWord(n, i); fam != "" && j < len(n) && n[j] == '-' {
		// claude-<family>-<major>[-<minor>]
		major, k, ok := readVersionNum(n, j+1)
		if !ok {
			return parsedModelID{}, false
		}
		id = parsedModelID{family: fam, major: major, base: n[:k]}
		if k < len(n) && n[k] == '-' {
			if minor, m, ok := readVersionNum(n, k+1); ok {
				id.minor, id.hasMinor, id.base = minor, true, n[:m]
			}
		}
	} else {
		// claude-<major>[-<minor>]-<family> (legacy, version first)
		major, k, ok := readVersionNum(n, i)
		if !ok {
			return parsedModelID{}, false
		}
		id = parsedModelID{major: major, legacy: true}
		if k < len(n) && n[k] == '-' {
			if minor, m, ok := readVersionNum(n, k+1); ok && m < len(n) && n[m] == '-' {
				id.minor, id.hasMinor, k = minor, true, m
			}
		}
		if k >= len(n) || n[k] != '-' {
			return parsedModelID{}, false
		}
		fam, m := readWord(n, k+1)
		if fam == "" {
			return parsedModelID{}, false
		}
		id.family, id.base = fam, n[:m]
	}
	rest := n[len(id.base):]
	if rest != "" && rest[0] != '-' && rest[0] != '@' {
		return parsedModelID{}, false
	}
	if !releaseTail.MatchString(rest) {
		id.trailer, id.hasTrail = rest, true
	}
	return id, true
}

// sameVersion reports whether two parsed ids name the same version.
func sameVersion(a, b parsedModelID) bool {
	return a.family == b.family && a.legacy == b.legacy && a.major == b.major && a.minor == b.minor
}

// isLatestSpelling reports that the id has no trailer and its tail starts
// "-latest".
func isLatestSpelling(s string, id parsedModelID) bool {
	if id.hasTrail {
		return false
	}
	k := modelKey(s)
	return strings.HasPrefix(k[min(len(id.base), len(k)):], "-latest")
}

// entryKind classifies an availableModels entry.
type entryKind int

const (
	entryIgnored entryKind = iota
	entryFamily
	entryModel
	entryLiteral
)

type classifiedEntry struct {
	kind   entryKind
	family string
	id     parsedModelID
	latest bool
}

// classifyEntry classifies an availableModels entry.
func classifyEntry(e string) classifiedEntry {
	n := modelKey(e)
	if n == "" {
		return classifiedEntry{kind: entryIgnored}
	}
	if isModelFamily(n) {
		return classifiedEntry{kind: entryFamily, family: n}
	}
	if r := strings.TrimPrefix(n, "claude-"); r != n && isModelFamily(r) {
		return classifiedEntry{kind: entryFamily, family: r}
	}
	if isModelAlias(n) || n == "default" {
		return classifiedEntry{kind: entryIgnored}
	}
	s := n
	if _, ok := parseModelID(n); !ok && !strings.HasPrefix(n, "claude-") {
		s = "claude-" + n
	}
	if id, ok := parseModelID(s); ok {
		return classifiedEntry{kind: entryModel, id: id, latest: isLatestSpelling(s, id)}
	}
	return classifiedEntry{kind: entryLiteral}
}

// --- the allowlist check -------------------------------------------

// allowOptions are the options of the allow check.
type allowOptions struct {
	// allowlist, when set, replaces the cascaded availableModels (and skips
	// the policy-load-failure refusal).
	allowlist    []string
	hasAllowlist bool
	// overrides, when set, is the modelOverrides map used for the reverse
	// mapping; ignoreOverrides skips it.
	overrides       map[string]string
	hasOverrides    bool
	ignoreOverrides bool
}

// allows reports whether model passes the availableModels allowlist and the
// deniedModels denylist. No allowlist allows everything, an empty
// one nothing; a policy source that failed to load refuses every model.
func (p *modelPolicy) allows(model string) bool {
	return p.allowsWith(model, allowOptions{})
}

func (p *modelPolicy) allowsWith(model string, o allowOptions) bool {
	if p == nil {
		return true
	}
	if p.denied(model) {
		return false
	}
	if !o.hasAllowlist && p.policyFailed {
		p.note("availableModels: a policy source exists but failed to load; refusing every model outside the default until it is fixed")
		return false
	}
	list, set := p.available, p.availableSet
	if o.hasAllowlist {
		list, set = o.allowlist, true
	}
	if !set {
		return true
	}
	if len(list) == 0 {
		return false
	}
	exact := p.exactMatch()
	var entries []string
	for _, e := range list {
		if exact && classifyEntry(e).kind == entryIgnored {
			continue
		}
		entries = append(entries, modelKey(e))
	}
	if len(entries) == 0 {
		return false
	}
	s := modelKey(model)
	if s == "" {
		return false
	}
	// Exact matching: an alias spelling is allowed only when the model it
	// names is allowed too.
	if exact && isModelAlias(s) {
		if r := p.resolveAlias(s); r != "" && !p.allowsWith(r, o) {
			return false
		}
	}
	if contains(entries, s) && !isModelFamily(s) {
		if !isModelAlias(s) || p.aliasAllowed(s, o) {
			return true
		}
	}
	// The modelOverrides reverse mapping: a provider-specific id
	// an admin mapped a canonical model to counts as that canonical model.
	c := modelKey(p.unmapOverride(model, o))
	if contains(entries, c) && (!isModelFamily(c) || !familyNarrowed(c, entries)) {
		if c != s || !isModelAlias(c) || p.aliasAllowed(c, o) {
			return true
		}
	}
	// The concrete id an alias candidate names.
	concrete := c
	if isModelAlias(c) {
		concrete = modelKey(p.resolveAlias(c))
	}
	for _, r := range entries {
		if isModelFamily(r) && !familyNarrowed(r, entries) && concrete != "" && wordContains(concrete, r) {
			return true
		}
	}
	if isModelAlias(c) && concrete != "" && contains(entries, concrete) {
		return true
	}
	for _, r := range entries {
		if !isModelFamily(r) && isModelAlias(r) {
			if res := modelKey(p.resolveAlias(r)); res != "" && res == c {
				return true
			}
		}
	}
	if concrete == "" {
		return false
	}
	for _, r := range entries {
		if isModelFamily(r) || isModelAlias(r) {
			continue
		}
		prefix := hasSegmentPrefix(concrete, r) || (!strings.HasPrefix(r, "claude-") && hasSegmentPrefix(concrete, "claude-"+r))
		if prefix && (!exact || exactEntryAllows(concrete, r)) {
			return true
		}
	}
	return false
}

// aliasAllowed reports that an alias is allowed when the model it resolves to
// is.
func (p *modelPolicy) aliasAllowed(alias string, o allowOptions) bool {
	r := p.resolveAlias(alias)
	if r == "" {
		return false
	}
	return p.allowsWith(r, o)
}

// unmapOverride returns the canonical id a provider-specific id was mapped
// from in modelOverrides (the policy's own map when the policy sets
// availableModels), or model itself.
func (p *modelPolicy) unmapOverride(model string, o allowOptions) string {
	var m map[string]string
	switch {
	case o.hasOverrides:
		m = o.overrides
	case o.ignoreOverrides:
		return model
	default:
		if _, ok := p.policyAvailable(); ok {
			m, _ = p.policyOverrides()
		} else {
			m = p.overrides
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(stripContext1M(m[k]), stripContext1M(model)) {
			return k
		}
	}
	return model
}

// exactEntryAllows applies availableModelsMatch "exact": a version entry
// allows only that exact version (with its release-date variants), not the
// versions it prefixes; a family entry still allows its family.
func exactEntryAllows(candidate, entry string) bool {
	ce := classifyEntry(entry)
	switch ce.kind {
	case entryFamily:
		return true
	case entryModel:
		id, ok := parseModelID(candidate)
		if !ok {
			return false
		}
		return sameVersion(ce.id, id) && ce.id.trailer == id.trailer && (!isLatestSpelling(candidate, id) || ce.latest)
	}
	return false
}

// --- deniedModels -------------------------------

type deniedEntry struct {
	kind    entryKind
	family  string
	id      parsedModelID
	literal string
}

// parseDeniedEntry parses one deniedModels entry.
func parseDeniedEntry(e string) (deniedEntry, bool) {
	n := strings.ToLower(strings.TrimSpace(e))
	r := strings.TrimSpace(strings.NewReplacer("[1m]", "", "[2m]", "").Replace(n))
	if r == "" {
		return deniedEntry{}, false
	}
	if isModelFamily(r) {
		return deniedEntry{kind: entryFamily, family: r}, true
	}
	if isModelAlias(r) || r == "default" {
		return deniedEntry{}, false
	}
	if id, ok := parseModelID(r); ok {
		return deniedEntry{kind: entryModel, id: id}, true
	}
	if k := modelKey(r); !strings.HasPrefix(k, "claude-") {
		if id, ok := parseModelID("claude-" + k); ok {
			return deniedEntry{kind: entryModel, id: id}, true
		}
	}
	return deniedEntry{kind: entryLiteral, literal: r}, true
}

// deniedCovers reports whether entry e blocks n: an entry id blocks every
// model of its version (an entry without a minor version blocks the whole
// major), and the trailer it names.
func deniedCovers(e, n parsedModelID) bool {
	if e.family != n.family || e.major != n.major {
		return false
	}
	if e.hasMinor && n.minor != e.minor {
		return false
	}
	if !e.hasTrail {
		return true
	}
	return n.trailer == e.trailer || strings.HasPrefix(n.trailer, e.trailer+"-")
}

// denied reports whether the policy's deniedModels blocks model.
// deniedModels is read from the admin policy only.
func (p *modelPolicy) denied(model string) bool {
	if p == nil || p.policy == nil {
		return false
	}
	var entries []deniedEntry
	for _, e := range stringList(p.policy["deniedModels"]) {
		if de, ok := parseDeniedEntry(e); ok {
			entries = append(entries, de)
		}
	}
	if len(entries) == 0 {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(strings.NewReplacer("[1m]", "", "[2m]", "").Replace(model)))
	cands := []string{raw, modelKey(raw)}
	if r := p.resolveAlias(raw); r != "" {
		cands = append(cands, strings.ToLower(r), modelKey(r))
	}
	// A provider-specific id an admin mapped a canonical model to is that model.
	maps := []map[string]string{p.overrides}
	if m, ok := p.policyOverrides(); ok {
		maps = append(maps, m)
	}
	for _, m := range maps {
		for k, v := range m {
			for _, c := range cands {
				if strings.EqualFold(stripContext1M(v), c) || modelKey(v) == c {
					cands = append(cands, strings.ToLower(k), modelKey(k))
					break
				}
			}
		}
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		id, parsed := parseModelID(c)
		for _, e := range entries {
			switch e.kind {
			case entryLiteral:
				if c == e.literal || c == modelKey(e.literal) {
					return true
				}
			case entryFamily:
				if (parsed && id.family == e.family) || wordContains(c, e.family) {
					return true
				}
			case entryModel:
				if parsed && deniedCovers(e.id, id) {
					return true
				}
			}
		}
	}
	return false
}

// exactBlocked applies availableModelsMatch "exact": a model the policy's own
// list does not allow is blocked even as the default.
func (p *modelPolicy) exactBlocked(model string) bool {
	if p == nil || !p.exactMatch() {
		return false
	}
	list, ok := p.policyAvailable()
	if !ok {
		return false
	}
	usable := false
	for _, e := range list {
		if classifyEntry(e).kind != entryIgnored {
			usable = true
		}
	}
	if !usable {
		return false
	}
	return !p.allowsWith(model, allowOptions{allowlist: list, hasAllowlist: true})
}

// --- the default-model exemption, enforcement, step-down ---------------------

// isDefaultModel reports the session's default model, which is always allowed —
// unless it is the "opusplan"/"haiku" alias, deniedModels names it, or an
// exact-match policy list leaves it out.
func (p *modelPolicy) isDefaultModel(model, defaultModel string) bool {
	if strings.TrimSpace(defaultModel) == "" {
		return false
	}
	n := modelKey(model)
	if n == "opusplan" || n == "haiku" || n == "best" {
		return false
	}
	if p.denied(model) || p.exactBlocked(model) {
		return false
	}
	return n == modelKey(defaultModel)
}

// allowsOrDefault is the "default model or allowed" check of the skill/agent
// paths.
func (p *modelPolicy) allowsOrDefault(model, defaultModel string) bool {
	if p == nil {
		return true
	}
	return p.isDefaultModel(model, defaultModel) || p.allows(model)
}

// restricted reports that some allowlist/denylist applies (or the policy
// failed to load, which refuses every model).
func (p *modelPolicy) restricted() bool {
	if p == nil {
		return false
	}
	if p.availableSet || p.policyFailed {
		return true
	}
	return p.policy != nil && len(stringList(p.policy["deniedModels"])) > 0
}

// enforcementState is gi's state.
type enforcementState int

const (
	enforcementInactive enforcementState = iota
	enforcementActive
	enforcementRefused
)

// modelEnforcement is gi's result.
type modelEnforcement struct {
	state enforcementState
	// cascadeTrusted (inactive only): no admin policy applies, so an
	// enforceAvailableModels set in user/project settings is honoured.
	cascadeTrusted bool
	allowlist      []string
	overrides      map[string]string
}

// enforcement reports whether enforceAvailableModels is in force, and from
// which settings.
func (p *modelPolicy) enforcement() modelEnforcement {
	if p.policyFailed {
		p.note("enforceAvailableModels: a policy source exists but failed to load; refusing cascade-trust mode (model enforcement from user/project settings is disabled until the policy source is fixed)")
		return modelEnforcement{state: enforcementRefused}
	}
	if p.policy == nil {
		return modelEnforcement{state: enforcementInactive, cascadeTrusted: true}
	}
	degraded := func(surviving bool) {
		if !p.policyDegraded {
			return
		}
		if surviving {
			p.note("enforceAvailableModels: an admin policy source failed to load; enforcing the surviving admin tier (the failed source may carry a different policy — fix it to restore full coverage)")
		} else {
			p.note("enforceAvailableModels: an admin policy source failed to load and the surviving admin tier carries no model policy — model enforcement is OFF; the failed source may have carried it")
		}
	}
	list, hasList := p.policyAvailable()
	enforce := p.policy["enforceAvailableModels"] == true
	if enforce && !hasList {
		p.note("enforceAvailableModels: the policy view sets the enforce flag but not availableModels; enforcement is disabled (the flag requires a policy-owned allowlist)")
		degraded(false)
		return modelEnforcement{state: enforcementInactive}
	}
	if !enforce || !hasList || len(list) == 0 {
		degraded(false)
		return modelEnforcement{state: enforcementInactive}
	}
	degraded(true)
	ov, _ := p.policyOverrides()
	return modelEnforcement{state: enforcementActive, allowlist: list, overrides: ov}
}

// enforcedDefault is the default for a session without a chosen model: under
// enforceAvailableModels the Default model is the first availableModels entry
// that names an allowed model. "" when enforcement is off or nothing
// qualifies.
//
// There is no tier default model (the model comes from
// --model, the "model" setting or /model), so this is what a session with no
// chosen model starts on when enforcement is in force.
func (p *modelPolicy) enforcedDefault() string {
	if p == nil {
		return ""
	}
	e := p.enforcement()
	list, enforce := p.available, p.enforce
	switch {
	case e.state == enforcementRefused:
		return ""
	case e.state == enforcementActive:
		list, enforce = e.allowlist, true
	case !e.cascadeTrusted:
		return ""
	}
	if !enforce || len(list) == 0 {
		return ""
	}
	for _, entry := range list {
		ce := classifyEntry(entry)
		var cand string
		switch ce.kind {
		case entryIgnored:
			continue
		case entryFamily:
			if cand = p.resolveAlias(ce.family); cand == "" || !p.allows(cand) {
				cand = p.newestAllowedInFamily(ce.family)
			}
		default:
			cand = p.providerModelID(strings.TrimSpace(entry))
		}
		if cand != "" && p.allows(cand) {
			return cand
		}
	}
	p.note("enforceAvailableModels: no availableModels entry expands to an allowed model; keeping the tier default")
	return ""
}

// providerModelID spells an availableModels entry as a model id the session
// provider accepts ("" when it cannot): OpenRouter ids carry a vendor prefix.
func (p *modelPolicy) providerModelID(entry string) string {
	e := strings.TrimSpace(entry)
	if e == "" {
		return ""
	}
	if p.provider == providerOpenRouter || p.provider == "" {
		if strings.Contains(e, "/") {
			return e
		}
		if strings.HasPrefix(strings.ToLower(e), "claude-") {
			return "anthropic/" + e
		}
		return ""
	}
	return e
}

// newestAllowedInFamily returns the newest built-in model of family the
// policy allows, spelled for the session provider ("" when none).
func (p *modelPolicy) newestAllowedInFamily(family string) string {
	type cand struct {
		key string
		id  parsedModelID
	}
	var cs []cand
	for _, k := range provider.BuiltinModelKeys() {
		id, ok := parseModelID(k)
		if !ok || id.family != family || id.hasTrail {
			continue
		}
		cs = append(cs, cand{k, id})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i].id, cs[j].id
		if a.major != b.major {
			return a.major > b.major
		}
		if a.minor != b.minor {
			return a.minor > b.minor
		}
		return len(cs[i].key) > len(cs[j].key)
	})
	for _, c := range cs {
		id := ""
		switch p.provider {
		case providerAnthropic:
			id = c.key
		case providerOpenRouter, "":
			id = "anthropic/" + c.key
		}
		if id != "" && p.allows(id) {
			return id
		}
	}
	return ""
}

// familyStepDown steps a family alias ("opus", "opus[1m]") outside the
// allowlist down to the newest allowed model of its family. "" when the
// spelling is no family alias, nothing restricts models, or no model of the
// family is allowed.
func (p *modelPolicy) familyStepDown(requested string) string {
	if p == nil {
		return ""
	}
	n := strings.ToLower(strings.TrimSpace(requested))
	r := strings.TrimSpace(stripContext1M(n))
	if !isModelFamily(r) || !p.restricted() {
		return ""
	}
	h := p.newestAllowedInFamily(r)
	if h == "" {
		return ""
	}
	if r != n && provider.Supports1MSuffix(h) {
		return h + "[1m]"
	}
	return h
}

// --- the choice points -------------------------------------------------------

// restrictedModelNotice is the startup notice for a user-specified model
// the policy refused.
func restrictedModelNotice(requested, using string) string {
	if strings.TrimSpace(using) == "" {
		// Port adaptation: with no tier default there may be no model to fall
		// back to; say how to pick one instead of "Using … instead".
		return `Model "` + sanitizeModelName(requested) + `" is restricted by your organization's settings. Run /model to choose a different model.`
	}
	return `Model "` + sanitizeModelName(requested) + `" is restricted by your organization's settings. Using ` + sanitizeModelName(using) + ` instead.`
}

// sanitizeModelName keeps only [A-Za-z0-9._:/@[\]-], capped at 128.
func sanitizeModelName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:/@[]-", c) {
			b.WriteRune(c)
		}
	}
	out := b.String()
	if out == "" {
		return "(unrecognized model name)"
	}
	if r := []rune(out); len(r) > 128 {
		return string(r[:128]) + "…"
	}
	return out
}

// startupModel resolves the user-specified model (--model, else the
// persisted "model" setting): a model the policy refuses steps down within its
// family or is dropped (restricted reports it, for the startup notice); with
// no model and enforceAvailableModels in force the session starts on the
// enforced default.
func (p *modelPolicy) startupModel(requested string) (model, restricted string) {
	req := strings.TrimSpace(requested)
	if req != "" && !p.allows(req) {
		if s := p.familyStepDown(req); s != "" {
			return s, req
		}
		restricted = req
		req = ""
	}
	if req == "" {
		req = p.enforcedDefault()
	}
	return req, restricted
}

// fallbackModels filters the fallback chain to allowed models.
func (p *modelPolicy) fallbackModels(models []string) []string {
	if p == nil || len(models) == 0 {
		return models
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if p.allows(m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// switchModel is the availableModels part of `/model <id>`: an allowed
// model is used as given, a family alias outside the list steps down, and
// anything else is refused with a message.
func (p *modelPolicy) switchModel(requested string) (string, string) {
	req := strings.TrimSpace(requested)
	if p == nil || req == "" || req == "default" || p.allows(req) {
		return req, ""
	}
	if s := p.familyStepDown(req); s != "" {
		return s, ""
	}
	return "", "Model '" + req + "' is not available. Your organization restricts model selection."
}

// pickerModels filters the picker: with a restriction in force it lists only
// the models the policy allows.
func (p *modelPolicy) pickerModels(models []provider.ModelInfo) []provider.ModelInfo {
	if !p.restricted() {
		return models
	}
	out := make([]provider.ModelInfo, 0, len(models))
	for _, m := range models {
		if p.allows(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

// subagentModel returns the model a subagent runs on. A requested model naming
// the parent's runs on the parent; one the policy refuses steps down within
// its family or inherits the parent model. Only the allow check applies — the
// default-model exemption does not.
func (p *modelPolicy) subagentModel(requested, parent string) string {
	resolved := resolveSubagentModel(p.provider, requested, parent)
	if resolved == parent || p.allows(resolved) {
		return resolved
	}
	if s := p.familyStepDown(requested); s != "" {
		p.note(`Subagent model "` + requested + `" is not in the availableModels allowlist; using the newest allowed model in its family instead`)
		return s
	}
	p.note(`Subagent model "` + requested + `" is not in the availableModels allowlist; inheriting the parent model instead`)
	return parent
}
