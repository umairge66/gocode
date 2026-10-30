package redact

import (
	"regexp"
	"strings"
)

// Placeholder is the redaction marker substituted for every detected secret.
const Placeholder = "[REDACTED]"

// tail is the shared trailing boundary group that many
// high-confidence rules append after their captured token:
//
//	(?:[\x60'"\s;]|\\[nr]|$)
//
// It consumes one of: backtick, quote, whitespace, semicolon; or a literal
// backslash-n / backslash-r escape; or end-of-input. This is a consuming
// alternation (not look-ahead), so it works in RE2 unchanged.
const tail = `(?:[\x60'"\s;]|\\[nr]|$)`

// rule is one provider token-detection rule. re always captures the secret in
// submatch group 1 when hasGroup is true; otherwise the whole match is the
// secret. denyTrailing, when non-nil, emulates a trailing negative look-ahead
// (?!class) that RE2 cannot express: a match whose immediately following rune
// matches denyTrailing is rejected.
type rule struct {
	id           string
	re           *regexp.Regexp
	hasGroup     bool
	denyTrailing *regexp.Regexp
}

// Shared sub-patterns.
const (
	// GitLab token body.
	gitlabBody = `[\w=-]{20,}(?:\.[0-9a-z]{9})?`
)

// highConfidenceRules holds the ~60 high-confidence token rules (low-confidence
// context rules are handled separately by the free-text scanner). Each entry's placeholder is the
// shared Placeholder constant.
var highConfidenceRules = buildHighConfidenceRules()

func grp(id, pat string) rule {
	return rule{id: id, re: regexp.MustCompile(pat), hasGroup: true}
}

func whole(id, pat string) rule {
	return rule{id: id, re: regexp.MustCompile(pat), hasGroup: false}
}

// wholeDeny builds a no-capture rule whose trailing negative look-ahead is
// emulated by a denyTrailing rune class.
func wholeDeny(id, pat, deny string) rule {
	return rule{id: id, re: regexp.MustCompile(pat), hasGroup: false, denyTrailing: regexp.MustCompile(deny)}
}

// grpDeny builds a group-capturing rule with an emulated trailing look-ahead.
func grpDeny(id, pat, deny string) rule {
	return rule{id: id, re: regexp.MustCompile(pat), hasGroup: true, denyTrailing: regexp.MustCompile(deny)}
}

