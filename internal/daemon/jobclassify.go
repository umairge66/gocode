package daemon

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// -----------------------------------------------------------------------------
// The background-session state classifier: turns the tail of the assistant's
// last message into a job state (working / blocked / done / failed), the
// detail line and, for a blocked job, what it needs from the user.
//
// Classification is deterministic: a pre-classifier first, then a heuristic
// last-line fallback. No model call is made for classification.
//
// Positions are byte offsets; every position is computed and consumed in the
// same unit.

// Classification is one classifier verdict.
type Classification struct {
	Branch string
	State  string
	Tempo  string
	Detail string
	Needs  string
	// Result is output.result ("" when the verdict carries no result).
	Result string
}

// openFence returns the fence marker ('`' or '~') still open before byte n of
// s, or 0 when position n is outside any fenced code block.
func openFence(s string, n int) (byte, int) {
	var open byte
	openLen := 0
	pos := 0
	a := strings.Index(s, "```")
	l := strings.Index(s, "~~~")
	for pos < n {
		if a != -1 && a < pos {
			a = indexFrom(s, "```", pos)
		}
		if l != -1 && l < pos {
			l = indexFrom(s, "~~~", pos)
		}
		u := a
		if a == -1 || (l != -1 && l < a) {
			u = l
		}
		if u == -1 || u >= n {
			break
		}
		r := s[u]
		c, h := u-1, 0
		for c >= 0 && s[c] == ' ' && h < 3 {
			c--
			h++
		}
		lineStart := c < 0 || s[c] == '\n'
		f := 3
		pos = u + 3
		for pos < len(s) && s[pos] == r {
			pos++
			f++
		}
		if !lineStart {
			continue
		}
		if open == 0 {
			open, openLen = r, f
		} else if open == r && f >= openLen {
			open, openLen = 0, 0
		}
	}
	return open, openLen
}

// inFence reports whether byte n of s lies inside a fenced code block.
func inFence(s string, n int) bool {
	m, _ := openFence(s, n)
	return m != 0
}

func indexFrom(s, sub string, from int) int {
	if from > len(s) {
		return -1
	}
	i := strings.Index(s[from:], sub)
	if i < 0 {
		return -1
	}
	return from + i
}

// TruncateDetail caps s at the 800-character detail limit with an ellipsis.
func TruncateDetail(s string) string { return truncateDetail(s) }

// lastRunes returns the suffix of s holding at most n runes, and its offset.
func lastRunes(s string, n int) (string, int) {
	if utf8.RuneCountInString(s) <= n {
		return s, 0
	}
	off := len(s)
	for i := 0; i < n; i++ {
		_, sz := utf8.DecodeLastRuneInString(s[:off])
		off -= sz
	}
	return s[off:], off
}

