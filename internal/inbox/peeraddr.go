package inbox

// Cross-session (other local gocode process) addressing and the
// <cross-session-message> envelope the receiving model sees.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EnvelopeTag is the wrapper tag a cross-session message body is delivered in.
const EnvelopeTag = "cross-session-message"

// AddressScheme is the scheme of a parsed peer address.
type AddressScheme string

const (
	SchemeUDS    AddressScheme = "uds"
	SchemeBridge AddressScheme = "bridge"
	SchemeDID    AddressScheme = "did"
	SchemeOther  AddressScheme = "other"
)

// ParsedAddress is a parsed {scheme,target} address.
type ParsedAddress struct {
	Scheme AddressScheme
	Target string
}

// ParseAddress splits a SendMessage `to` into scheme and target: "uds:" and
// "bridge:" targets are percent-decoded, a bare "/….sock" path or a
// "\\.\pipe\…" name is a uds target as-is.
func ParseAddress(to string) ParsedAddress {
	switch {
	case strings.HasPrefix(to, "uds:"):
		return ParsedAddress{SchemeUDS, decodeAddress(to[4:])}
	case strings.HasPrefix(to, "bridge:"):
		return ParsedAddress{SchemeBridge, decodeAddress(to[7:])}
	case strings.HasPrefix(to, "did:"):
		return ParsedAddress{SchemeDID, to}
	case socketPathRe.MatchString(to) || pipePathRe.MatchString(to):
		return ParsedAddress{SchemeUDS, to}
	}
	return ParsedAddress{SchemeOther, to}
}

// decodeAddress percent-decodes (decodeURIComponent), falling back to the raw
// text when the escape sequence (or the decoded UTF-8) is malformed.
func decodeAddress(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(d) {
		return s
	}
	return d
}

