package plugineval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GraderResult is one grader's verdict on one run.
type GraderResult struct {
	Name        string  `json:"name"`
	Passed      bool    `json:"passed"`
	Weight      float64 `json:"weight"`
	Explanation string  `json:"explanation"`
	WithOnly    bool    `json:"with_only,omitempty"`
	Scored      bool    `json:"scored"`
	JudgeVotes  []bool  `json:"judge_votes,omitempty"`
	Evidence    string  `json:"evidence,omitempty"`
}

// JudgeRequest is one LLM-judge call. Image, when set, is the file the judge
// must look at (its text prompt says so).
type JudgeRequest struct {
	System string
	Prompt string
	// Model is the --judge-model override ("" = the judge's default).
	Model string
	Image *JudgeImage
	// Parts is the prompt split around the image: Parts[0] before, Parts[1]
	// after. Used only with Image.
	Parts [2]string
}

// JudgeImage is an image shown to the judge.
type JudgeImage struct {
	MediaType string
	Data      []byte
}

// Judge runs one judge call through the user's own configured provider and
// returns the reply text and its cost.
type Judge func(ctx context.Context, req JudgeRequest) (text string, costUSD float64, err error)

// ErrJudgeImageRejected marks an image the judge route refused (a Judge
// returns it, possibly wrapped, when it cannot show the image).
var ErrJudgeImageRejected = errors.New("attached media rejected by the API")

const (
	judgeVotes    = 3
	judgeTextCap  = 100000
	judgeTextTail = 20000
	longFileChars = 8000
	// maxFocusBytes caps a focus file.
	maxFocusBytes = 10 << 20
	// judgeSystem is the judge's system prompt.
	judgeSystem = "You are a strict, terse evaluation judge for coding-agent traces."
	judgeAnswer = "Respond with exactly one word: PASS or FAIL."
)

// gradeInput is everything a grader may look at for one run.
type gradeInput struct {
	c          *Case
	run        *RunTrace
	cwdDiff    string
	sandboxCwd string
	arm        string // "", "with", "without"
	judgeModel string
	skipPaid   bool
	judge      Judge
}

// isWithOnly: an explicit arm, or the implicit with-only
// `tool_used: Skill`.
func isWithOnly(g Grader) bool {
	return g.Arm == "with-only" || (g.Arm == "" && g.Type == "tool_used" && g.Tool == "Skill")
}

// gradeRun: every grader runs (the without arm drops the
// with-only ones unless every grader is with-only); under ablation a
// with-only grader is reported but not scored.
func gradeRun(ctx context.Context, in gradeInput) (results []GraderResult, judgeCost float64, paidSkipped bool) {
	allWithOnly := true
	for _, g := range in.c.Graders {
		if !isWithOnly(g) {
			allWithOnly = false
		}
	}
	gs := in.c.Graders
	if in.arm == "without" && !allWithOnly {
		gs = nil
		for _, g := range in.c.Graders {
			if !isWithOnly(g) {
				gs = append(gs, g)
			}
		}
	}
	for _, g := range gs {
		if in.skipPaid && (g.Type == "llm" || g.Type == "baseline") {
			paidSkipped = true
		}
	}
	for _, g := range gs {
		r, cost := runGrader(ctx, g, in)
		judgeCost += cost
		if in.arm != "" && !allWithOnly && isWithOnly(g) {
			r.WithOnly = true
		}
		r.Scored = !r.WithOnly
		results = append(results, r)
	}
	return results, judgeCost, paidSkipped
}

// runGrader: a grader that throws fails with "grader threw: ...".
func runGrader(ctx context.Context, g Grader, in gradeInput) (r GraderResult, cost float64) {
	defer func() {
		if p := recover(); p != nil {
			r = failed(g, fmt.Sprintf("grader threw: %v", p))
		}
	}()
	var err error
	switch g.Type {
	case "regex":
		r, err = gradeRegex(g, in)
	case "tool_order":
		r, err = gradeToolOrder(g, in)
	case "tool_used":
		r, err = gradeToolUsed(g, in)
	case "file_exists":
		r, err = gradeFileExists(g, in)
	case "llm", "baseline":
		if in.skipPaid {
			return failed(g, "skipped: cost ceiling"), 0
		}
		if g.Type == "llm" {
			r, cost, err = gradeLLM(ctx, g, in)
		} else {
			r, cost, err = gradeBaseline(ctx, g, in)
		}
	default:
		err = fmt.Errorf("unknown grader type %q", g.Type)
	}
	if err != nil {
		return failed(g, "grader threw: "+err.Error()), cost
	}
	return r, cost
}

func failed(g Grader, why string) GraderResult {
	return GraderResult{Name: g.Name, Passed: false, Weight: g.Weight, Explanation: why}
}