var (
	// Markers.
	reFailedMarker  = regexp.MustCompile(`(?i)(?:^|\n)\s*failed\s*[:\x{2014}\x{2013}-]\s*([^\n]{3,200})`)
	reNeedsMarker   = regexp.MustCompile(`(?i)(?:^|\n)\s*needs input\s*[:\x{2014}\x{2013}-]\s*([^\n]{3,200})`)
	reBlockedMarker = regexp.MustCompile(`(?i)(?:^|\n)\s*blocked\s*[:\x{2014}\x{2013}-]\s*([^\n]{3,200})`)
	reImBlocked     = regexp.MustCompile(`(?i)\bI'?m blocked\s*[:\x{2014}\x{2013}-]\s*([^\n]{3,200})`)

	reResultLine = regexp.MustCompile(`(?i)(?:^|\n)\s*result:\s*([^\n]+?)\s*(?:\n|$)`)
	reNextLine   = regexp.MustCompile(`(?i)(?:^|\n)\s*next:\s*\S`)
	reParaSplit  = regexp.MustCompile(`\n\s*\n`)
	reNoAction   = regexp.MustCompile(`(?i)\bnothing (?:needed|required) from you\b|\bno(?: user)? action (?:needed|required)\b`)
	reTrailingQ  = regexp.MustCompile(`[?\x{FF1F}]\s*$`)
	reTrailQTrim = regexp.MustCompile(`[?\x{FF1F}\s]+$`)
	reLeadPunct  = regexp.MustCompile(`^[.!?\s]+`)
	reWaitExt    = regexp.MustCompile(`(?i)\b(?:waiting (?:for|on)|pending)\s+(?:the\s+)?(?:CI|build|tests?|reviewer|deploy(?:ment)?|workflow|checks?|rollout|merge queue)\b`)
	reAskVerb    = regexp.MustCompile(`(?i)\b(please (?:run|provide|confirm|clarify|choose|let me know)|let me know (?:which|what|how|when)|which (?:option|approach|one)|should I (?:proceed|continue|use))\b`)
	reAuthProse  = regexp.MustCompile(`(?i)\b(not logged in|please run /login|authentication failed|invalid api key|oauth token (?:expired|revoked)|credit balance (?:is )?too low|usage limit reached|mcp (?:server )?(?:authentication|auth|authorization|unauthorized)|mcp (?:server )?(?:credential|token) (?:missing|expired|invalid)|401 unauthorized|403 forbidden|token (?:has )?expired|bad credentials|gh auth login|gcloud auth login|aws (?:sso )?login)\b`)

	reWantMeTo = regexp.MustCompile(`(?i)\b(?:want|like) me to\b|\b(?:shall|should) I also\b`)
	// Y without its lookaheads: group 1 is "Let me " (not followed by
	// "know"), group 2 the "I'll / I'm going to / Going to " family (not
	// followed by need/require/wait/leave/hold/skip/stop).
	reWorkingVerb   = regexp.MustCompile(`(?i)^(?:(?:Now|Next|Then|Alright|OK|Okay|Right|Good|First|Also),?\s+)?(?:(Let me )|((?:I(?:'?ll| will) |I'?m going to |Going to ))|Proceeding |Moving (?:on|to)\b|Continuing |Starting |Trying |Checking |Looking |Searching |Reading |Investigating |Running |Re-?running |Building |Rebuilding |Installing |Fetching |Applying |Fixing |Patching |Updating |Adding |Removing |Deleting |Importing |Refactoring |Rewriting |Writing |Grepping |Scanning |Wrapping |Switching |Testing |Verifying |Regenerating |Pushing |Pulling |Reviewing |Examining |Loading |Compiling |Parsing |Analyzing |Tracing |Exploring )`)
	reNotKnow       = regexp.MustCompile(`(?i)^know\b`)
	reNotWillVerb   = regexp.MustCompile(`(?i)^(?:need|require|wait|leave|hold|skip|stop)\b`)
	reDeferredVerb  = regexp.MustCompile(`(?i)\b(?:once |when |after |until |as soon as )(?:you|it|the|that|this|they)\b|\bagain in\b|\bcheck back\b|\bin ~?\d+\s*(?:s(?:ec(?:ond)?s?)?|m(?:in(?:ute)?s?)?|h(?:ours?|rs?)?)\b|\bthen\.?\s*$|\bwhichever you\b|\bhold(?:ing)? for your\b|\b(?:to|and) wait for\b|\bgive it (?:more |some )?time\b|\bif (?:you(?:'d| want| prefer| need|'re)?|that(?:'s| helps| works)?|useful|needed|helpful|desired)\b|\b(?:isn'?t|not|won'?t) going to work\b`)
	reAgentsStatus  = regexp.MustCompile(`^(?:(?:\*\*)?[1-9]\d* (?:agent|cron|task|fork|job|worker|PR|check)s? (?:in flight|remaining|active|still (?:running|working)|pending|running|launched)\b|(?:Continuous )?(?:[Ll]oop|[Cc]rons?|[Bb]abysit) (?:active|healthy|continuing|running|will keep|continues)\b|Waiting for (?:the )?(?:agent|cron|task|fork|worker|job|remaining|them)s?\b|Agents? will report back\b|Waiting\.?$)`)
	reCheckBack     = regexp.MustCompile(`(?i)^(?:I will|I'll|Will) (?:check back|re-?check|poll|look again|retry|re-?run|try again) (?:(when |once |after |until )|in\b|again\b)`)
	reNotYour       = regexp.MustCompile(`(?i)^your?\b`)
	reCantProceed   = regexp.MustCompile(`(?i)^I (?:can(?:'?t|not)|am unable to) (?:proceed|continue|make (?:any )?progress|complete|fix this)\b`)
	reGivingUp      = regexp.MustCompile(`(?i)^(?:Giving up|I(?:'m| am) giving up|The task is not actionable)\b`)
	rePushed        = regexp.MustCompile("^(?:Pushed (?:to `|`[0-9a-f]{7,})|Committed as `?[0-9a-f]{7,}\\b|Commit: `?[0-9a-f]{7,}\\b|(?:Opened|Created) PR #?\\d)")
	reReadyFor      = regexp.MustCompile(`^Ready (?:for review|to (?:upload|merge|ship|land))\b`)
	reVerdict       = regexp.MustCompile(`^VERDICT: (?:PASS|FAIL)\b`)
	rePleaseDo      = regexp.MustCompile("^Please (?:start|run|provide|grant|export|add|install|configure|give me|paste|point me|set (?:the |up |`?[A-Z][A-Z0-9_]+\\b))")
	reStoppingHere  = regexp.MustCompile(`(?i)^(?:Stopping here|I've stopped here|Parked (?:the|this) branch|Paused here)(?:\.|$| \x{2014}| -| until| pending| since| because)`)
	reApprovalCore  = regexp.MustCompile(`(?i)\b(?:awaiting|waiting (?:for|on)|pending|ready for)\s+your\s+(?:approval|confirmation|go[- ]ahead|ok(?:ay)?\b|sign[- ]?off|green light)`)
	reAwaitingCore  = regexp.MustCompile(`(?i)\b(?:awaiting|waiting (?:for|on)|pending|ready for)\s+your\s+(?:approval|confirmation|go[- ]ahead|ok(?:ay)?\b|sign[- ]?off|green light)|\b(?:awaiting|waiting (?:for|on)|pending)\s+(?:your\s+(?:feedback|input|decision|response|direction|guidance)|you\b|the user\b)`)
	reNegatedBefore = regexp.MustCompile(`(?i)(?:\b(?:not|no longer|nothing(?: is|['\x{2019}]s)?|without|instead of|rather than|was|were|had been)\s+|n['\x{2019}]t\s+)$`)

	reBeforeLead   = regexp.MustCompile(`(?i)^\s*(?:result:\s*)?(?:\*\*|\()?(?:(?:I['\x{2019}]?m|I am|still|now|currently|just)\s+)*$`)
	reBeforeClause = regexp.MustCompile(`(?i)(?:[:;,\x{2014}\x{2013}]|\s-|\s\()\s*(?:\*\*|\()?(?:(?:I['\x{2019}]?m|I am|still|now|currently|just)\s+)*$`)
	reAfterEmpty   = regexp.MustCompile(`^\s*(?:\*\*|\))?\s*$`)
	reReadyWord    = regexp.MustCompile(`(?i)^ready\b`)
	reAsBefore     = regexp.MustCompile(`(?i)\bas\s+$`)
	reSentenceEnd  = regexp.MustCompile(`[.!?](?:\*\*|\))?\s+|\n`)
	reClauseSplit  = regexp.MustCompile(`(?i)[,;:\x{2014}\x{2013}(]|\s-\s|\b(?:and|but|so|while)\b`)
	reListLine     = regexp.MustCompile("^(?:```|~~~|[-+>|@]|[*•]\\s|\\d+[.)]\\s|#{1,6}\\s)|:(?:\\*\\*)?$")
	reNegation     = regexp.MustCompile(`(?i)\b(?:no|not|nothing|none|never|nobody|neither|nor)\b|n['\x{2019}]t\b`)
	reOptional     = regexp.MustCompile(`(?i)\bif you(?:['\x{2019}]d like| want| prefer| like)\b|\bI can also\b|\bhappy to\b|\boptionally\b`)
	reConditional  = regexp.MustCompile(`(?i)\b(?:once|when|after|until|as soon as)\s+(?:it|the|that|this|they)\b|\b(?:will|['\x{2019}]ll)\s+be\s+(?:ready|pending)\b`)
	reDeferSimple  = regexp.MustCompile(`(?i)\bnext check\b|\bagain in\b|\bre-?poll|\bpoll(?:ing)? again\b|\bshepherding\b`)
	reDeferReport  = regexp.MustCompile(`(?i)\b(?:check(?:ing)? (?:back|again)|re-?check(?:ing)?|report(?:ing)?(?: back)?) (?:in|when|once|after)\b`)
	reSentenceStop = regexp.MustCompile(`[.!?](?:\*\*|\)|["'\x{2019}\x{201D}])*`)
)