// UDSAddress renders a socket path as its "uds:" address, percent-encoding
// every byte outside [A-Za-z0-9:_/.\-] (uppercase hex).
func UDSAddress(path string) string {
	var b strings.Builder
	b.WriteString("uds:")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			strings.IndexByte(":_/.\\-", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// peerAddressRe matches a well-formed scheme address.
var peerAddressRe = regexp.MustCompile(`^(?:uds|bridge|did):[A-Za-z0-9%:_/.\\\-]{1,200}$`)

// IsPeerAddress reports whether s is a well-formed uds:/bridge:/did: address.
func IsPeerAddress(s string) bool { return peerAddressRe.MatchString(s) }

// localPipeRe is the local pipe-name shape.
var localPipeRe = regexp.MustCompile(`(?i)^[\\/]{2}[.?][\\/]pipe[\\/](?:(LOCAL)[\\/])?([^\\/]+)$`)

// localPipeName returns the bare pipe name of a usable local named-pipe path,
// or "" when it is not one (extra segments, "."/"..", trailing dot/space, or a
// "\\?\" path carrying a forward slash).
func localPipeName(p string) string {
	m := localPipeRe.FindStringSubmatch(p)
	if m == nil || m[2] == "." || m[2] == ".." {
		return ""
	}
	if strings.HasSuffix(m[2], ".") || strings.HasSuffix(m[2], " ") {
		return ""
	}
	if strings.HasPrefix(p, `\\?\`) && strings.Contains(p, "/") {
		return ""
	}
	if m[1] == "" {
		return m[2]
	}
	return `LOCAL\` + m[2]
}

// isLocalIPCPath reports a path that does not start with two slashes, or one
// that is a usable local pipe name. A UNC "//host/…" target is remote.
func isLocalIPCPath(p string) bool {
	if !(len(p) >= 2 && (p[0] == '/' || p[0] == '\\') && (p[1] == '/' || p[1] == '\\')) {
		return true
	}
	return localPipeName(p) != ""
}

// ValidateAddress returns the validation message for a
// `to`, or "" when it is acceptable. listTool names the tool that lists
// reachable sessions.
func ValidateAddress(to, listTool string) string {
	if strings.TrimSpace(to) == "" {
		return "to must not be empty"
	}
	p := ParseAddress(to)
	if (p.Scheme == SchemeBridge || p.Scheme == SchemeUDS) && strings.TrimSpace(p.Target) == "" {
		return "address target must not be empty"
	}
	if !isLocalIPCPath(p.Target) || !isLocalIPCPath(to) {
		return fmt.Sprintf("'%s' is not a local socket address. Use an address from %s.", to, listTool)
	}
	return ""
}

// --- hop chain ----------------------------------------------------------------

// hopTokenLen / maxHopChain: the HMAC is sliced to 24 hex; at most 32 entries.
const (
	hopTokenLen = 24
	maxHopChain = 32
)

// hopKey is the per-process random key of HopToken (32 random bytes).
var hopKey = func() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}()

// HopToken(address) is HMAC-SHA256(per-process key, address)[:24 hex]. A
// session recognises its own tokens in an inbound hop-chain (relay loop guard)
// without revealing its address to the next hop.
func HopToken(address string) string {
	m := hmac.New(sha256.New, hopKey)
	m.Write([]byte(address))
	return hex.EncodeToString(m.Sum(nil))[:hopTokenLen]
}

// appendHop returns the chain carried forward plus our own token, keeping the
// newest 32 entries. A nil previous chain stays nil.
func appendHop(prev []string, own string) []string {
	if prev == nil {
		return nil
	}
	out := append([]string(nil), prev...)
	if own != "" {
		out = append(out, own)
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > maxHopChain {
		out = out[len(out)-maxHopChain:]
	}
	return out
}

// --- envelope -----------------------------------------------------------------

// Envelope is the parsed <cross-session-message> wrapper.
type Envelope struct {
	From        string
	FromSession string
	HopChain    []string
	FromName    string
	FromMode    string // "bypass" | "prompting"
	FromPlugin  string
	Body        string
}

var (
	fromSessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
	hopChainRe    = regexp.MustCompile(`^[0-9a-f]{24}(?:,[0-9a-f]{24}){0,31}$`)
	envelopeRe    = regexp.MustCompile(`^<` + EnvelopeTag +
		`(?: from="([A-Za-z0-9%:_/.\\\-]+)")?` +
		`(?: from-session="([A-Za-z0-9_-]{1,80})")?` +
		`(?: hop-chain="([0-9a-f]{24}(?:,[0-9a-f]{24}){0,31})")?` +
		`(?: from-name="([^"<>\n\r]+)")?` +
		`(?: from-mode="(bypass|prompting)")?` +
		`(?: from-plugin="([^"<>\n\r]+)")?` +
		`>\n([\s\S]*)\n</` + EnvelopeTag + `>$`)
	attrStripRe = regexp.MustCompile(`["<>]`)
)

// cleanLabel drops `"`, `<`, `>` and format/control/surrogate/line
// separators, trim, cap at 64 code points (with an ellipsis).
func cleanLabel(s string) string {
	s = attrStripRe.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf, unicode.Cc, unicode.Cs, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 64 {
		return string(r[:64]) + "\u2026"
	}
	return s
}

// FrameEnvelope returns the delivered text, with the body's closing tags
// neutralized so a peer cannot end the wrapper early and speak outside it.
func FrameEnvelope(e Envelope) string {
	var attrs []string
	if e.From != "" {
		attrs = append(attrs, `from="`+e.From+`"`)
	}
	if e.FromSession != "" && fromSessionRe.MatchString(e.FromSession) {
		attrs = append(attrs, `from-session="`+e.FromSession+`"`)
	}
	if len(e.HopChain) > 0 {
		if l := strings.Join(e.HopChain, ","); hopChainRe.MatchString(l) {
			attrs = append(attrs, `hop-chain="`+l+`"`)
		}
	}
	if n := cleanLabel(e.FromName); e.FromName != "" && n != "" {
		attrs = append(attrs, `from-name="`+n+`"`)
	}
	if e.FromMode != "" {
		attrs = append(attrs, `from-mode="`+e.FromMode+`"`)
	}
	if p := cleanLabel(e.FromPlugin); e.FromPlugin != "" && p != "" {
		attrs = append(attrs, `from-plugin="`+p+`"`)
	}
	head := ""
	if len(attrs) > 0 {
		head = " " + strings.Join(attrs, " ")
	}
	return "<" + EnvelopeTag + head + ">\n" + NeutralizeClosingTag(EnvelopeTag, e.Body) + "\n</" + EnvelopeTag + ">"
}

// ParseEnvelope accepts text only when it is exactly a canonical
// envelope (re-framing the parsed fields must reproduce it byte for byte).
func ParseEnvelope(s string) (Envelope, bool) {
	m := envelopeRe.FindStringSubmatch(s)
	if m == nil {
		return Envelope{}, false
	}
	e := Envelope{From: m[1], FromSession: m[2], FromName: m[4], FromMode: m[5], FromPlugin: m[6], Body: m[7]}
	if m[3] != "" {
		e.HopChain = strings.Split(m[3], ",")
	}
	if FrameEnvelope(e) != s {
		return Envelope{}, false
	}
	return e, true
}

// --- closing-tag neutralization ------------------------------------------------

// openerChars / slashChars are `<` and `/` plus their lookalikes.
const (
	openerChars = "<\uFF1C\uFE64\u2329\u27E8\u3008\u2039\u02C2\u1438\u276C\u276E\u2770\u29FC\u226E\u227A\u22D6"
	closerChars = ">\uFF1E\uFE65\u232A\u27E9\u3009\u203A\u02C3\u1433\u276D\u276F\u2771\u29FD\u226F\u227B\u22D7"
	slashChars  = "/\uFF0F\u2215\u2044"
)

// isTagWordRune reports [A-Za-z0-9_-].
func isTagWordRune(r rune) bool {
	return r < 128 && (r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
}

// isFiller is the filler class: anything but a tag-word rune or an
// opener/closer (lookalike) — whitespace, punctuation, invisibles.
func isFiller(r rune) bool {
	return !isTagWordRune(r) && !strings.ContainsRune(openerChars, r) && !strings.ContainsRune(closerChars, r)
}

// isInvisible approximates the invisible class: format/control characters,
// combining marks, variation selectors and line/paragraph separators that may
// sit between the tag's letters without being seen.
func isInvisible(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Cc, unicode.Mn, unicode.Me, unicode.Zl, unicode.Zp, unicode.Variation_Selector) ||
		r == '\u115f' || r == '\u1160' || r == '\u3164' || r == '\uffa0' || r == '\u034f'
}

// isDashLike reports underscore, connector punctuation, dashes and their
// lookalikes, standing for a "-" or "_" in the tag name.
func isDashLike(r rune) bool {
	return r == '_' || unicode.In(r, unicode.Pc, unicode.Pd) || strings.ContainsRune("\u2017\u02cd\u07fa\u0640\u2212\u207b\u208b\u02d7\u2796\u2043\u30fc\uff70", r)
}

// NeutralizeClosingTag(tag, s): every opener (or lookalike) that starts
// a closing `</tag` — fillers allowed around the slash, invisibles between the
// letters, "-"/"_" spelt with any dash-like rune, any letter case — is replaced
// by `<\`, so the body cannot terminate its wrapper.
func NeutralizeClosingTag(tag, s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if strings.ContainsRune(openerChars, rs[i]) && closingTagAt(rs, i+1, tag) {
			b.WriteString(`<\`)
			continue
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

func closingTagAt(rs []rune, j int, tag string) bool {
	if j < len(rs) && rs[j] == '\\' {
		return false
	}
	for j < len(rs) && isFiller(rs[j]) && !strings.ContainsRune(slashChars, rs[j]) {
		j++
	}
	if j >= len(rs) || !strings.ContainsRune(slashChars, rs[j]) {
		return false
	}
	j++
	for j < len(rs) && isFiller(rs[j]) {
		j++
	}
	for k, c := range tag {
		if k > 0 {
			for j < len(rs) && isInvisible(rs[j]) {
				j++
			}
		}
		if j >= len(rs) {
			return false
		}
		r := rs[j]
		if c == '-' || c == '_' {
			if !isDashLike(r) {
				return false
			}
		} else if unicode.ToLower(r) != c {
			return false
		}
		j++
	}
	return j >= len(rs) || !isTagWordRune(rs[j])
}