// focus is the result of readFocus.
type focus struct {
	kind       string // text | image | unavailable
	text       string
	binaryHead string
	reason     string
	path       string
	mediaType  string
	bytes      []byte
}

// readFocus: the text (or image) a regex/llm grader looks at.
func readFocus(t Target, in gradeInput) (focus, error) {
	switch t.Kind {
	case "file":
		b, err := readFocusFile(in.sandboxCwd, t.Path)
		if err != nil {
			return focus{}, err
		}
		if mt := imageMediaType(b); mt != "" {
			return focus{kind: "image", path: t.Path, mediaType: mt, bytes: b}, nil
		}
		f := focus{kind: "text", text: strings.ToValidUTF8(string(b), "�")}
		if i := bytes.IndexByte(b, 0); i != -1 {
			f.binaryHead = fmt.Sprintf("it contains a NUL byte at offset %d (binary data, UTF-16 text — save such artifacts as UTF-8 — or NUL-separated output; leading bytes: %s)", i, hexHead(b))
		}
		return f, nil
	case "last_message":
		return focus{kind: "text", text: in.run.LastAssistantText}, nil
	case "files":
		return focus{kind: "text", text: in.cwdDiff}, nil
	case "mock_calls":
		return focus{kind: "unavailable", reason: "no mock stand-ins were active in this run (--mocks off, or no mocks/ directory applies to this case) — a mock_calls grader has nothing to check"}, nil
	}
	var lines []string
	for _, r := range in.run.Records {
		lines = append(lines, stringify(r))
	}
	return focus{kind: "text", text: strings.Join(lines, "\n")}, nil
}

func hexHead(b []byte) string {
	n := len(b)
	if n > 16 {
		n = 16
	}
	return fmt.Sprintf("% x", b[:n])
}

// imageMediaType recognizes the image formats the judge accepts.
func imageMediaType(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// readFocusFile: a regular, single-link file inside the run directory,
// at most 10 MB.
func readFocusFile(cwd, rel string) ([]byte, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, filepath.FromSlash(rel))
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("focus file %s leaves the run directory through a link", rel)
	}
	st, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("focus file %s does not exist", rel)
		}
		return nil, fmt.Errorf("focus file %s is unreadable (%v)", rel, err)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !within(real, realCwd) {
		return nil, fmt.Errorf("focus file %s leaves the run directory through a link", rel)
	}
	st, err = os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("focus file %s is not a regular file", rel)
	}
	if hardLinks(st) > 1 {
		return nil, fmt.Errorf("focus file %s has more than one hard link, so the grader cannot tell whether it also lives outside the run directory — not read (point the focus at a file with a single link, e.g. a copy)", rel)
	}
	if st.Size() > maxFocusBytes {
		return nil, fmt.Errorf("focus file %s exceeds 10 MB", rel)
	}
	return os.ReadFile(real)
}

func gradeRegex(g Grader, in gradeInput) (GraderResult, error) {
	f, err := readFocus(g.Target, in)
	if err != nil {
		return GraderResult{}, err
	}
	what := g.Target.String()
	switch f.kind {
	case "unavailable":
		return failed(g, f.reason), nil
	case "image":
		hint := fmt.Sprintf("grade what is visible with an llm grader (focus: {source: file, path: %s})", f.path)
		if g.Match == "not_contains" || g.Match == "count:0" {
			hint = "to assert something is absent, have the case also write a text rendering and run this grader over that"
		}
		return failed(g, fmt.Sprintf("%s is an image (%s); a regex grader needs text — %s", what, f.mediaType, hint)), nil
	}
	re, err := jsRegexp(g.Pattern, g.Flags)
	if err != nil {
		return GraderResult{}, err
	}
	var passed bool
	var why string
	switch {
	case g.Match == "contains":
		ok, err := jsTest(re, f.text)
		if err != nil {
			return GraderResult{}, err
		}
		passed = ok
		why = "pattern not found in " + what
		if ok {
			why = "matched " + g.Pattern
		}
	case g.Match == "not_contains":
		ok, err := jsTest(re, f.text)
		if err != nil {
			return GraderResult{}, err
		}
		passed = !ok
		why = "pattern found (expected absent)"
		if passed {
			why = "pattern absent as expected"
		}
	case strings.HasPrefix(g.Match, "count:"):
		var want int
		fmt.Sscanf(g.Match[6:], "%d", &want)
		n, err := jsCountMatches(re, f.text)
		if err != nil {
			return GraderResult{}, err
		}
		passed = n == want
		why = fmt.Sprintf("found %d matches (expected %d)", n, want)
	default:
		why = fmt.Sprintf("unknown match mode %q (use contains | not_contains | count:N)", g.Match)
	}
	return GraderResult{Name: g.Name, Passed: passed, Weight: g.Weight, Explanation: why}, nil
}