// notQuotedOrNegated is a lookbehind check: the phrase at idx must
// not follow a quote character, nor a negation ("not", "no longer", "was",
// "n't", …).
func notQuotedOrNegated(s string, idx int) bool {
	if idx > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:idx])
		switch r {
		case '"', '\'', '“', '‘', '`':
			return false
		}
	}
	return !reNegatedBefore.MatchString(s[:idx])
}

// guardedMatches returns the matches of re in s whose start passes the P
// lookbehind.
func guardedMatches(re *regexp.Regexp, s string) [][]int {
	var out [][]int
	for _, m := range re.FindAllStringIndex(s, -1) {
		if notQuotedOrNegated(s, m[0]) {
			out = append(out, m)
		}
	}
	return out
}

// marker is one state-marker hit.
type marker struct {
	state   string
	capture string
	index   int
	end     int
}

// scanMarkers finds the last failed/needs-input/blocked marker in seg (which
// starts at byte off of full) outside a code fence.
func scanMarkers(full, seg string, off int) *marker {
	var best *marker
	for _, p := range []struct {
		state string
		re    *regexp.Regexp
	}{{"failed", reFailedMarker}, {"blocked", reNeedsMarker}, {"blocked", reBlockedMarker}, {"blocked", reImBlocked}} {
		for _, m := range p.re.FindAllStringSubmatchIndex(seg, -1) {
			// The capture runs to the end of its line.
			if m[1] != len(seg) && seg[m[1]] != '\n' {
				continue
			}
			if inFence(full, off+m[0]) {
				continue
			}
			if best == nil || m[0] > best.index {
				best = &marker{state: p.state, capture: strings.TrimSpace(seg[m[2]:m[3]]), index: m[0], end: m[1]}
			}
		}
	}
	return best
}