func buildHighConfidenceRules() []rule {
	return []rule{
		grp("aws-access-token", `\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})\b`),

		// gcp-api-key: `\b(AIza[\w-]{35})(?![\w-])` — trailing (?![\w-])
		// rewritten to a denyTrailing rune check.
		grpDeny("gcp-api-key", `\b(AIza[\w-]{35})`, `[\w-]`),

		// google-oauth-client-secret: `\bGOCSPX-[\w-]{28}(?![\w-])`.
		wholeDeny("google-oauth-client-secret", `\bGOCSPX-[\w-]{28}`, `[\w-]`),

		// azure-ad-client-secret: leading/trailing are consuming classes, not
		// look-around, so they work in RE2 directly. \x60 = backtick.
		grp("azure-ad-client-secret",
			`(?:^|[\\'"`+"`"+`\s>=:(,)])([a-zA-Z0-9_~.]{3}\dQ~[a-zA-Z0-9_~.-]{31,34})(?:$|[\\'"`+"`"+`\s<),])`),

		grp("digitalocean-pat", `\b(dop_v1_[a-f0-9]{64})`+tail),
		grp("digitalocean-access-token", `\b(doo_v1_[a-f0-9]{64})`+tail),
		grp("gocode-api-key", `\b(sk-ant-api03-[a-zA-Z0-9_\-]{93}AA)`+tail),
		grp("gocode-admin-api-key", `\b(sk-ant-admin01-[a-zA-Z0-9_\-]{93}AA)`+tail),

		// gocode-oauth-token: a `run:"[\w-]"` wrapper
		// ((?:(?<!run)|(?<=\\[nr]))(?=run+tail)run*?source) is unnecessary: because the
		// source starts with \b and ends at the run's tail, the wrapper would only
		// anchor the scan at run starts (a backtracking guard); it accepts the
		// same tokens as the bare RE2-safe source used here.
		grp("gocode-oauth-token", `\b(sk-ant-(?:oat|ort)\d{2}-[\w-]{20,})`+tail),

		// openrouter-api-key: OpenRouter is the primary
		// provider. Keys are sk-or-v1-<64 hex>;
		// the body is matched loosely so a future version/alphabet still hits.
		wholeDeny("openrouter-api-key", `\bsk-or-v\d+-[A-Za-z0-9_-]{32,200}`, `[A-Za-z0-9_-]`),

		// Model-provider keys of the registry providers
		// (internal/provider/registry_specs.go). Avian documents
		// its "avian-" prefix; xAI "xai-", Groq "gsk_" and Cerebras "csk-"
		// are the providers' issued formats though their API docs do not
		// spell them out, so the bodies are matched loosely with a length
		// floor high enough to skip ordinary identifiers. Hugging Face
		// "hf_" tokens are covered by huggingface-access-token below and
		// Gemini "AIza" keys by gcp-api-key above.
		wholeDeny("xai-api-key", `\bxai-[A-Za-z0-9]{40,200}`, `[A-Za-z0-9]`),
		wholeDeny("groq-api-key", `\bgsk_[A-Za-z0-9]{40,200}`, `[A-Za-z0-9]`),
		wholeDeny("cerebras-api-key", `\bcsk-[A-Za-z0-9]{40,200}`, `[A-Za-z0-9]`),
		wholeDeny("avian-api-key", `\bavian-[A-Za-z0-9_-]{24,200}`, `[A-Za-z0-9_-]`),

		whole("openai-api-key", `sk-[A-Za-z0-9_-]{8,200}T3BlbkFJ[A-Za-z0-9_-]{8,200}`),

		// openai-legacy-api-key: `\bsk-[a-zA-Z0-9]{48}(?![a-zA-Z0-9])`.
		wholeDeny("openai-legacy-api-key", `\bsk-[a-zA-Z0-9]{48}`, `[a-zA-Z0-9]`),

		grp("huggingface-access-token", `\b(hf_[a-zA-Z]{34})`+tail),
		whole("supabase-secret-key", `\bsb_secret_[A-Za-z0-9_-]{20,}`),
		whole("supabase-access-token", `\bsbp_[a-z0-9]{40,}`),

		whole("github-pat", `ghp_[0-9a-zA-Z]{36}`),
		whole("github-fine-grained-pat", `github_pat_\w{82}`),
		whole("github-app-token", `(?:ghu|ghs)_[0-9a-zA-Z]{36}`),
		whole("github-oauth", `gho_[0-9a-zA-Z]{36}`),
		whole("github-refresh-token", `ghr_[0-9a-zA-Z]{36}`),

		whole("gitlab-pat", `glpat-`+gitlabBody),
		whole("gitlab-deploy-token", `gldt-`+gitlabBody),
		whole("gitlab-runner-authentication-token", `glrt-`+gitlabBody),
		whole("gitlab-oauth-app-secret", `gloas-`+gitlabBody),
		whole("gitlab-pipeline-trigger-token", `glptt-`+gitlabBody),
		whole("gitlab-kubernetes-agent-token", `glagent-`+gitlabBody),
		whole("gitlab-incoming-mail-token", `glimt-`+gitlabBody),
		whole("gitlab-scim-oauth-token", `glsoat-`+gitlabBody),
		whole("gitlab-ci-build-token", `glcbt-`+gitlabBody),
		whole("gitlab-feed-token", `glft-`+gitlabBody),
		whole("gitlab-feature-flag-client-token", `glffct-`+gitlabBody),

		whole("slack-bot-token", `xoxb-[0-9]{10,13}-[0-9]{10,13}[a-zA-Z0-9-]*`),
		whole("slack-user-token", `xox[a-z](?:-[0-9]{10,13}){3}-[a-zA-Z0-9-]{28,34}`),
		whole("slack-rotation-token", `xoxe(?:\.xox[a-z])?-[0-9]-[A-Za-z0-9-]{28,}`),
		whole("slack-app-token", `(?i)xapp-\d-[A-Z0-9]+-\d+-[a-z0-9]+`),
		whole("slack-workflow-token", `\bxwfp-[a-zA-Z0-9-]{20,}`),
		whole("slack-webhook-url", `(?i)(?:https?://)?hooks\.slack\.com/(?:services|workflows|triggers)/[A-Za-z0-9+/_-]{40,}`),

		whole("twilio-api-key", `SK[0-9a-fA-F]{32}`),
		grp("sendgrid-api-token", `\b(SG\.[a-zA-Z0-9=_\-.]{66})`+tail),
		grp("npm-access-token", `\b(npm_[a-zA-Z0-9]{36})`+tail),
		whole("pypi-upload-token", `pypi-AgEIcHlwaS5vcmc[\w-]{50,1000}`),
		grp("databricks-api-token", `\b(dapi[a-f0-9]{32}(?:-\d)?)`+tail),
		whole("hashicorp-tf-api-token", `[a-zA-Z0-9]{14}\.atlasv1\.[a-zA-Z0-9\-_=]{60,70}`),
		grp("pulumi-api-token", `\b(pul-[a-f0-9]{40})`+tail),
		grp("postman-api-token", `\b(PMAK-[a-fA-F0-9]{24}-[a-fA-F0-9]{34})`+tail),

		grp("grafana-api-key", `\b(eyJrIjoi[A-Za-z0-9+/]{70,400}={0,3})`+tail),
		grp("grafana-cloud-api-token", `\b(glc_[A-Za-z0-9+/]{32,400}={0,3})`+tail),
		grp("grafana-service-account-token", `\b(glsa_[A-Za-z0-9]{32}_[A-Fa-f0-9]{8})`+tail),

		grp("sentry-user-token", `\b(sntryu_[a-f0-9]{64})`+tail),
		whole("sentry-org-token", `\bsntrys_eyJpYXQiO[a-zA-Z0-9+/]{10,200}(?:LCJyZWdpb25fdXJs|InJlZ2lvbl91cmwi|cmVnaW9uX3VybCI6)[a-zA-Z0-9+/]{10,200}={0,2}_[a-zA-Z0-9+/]{43}`),

		grp("stripe-access-token", `\b((?:sk|rk)_(?:test|live|prod)_[a-zA-Z0-9]{10,99})`+tail),
		whole("shopify-access-token", `shpat_[a-fA-F0-9]{32}`),
		whole("shopify-shared-secret", `shpss_[a-fA-F0-9]{32}`),
	}
}