func callMatches(c ToolCall, ref ToolRef) (bool, error) {
	if c.Name != ref.Tool {
		return false, nil
	}
	if ref.InputMatch == "" {
		return true, nil
	}
	re, err := jsDefaultRegexp(ref.InputMatch)
	if err != nil {
		return false, err
	}
	return jsTest(re, c.InputText)
}

func firstCall(calls []ToolCall, ref ToolRef) (int, error) {
	for i, c := range calls {
		ok, err := callMatches(c, ref)
		if err != nil {
			return -1, err
		}
		if ok {
			return i, nil
		}
	}
	return -1, nil
}

func gradeToolOrder(g Grader, in gradeInput) (GraderResult, error) {
	b, err := firstCall(in.run.ToolCalls, g.Before)
	if err != nil {
		return GraderResult{}, err
	}
	a, err := firstCall(in.run.ToolCalls, g.After)
	if err != nil {
		return GraderResult{}, err
	}
	if b == -1 {
		return failed(g, fmt.Sprintf("%q tool %s never called", "before", g.Before.Tool)), nil
	}
	if a == -1 {
		return failed(g, fmt.Sprintf("%q tool %s never called", "after", g.After.Tool)), nil
	}
	ok := b < a
	why := fmt.Sprintf("%s@%d does NOT precede %s@%d", g.Before.Tool, b, g.After.Tool, a)
	if ok {
		why = fmt.Sprintf("%s@%d precedes %s@%d", g.Before.Tool, b, g.After.Tool, a)
	}
	return GraderResult{Name: g.Name, Passed: ok, Weight: g.Weight, Explanation: why}, nil
}

func gradeToolUsed(g Grader, in gradeInput) (GraderResult, error) {
	n := 0
	for _, c := range in.run.ToolCalls {
		ok, err := callMatches(c, ToolRef{Tool: g.Tool, InputMatch: g.InputMatch})
		if err != nil {
			return GraderResult{}, err
		}
		if ok {
			n++
		}
	}
	lo := 1
	if g.Min != nil {
		lo = *g.Min
	}
	hi, hiText := -1, "∞"
	if g.Max != nil {
		hi, hiText = *g.Max, fmt.Sprint(*g.Max)
	}
	ok := n >= lo && (hi < 0 || n <= hi)
	return GraderResult{Name: g.Name, Passed: ok, Weight: g.Weight, Explanation: fmt.Sprintf("%s called %dx (expected %d..%s)", g.Tool, n, lo, hiText)}, nil
}

// gradeFileExists: matched against the files the run created.
func gradeFileExists(g Grader, in gradeInput) (GraderResult, error) {
	re, err := globToPathRegexp(g.Path)
	if err != nil {
		return GraderResult{}, err
	}
	found := false
	for _, l := range strings.Split(in.cwdDiff, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if ok, _ := re.MatchString(l); ok {
			found = true
			break
		}
	}
	ok := found == g.Exists
	var why string
	switch {
	case ok && g.Exists:
		why = g.Path + " exists as expected"
	case ok:
		why = g.Path + " absent as expected"
	default:
		state, want := "missing", "present"
		if found {
			state = "exists"
		}
		if !g.Exists {
			want = "absent"
		}
		why = fmt.Sprintf("%s %s (expected %s)", g.Path, state, want)
	}
	return GraderResult{Name: g.Name, Passed: ok, Weight: g.Weight, Explanation: why}, nil
}

// elideMiddle: keep the head and the last `tail` characters.
func elideMiddle(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	return string(r[:head]) + fmt.Sprintf("\n[…%d chars elided…]\n", len(r)-head-tail) + string(r[len(r)-tail:])
}