// askContext is the sentence fragment before a match, the
// previous sentence, the text after it up to the sentence end, and that end.
type askContext struct {
	before, previous, after string
	end                     int
}

func contextOf(e string, mStart, mEnd int) askContext {
	t := e[max(0, mStart-800):mStart]
	ends := reSentenceEnd.FindAllStringIndex(t, -1)
	var c askContext
	if len(ends) > 0 {
		last := ends[len(ends)-1]
		c.before = t[last[1]:]
		prevStart := 0
		if len(ends) > 1 {
			prevStart = ends[len(ends)-2][1]
		}
		c.previous = t[prevStart : last[0]+1]
	} else {
		c.before = t
	}
	rest := e[mEnd:]
	if i, n := sentenceStop(rest); i >= 0 {
		c.after = rest[:i]
		c.end = mEnd + i + n
	} else {
		c.after = rest
		c.end = len(e)
	}
	return c
}

// sentenceStop finds a sentence stop (`[.!?](?:\*\*|\)|["'’”])*(?=\s|$)|\n`)
// in s: its start and length, or -1.
func sentenceStop(s string) (int, int) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			return i, 1
		case '.', '!', '?':
			loc := reSentenceStop.FindStringIndex(s[i:])
			if loc == nil || loc[0] != 0 {
				continue
			}
			end := i + loc[1]
			if end == len(s) || isSpaceByte(s[end]) {
				return i, loc[1]
			}
		}
	}
	return -1, 0
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// askKind classifies a match: "not-an-ask", "defer" or "ask".
func askKind(e string, mStart, mEnd int, c askContext) string {
	parts := reClauseSplit.Split(c.before, -1)
	s := parts[len(parts)-1]
	if reNegation.MatchString(s) || reConditional.MatchString(s+e[mStart:mEnd]) || reOptional.MatchString(c.before) ||
		reWantMeTo.MatchString(c.before) || (strings.HasSuffix(c.previous, "?") && reWantMeTo.MatchString(c.previous)) {
		return "not-an-ask"
	}
	tail := e[mStart:]
	if reDeferSimple.MatchString(tail) {
		return "defer"
	}
	for _, m := range reDeferReport.FindAllStringIndex(tail, -1) {
		if !strings.HasPrefix(tail[m[1]:], " you") {
			return "defer"
		}
	}
	return "ask"
}