// applyPlaceholder: within the full match text, replace the
// captured secret `group` with the Placeholder, preserving a wrapping quote if
// the secret itself was quoted on both ends.
func applyPlaceholder(full, group string) string {
	q := ""
	if len(group) >= 2 && (group[0] == '"' || group[0] == '\'') && group[len(group)-1] == group[0] {
		q = string(group[0])
	}
	i := strings.LastIndex(full, group)
	if i < 0 {
		return Placeholder
	}
	return full[:i] + q + Placeholder + q + full[i+len(group):]
}

// nextRune returns the first rune of s, or -1 if empty.
func nextRune(s string) rune {
	for _, r := range s {
		return r
	}
	return -1
}

// applyRule redacts every non-overlapping match of r in s, honouring
// denyTrailing, in a single replace-all pass.
func applyRule(s string, r rule) string {
	locs := r.re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		start, end := m[0], m[1]
		if start < last {
			continue // overlaps a previous replacement
		}
		if r.denyTrailing != nil && end < len(s) {
			if rn := nextRune(s[end:]); rn >= 0 && r.denyTrailing.MatchString(string(rn)) {
				continue // emulated trailing (?!class): reject
			}
		}
		full := s[start:end]
		var repl string
		if r.hasGroup && len(m) >= 4 && m[2] >= 0 {
			repl = applyPlaceholder(full, s[m[2]:m[3]])
		} else {
			repl = Placeholder
		}
		b.WriteString(s[last:start])
		b.WriteString(repl)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// RedactTokens applies every high-confidence provider token rule in order,
// replacing each detected secret with Placeholder.
func RedactTokens(s string) string {
	for _, r := range highConfidenceRules {
		s = applyRule(s, r)
	}
	return s
}

// ---------------------------------------------------------------------------
// Private-key block redaction
// ---------------------------------------------------------------------------

var (
	beginPrivateKey = regexp.MustCompile(`(?i)-----BEGIN[ A-Z0-9_-]{0,100}?PRIVATE KEY(?: BLOCK)?-----`)
	endPrivateKey   = regexp.MustCompile(`(?i)-----END[ A-Z0-9_-]{0,100}?PRIVATE KEY(?: BLOCK)?-----`)
)

const privateKeyMinGap = 64

// privateKeySpans finds [start,end) byte spans covering BEGIN..END PRIVATE KEY
// blocks, requiring at least privateKeyMinGap bytes between the BEGIN marker's
// end and the END marker.
func privateKeySpans(s string) [][2]int {
	var spans [][2]int
	pos := 0
	for pos < len(s) {
		bl := beginPrivateKey.FindStringIndex(s[pos:])
		if bl == nil {
			break
		}
		bStart, bEnd := pos+bl[0], pos+bl[1]
		searchFrom := bEnd + privateKeyMinGap
		if searchFrom > len(s) {
			break
		}
		el := endPrivateKey.FindStringIndex(s[searchFrom:])
		if el == nil {
			break
		}
		eEnd := searchFrom + el[1]
		spans = append(spans, [2]int{bStart, eEnd})
		pos = eEnd
	}
	return spans
}

// RedactPrivateKeys replaces each PEM private-key block with Placeholder.
func RedactPrivateKeys(s string) string {
	spans := privateKeySpans(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp[0]])
		b.WriteString(Placeholder)
		last = sp[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Redact is the primary entry point: it strips PEM private-key blocks and then
// redacts every high-confidence provider token. The low-confidence URL/userinfo
// context rules are handled by the free-text scanner (see freetext.go).
func Redact(s string) string {
	return RedactTokens(RedactPrivateKeys(s))
}