func gradeLLM(ctx context.Context, g Grader, in gradeInput) (GraderResult, float64, error) {
	var f focus
	var err error
	if g.Focus.Kind == "trace" {
		f = focus{kind: "text", text: elideTrace(in.run.Records)}
	} else if f, err = readFocus(g.Focus, in); err != nil {
		return GraderResult{}, 0, err
	}
	if f.kind == "unavailable" {
		return failed(g, f.reason), 0, nil
	}
	head := "You are grading the output of a coding agent against a criterion.\n\nCriterion:\n" + g.Criteria
	what := g.Focus.String()
	req := JudgeRequest{System: judgeSystem, Model: in.judgeModel}
	var evidence, note string
	if f.kind == "text" && f.binaryHead != "" {
		return failed(g, fmt.Sprintf("%s cannot be shown to the judge as text — %s. It is not a supported image either (PNG/JPEG/GIF/WebP), so have the case render it to an image or write its content as a UTF-8 text file, and grade that.", what, f.binaryHead)), 0, nil
	}
	if f.kind == "image" {
		req.Image = &JudgeImage{MediaType: f.mediaType, Data: f.bytes}
		req.Parts = [2]string{head + "\n\nAgent output (" + what + ") is the attached image:", judgeAnswer}
		req.Prompt = req.Parts[0] + "\n\n" + req.Parts[1]
		evidence = fmt.Sprintf("[image shown to the judge: %s — %s, %d bytes]", f.path, f.mediaType, len(f.bytes))
		note = " (image)"
	} else {
		text := f.text
		switch {
		case text == "" && g.Focus.Kind == "files":
			text = "(no file changes)"
		case text == "" && g.Focus.Kind == "mock_calls":
			text = "(no mocked tool calls)"
		}
		evidence = elideMiddle(text, judgeTextCap-judgeTextTail, judgeTextTail)
		req.Prompt = head + "\n\n\nAgent output (" + what + "):\n" + evidence + "\n\n\n" + judgeAnswer
		if g.Focus.Kind == "file" && len([]rune(text)) > longFileChars {
			note = fmt.Sprintf(" — note: long file (%d chars); llm judges are noisy on long inputs, prefer a regex grader for large artifacts", len([]rune(text)))
		}
	}
	votes, cost, err := judgeVotesFor(ctx, in.judge, req)
	if err != nil {
		if f.kind == "image" && errors.Is(err, ErrJudgeImageRejected) {
			return failed(g, fmt.Sprintf("focus file %s was rejected by the API as an image (%v) — the artifact may be truncated or corrupt, or the judge route does not accept images", f.path, err)), cost, nil
		}
		return GraderResult{}, cost, err
	}
	return GraderResult{Name: g.Name, Passed: majority(votes), Weight: g.Weight, Explanation: "judge votes: " + voteText(votes) + note, JudgeVotes: votes, Evidence: evidence}, cost, nil
}

func gradeBaseline(ctx context.Context, g Grader, in gradeInput) (GraderResult, float64, error) {
	p := g.BaselineFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(in.c.CaseDir, filepath.FromSlash(p))
	}
	text, err := readCapped(p, maxFocusBytes, "baseline file")
	if err != nil {
		return failed(g, "failed to read baseline: "+err.Error()), 0, nil
	}
	if text == nil {
		return failed(g, fmt.Sprintf("baseline file %s does not exist", g.BaselineFile)), 0, nil
	}
	var base []map[string]any
	for _, l := range strings.Split(*text, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		rec, err := decodeRecord([]byte(l))
		if err != nil {
			return failed(g, "failed to read baseline: "+err.Error()), 0, nil
		}
		base = append(base, rec)
	}
	prompt := strings.Join([]string{
		"You are comparing a NEW coding-agent trajectory against a BASELINE.",
		"Criterion:\n" + g.Criteria,
		"\nBASELINE trajectory:\n" + elideTrace(base),
		"\nNEW trajectory:\n" + elideTrace(in.run.Records),
		"\nDoes the NEW trajectory satisfy the criterion at least as well as the BASELINE?",
		judgeAnswer,
	}, "\n\n")
	votes, cost, err := judgeVotesFor(ctx, in.judge, JudgeRequest{System: judgeSystem, Prompt: prompt, Model: in.judgeModel})
	if err != nil {
		return GraderResult{}, cost, err
	}
	return GraderResult{Name: g.Name, Passed: majority(votes), Weight: g.Weight, Explanation: "judge votes: " + voteText(votes), JudgeVotes: votes}, cost, nil
}

var (
	passRe = regexp.MustCompile(`(?i)\bPASS\b`)
	failRe = regexp.MustCompile(`(?i)\bFAIL\b`)
)

// judgeVotesFor: three independent judge calls; a vote passes when the
// reply says PASS and not FAIL.
func judgeVotesFor(ctx context.Context, judge Judge, req JudgeRequest) ([]bool, float64, error) {
	if judge == nil {
		return nil, 0, errors.New("judge call failed: no model is configured for the LLM graders")
	}
	var votes []bool
	total := 0.0
	for i := 0; i < judgeVotes; i++ {
		text, cost, err := judge(ctx, req)
		total += cost
		if err != nil {
			return nil, total, err
		}
		votes = append(votes, passRe.MatchString(text) && !failRe.MatchString(text))
	}
	return votes, total, nil
}

func majority(votes []bool) bool {
	n := 0
	for _, v := range votes {
		if v {
			n++
		}
	}
	return float64(n) > float64(len(votes))/2
}

func voteText(votes []bool) string {
	parts := make([]string, len(votes))
	for i, v := range votes {
		parts[i] = "FAIL"
		if v {
			parts[i] = "PASS"
		}
	}
	return strings.Join(parts, " ")
}