// laterProse reports whether real prose (not a list, heading or fenced line)
// follows the paragraph holding byte n.
func laterProse(e string, n int) bool {
	loc := reParaSplit.FindStringIndex(e[n:])
	if loc == nil {
		return false
	}
	i := n + loc[0] + 1
	for _, line := range strings.Split(e[i:], "\n") {
		a := i
		i += len(line) + 1
		l := strings.TrimSpace(line)
		if l != "" && !reListLine.MatchString(l) && !inFence(e, a) {
			return true
		}
	}
	return false
}

// approvalAsk finds a leading "awaiting your approval"-style ask at or after
// byte from: ("ask", needs), ("stale", ""), or ("", "").
func approvalAsk(e string, from int) (string, string) {
	stale := false
	for _, m := range guardedMatches(reApprovalCore, e) {
		if m[0] < from || inFence(e, m[0]) {
			continue
		}
		c := contextOf(e, m[0], m[1])
		ok := reBeforeLead.MatchString(c.before) ||
			(reAfterEmpty.MatchString(c.after) && (reBeforeClause.MatchString(c.before) ||
				(reReadyWord.MatchString(e[m[0]:m[1]]) && !reAsBefore.MatchString(c.before))))
		if !ok {
			continue
		}
		switch askKind(e, m[0], m[1], c) {
		case "not-an-ask":
			continue
		case "defer":
			return "", ""
		}
		if laterProse(e, m[0]) {
			stale = true
			continue
		}
		d := strings.TrimSpace(e[m[0]:c.end])
		if strings.HasSuffix(d, "**") && len(strings.Split(d, "**"))%2 == 0 {
			d = d[:len(d)-2]
		} else if strings.HasSuffix(d, ")") && strings.Count(d, ")") > strings.Count(d, "(") {
			d = d[:len(d)-1]
		}
		return "ask", truncateDetail(d)
	}
	if stale {
		return "stale", ""
	}
	return "", ""
}

// PreClassify is the deterministic pre-classifier over an assistant message.
// It returns nil when nothing matched.
func PreClassify(text string) *Classification {
	n := strings.TrimSpace(text)
	if n == "" {
		return nil
	}
	t, base := lastRunes(n, 800)
	var res []int
	for _, m := range reResultLine.FindAllStringSubmatchIndex(t, -1) {
		if !inFence(n, base+m[0]) {
			res = m
		}
	}
	s, a := t, base
	if res != nil {
		s, a = t[res[1]:], base+res[1]
	}
	l := scanMarkers(n, s, a)
	if res != nil && l == nil {
		o := truncateDetail(t[res[2]:res[3]])
		for _, m := range reNextLine.FindAllStringIndex(s, -1) {
			if !inFence(n, a+m[0]) {
				return &Classification{Branch: "result-then-next", State: "working", Tempo: "idle", Detail: o, Result: o}
			}
		}
		kind, needs := approvalAsk(n, base+res[0])
		switch kind {
		case "stale":
			return nil
		case "ask":
			return &Classification{Branch: "result-approval-ask", State: "blocked", Tempo: "blocked", Needs: needs, Detail: o}
		}
		return &Classification{Branch: "result-marker", State: "done", Tempo: "idle", Detail: o, Result: o}
	}
	if l != nil && l.state == "failed" {
		return &Classification{Branch: "failed-marker", State: "failed", Tempo: "idle", Detail: truncateDetail(l.capture)}
	}
	if l != nil && l.state == "blocked" {
		after := s[l.end:]
		paras := 0
		for _, p := range reParaSplit.Split(after, -1) {
			if strings.TrimSpace(p) != "" {
				paras++
			}
		}
		if paras >= 3 {
			return nil
		}
		if !reNoAction.MatchString(s) {
			g := truncateDetail(l.capture)
			return &Classification{Branch: "blocked-marker", State: "blocked", Tempo: "blocked", Needs: g, Detail: g}
		}
		if res != nil {
			g := truncateDetail(t[res[2]:res[3]])
			return &Classification{Branch: "blocked-disclaimed", State: "done", Tempo: "idle", Detail: g, Result: g}
		}
		return nil
	}
	if reTrailingQ.MatchString(t) && utf8.RuneCountInString(reTrailQTrim.ReplaceAllString(t, "")) >= 4 {
		o := max(strings.LastIndex(t, "\n"), strings.LastIndex(t, ". "), strings.LastIndex(t, "! "), lastIndexBefore(t, "? ", len(t)-2))
		if !inFence(n, base+o) {
			g := truncateDetail(strings.TrimSpace(t[o+1:]))
			if reWantMeTo.MatchString(g) {
				return nil
			}
			return &Classification{Branch: "trailing-q", State: "blocked", Tempo: "blocked", Needs: g, Detail: g}
		}
	}
	u := max(0, strings.LastIndex(t, ". "), strings.LastIndex(t, "! "), strings.LastIndex(t, "? "), strings.LastIndex(t, "\n"))
	r := reLeadPunct.ReplaceAllString(t[u:], "")
	fenced := inFence(n, base+u)
	if fenced {
		kind, needs := approvalAsk(n, base)
		if kind == "ask" {
			return &Classification{Branch: "leading-approval-ask", State: "blocked", Tempo: "blocked", Needs: needs, Detail: needs}
		}
		return nil
	}
	if h := reWaitExt.FindString(r); h != "" {
		return &Classification{Branch: "wait-external", State: "working", Tempo: "idle", Detail: truncateDetail(h)}
	}
	for _, m := range guardedMatches(reAwaitingCore, r) {
		switch askKind(r, m[0], m[1], contextOf(r, m[0], m[1])) {
		case "not-an-ask":
			continue
		case "defer":
			return nil
		}
		k := truncateDetail(strings.TrimSpace(r[m[0]:]))
		return &Classification{Branch: "awaiting-user", State: "blocked", Tempo: "blocked", Needs: k, Detail: k}
	}
	if loc := reAskVerb.FindStringIndex(r); loc != nil {
		o := truncateDetail(strings.TrimSpace(r[loc[0]:]))
		return &Classification{Branch: "ask-verb", State: "blocked", Tempo: "blocked", Needs: o, Detail: o}
	}
	if reAuthProse.MatchString(r) {
		return &Classification{Branch: "auth-prose", State: "blocked", Tempo: "blocked", Needs: truncateDetail(r), Detail: "authentication required"}
	}
	if workingVerb(r) && !reDeferredVerb.MatchString(r) {
		return &Classification{Branch: "working-verb", State: "working", Tempo: "active", Detail: truncateDetail(r)}
	}
	if reAgentsStatus.MatchString(r) {
		return &Classification{Branch: "agents-status", State: "working", Tempo: "idle", Detail: truncateDetail(r)}
	}
	if checkBack(r) {
		return &Classification{Branch: "will-check-back", State: "working", Tempo: "idle", Detail: truncateDetail(r)}
	}
	if reCantProceed.MatchString(r) {
		o := truncateDetail(r)
		return &Classification{Branch: "cant-proceed", State: "blocked", Tempo: "blocked", Detail: o, Needs: o}
	}
	if reGivingUp.MatchString(r) {
		return &Classification{Branch: "giving-up", State: "failed", Tempo: "idle", Detail: truncateDetail(r)}
	}
	if rePushed.MatchString(r) {
		o := truncateDetail(r)
		return &Classification{Branch: "pushed-committed", State: "done", Tempo: "idle", Detail: o, Result: o}
	}
	if reReadyFor.MatchString(r) {
		return &Classification{Branch: "ready-for", State: "done", Tempo: "idle", Detail: truncateDetail(r)}
	}
	if reVerdict.MatchString(r) {
		o := truncateDetail(r)
		return &Classification{Branch: "verdict-marker", State: "done", Tempo: "idle", Detail: o, Result: o}
	}
	if rePleaseDo.MatchString(r) {
		o := truncateDetail(r)
		return &Classification{Branch: "please-do-x", State: "blocked", Tempo: "blocked", Detail: o, Needs: o}
	}
	if reStoppingHere.MatchString(r) {
		o := truncateDetail(r)
		return &Classification{Branch: "stopping-here", State: "blocked", Tempo: "blocked", Detail: o, Needs: o}
	}
	if kind, needs := approvalAsk(n, base); kind == "ask" {
		return &Classification{Branch: "leading-approval-ask", State: "blocked", Tempo: "blocked", Needs: needs, Detail: needs}
	}
	return nil
}

// lastIndexBefore returns the last occurrence of sub starting
// at or before from.
func lastIndexBefore(s, sub string, from int) int {
	if from < 0 {
		return -1
	}
	end := min(len(s), from+len(sub))
	return strings.LastIndex(s[:end], sub)
}

func workingVerb(r string) bool {
	m := reWorkingVerb.FindStringSubmatchIndex(r)
	if m == nil {
		return false
	}
	switch {
	case m[2] >= 0:
		return !reNotKnow.MatchString(r[m[1]:])
	case m[4] >= 0:
		return !reNotWillVerb.MatchString(r[m[1]:])
	}
	return true
}

func checkBack(r string) bool {
	m := reCheckBack.FindStringSubmatchIndex(r)
	if m == nil {
		return false
	}
	if m[2] >= 0 {
		return !reNotYour.MatchString(r[m[1]:])
	}
	return true
}

// HeuristicClassify is the last-resort verdict: still working (idle), with
// the last non-empty line as the detail.
func HeuristicClassify(text string) *Classification {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return &Classification{Branch: "heuristic", State: "working", Tempo: "idle", Detail: truncateDetail(l)}
		}
	}
	return &Classification{Branch: "heuristic", State: "working", Tempo: "idle", Detail: "—"}
}

// Classify runs the deterministic engine: the pre-classifier, then the
// heuristic fallback, normalised so a terminal state is never "active".
func Classify(text string) *Classification {
	c := PreClassify(text)
	if c == nil {
		c = HeuristicClassify(text)
	}
	if IsTerminalState(c.State) {
		c.Tempo = "idle"
	}
	if c.Tempo != "blocked" {
		c.Needs = ""
	}
	return c
}

// APIFailureState maps an API error kind to the state a background session
// shows: ("blocked", needs) for a condition the user can fix, ("failed",
// "API error") otherwise, or ("", "") when the error leaves the state alone.
func APIFailureState(kind, message string) (state, needs string) {
	switch kind {
	case "authentication_failed":
		return "blocked", "login required — run /login"
	case "billing_error":
		return "blocked", "usage limit reached — check plan"
	case "rate_limit":
		return "blocked", "rate limited — wait and retry"
	case "overloaded":
		return "blocked", "API overloaded — wait and retry"
	case "server_error":
		return "blocked", "API unavailable — retry"
	case "invalid_request":
		if reTooLarge.MatchString(message) {
			return "blocked", "request too large — /compact or trim"
		}
		return "blocked", "invalid API request — see detail"
	case "max_output_tokens":
		return "", ""
	case "":
		return "blocked", "API error — see detail"
	}
	return "failed", "API error"
}

var reTooLarge = regexp.MustCompile(`(?i)\b(too long|too large|exceeds|token limit|prompt is too long)\b`)
