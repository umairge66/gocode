package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

type line map[string]any

func textMsg(role, text string) map[string]any {
	return map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}}
}

func toolUseMsg(name string, input map[string]any) map[string]any {
	return map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": "tu_" + name, "name": name, "input": input},
	}}
}

func toolResultMsg(content string, isErr bool) map[string]any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu", "content": content, "is_error": isErr},
	}}
}

// writeSession writes a sessionstore-format transcript; msgs are placed one
// per `step` starting at start.
func writeSession(t *testing.T, home, cwd, id string, start time.Time, step time.Duration, msgs []map[string]any, extra ...line) string {
	t.Helper()
	dir := filepath.Join(home, "projects", strings.ReplaceAll(cwd, "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	parent := any(nil)
	for i, m := range msgs {
		uuid := fmt.Sprintf("%s-%04d", id[:8], i)
		e := line{
			"type": m["role"], "uuid": uuid, "parentUuid": parent,
			"timestamp": start.Add(time.Duration(i) * step).UTC().Format(time.RFC3339Nano),
			"sessionId": id, "cwd": cwd, "message": m, "gocodeSchema": 1,
		}
		parent = uuid
		data, _ := json.Marshal(e)
		b.Write(data)
		b.WriteByte('\n')
	}
	for _, e := range extra {
		data, _ := json.Marshal(e)
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeModel answers each prompt kind with canned JSON and counts calls.
type fakeModel struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeModel) count(k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[k]
}

func (f *fakeModel) fn(ctx context.Context, system, prompt string, maxTokens int) (string, error) {
	kind := "unknown"
	reply := "{}"
	switch {
	case strings.HasPrefix(prompt, facetPromptIntro):
		kind = "facet"
		if maxTokens != 4096 || system != "" {
			return "", fmt.Errorf("bad facet call %d %q", maxTokens, system)
		}
		goal := `{"implement_feature": 2}`
		if strings.Contains(prompt, "hello warmup") {
			goal = `{"warmup_minimal": 1}`
		}
		reply = `Here you go: {"underlying_goal": "Ship the parser", "goal_categories": ` + goal +
			`, "outcome": "fully_achieved", "user_satisfaction_counts": {"satisfied": 1}, "gocode_helpfulness": "very_helpful",` +
			` "session_type": "single_task", "friction_counts": {"buggy_code": 1}, "friction_detail": "One <bug> slipped in",` +
			` "primary_success": "correct_code_edits", "brief_summary": "User wanted a parser and got it"}`
	case strings.HasPrefix(prompt, chunkSummaryPrompt):
		kind = "chunk"
		reply = "summary of chunk"
	case strings.HasPrefix(prompt, "You're writing an \"At a Glance\""):
		kind = "at_a_glance"
		reply = `{"whats_working": "You **iterate** fast & <well>.", "whats_hindering": "Some bugs.", "quick_wins": "Try hooks.", "ambitious_workflows": "Parallel agents."}`
	default:
		for _, s := range insightSections {
			if strings.HasPrefix(prompt, s.prompt) {
				kind = s.name
			}
		}
		switch kind {
		case "project_areas":
			reply = `{"areas": [{"name": "Parser work", "session_count": 2, "description": "Built a parser."}]}`
		case "interaction_style":
			reply = `{"narrative": "You **move fast**.\n\n- point one", "key_pattern": "Fast iteration"}`
		case "suggestions":
			reply = `{"gocode_md_additions": [{"addition": "Run tests", "why": "Because", "prompt_scaffold": "Add under ## Testing"}], "features_to_try": [], "usage_patterns": []}`
		case "fun_ending":
			reply = `{"headline": "The parser parsed itself", "detail": "Tuesday"}`
		case "unknown":
			return "", fmt.Errorf("unexpected prompt: %.80q", prompt)
		default:
			reply = "not json at all"
		}
	}
	f.mu.Lock()
	f.calls[kind]++
	f.mu.Unlock()
	return reply, nil
}

func newFake() *fakeModel { return &fakeModel{calls: map[string]int{}} }

var base = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

const (
	idMain   = "11111111-1111-4111-8111-111111111111"
	idShort  = "22222222-2222-4222-8222-222222222222"
	idQuick  = "33333333-3333-4333-8333-333333333333"
	idWarmup = "44444444-4444-4444-8444-444444444444"
	idSecond = "55555555-5555-4555-8555-555555555555"
)

func mainMessages() []map[string]any {
	return []map[string]any{
		textMsg("user", "Please write a parser in main.go"),
		toolUseMsg("Edit", map[string]any{"file_path": "/w/main.go", "old_string": "a\nb\n", "new_string": "a\nc\nd\n"}),
		toolResultMsg("ok", false),
		toolUseMsg("Bash", map[string]any{"command": "git commit -m parser && git push"}),
		toolResultMsg("Error: exit code 1", true),
		textMsg("assistant", "Done, committed."),
		textMsg("user", "thanks, now add tests"),
		toolUseMsg("Write", map[string]any{"file_path": "/w/main_test.go", "content": "x\ny\nz"}),
		textMsg("user", "[Request interrupted by user]"),
		textMsg("assistant", "Stopped."),
	}
}

func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeSession(t, home, "/w", idMain, base, time.Minute, mainMessages(),
		line{"type": "permission-mode", "sessionId": idMain, "permissionMode": "default"})
	// 1 user message → filtered.
	writeSession(t, home, "/w", idShort, base.Add(time.Hour), time.Minute, []map[string]any{
		textMsg("user", "one question"), textMsg("assistant", "one answer"), textMsg("assistant", "more"),
	})
	// <1 minute → filtered.
	writeSession(t, home, "/w", idQuick, base.Add(2*time.Hour), time.Second, []map[string]any{
		textMsg("user", "q1"), textMsg("assistant", "a1"), textMsg("user", "q2"), textMsg("assistant", "a2"),
	})
	// warmup-only facet → excluded from the aggregate.
	writeSession(t, home, "/other", idWarmup, base.Add(24*time.Hour), time.Minute, []map[string]any{
		textMsg("user", "hello warmup"), textMsg("assistant", "hi"), textMsg("user", "hello warmup again"), textMsg("assistant", "hi"),
	})
	// Second substantive session on another day.
	writeSession(t, home, "/other", idSecond, base.Add(48*time.Hour), 2*time.Minute, []map[string]any{
		textMsg("user", "fix the bug"), toolUseMsg("Read", map[string]any{"file_path": "/o/app.py"}),
		toolResultMsg("contents", false), textMsg("assistant", "fixed"), textMsg("user", "great!"), textMsg("assistant", "np"),
	})
	return home
}

func opts(home string, m *fakeModel) Options {
	return Options{
		ConfigHome: home, Model: m.fn, Location: time.UTC,
		Now: func() time.Time { return time.Date(2026, 3, 5, 9, 8, 7, 0, time.UTC) },
	}
}

// --- tests ------------------------------------------------------------------

func TestSessionMeta(t *testing.T) {
	home := t.TempDir()
	path := writeSession(t, home, "/w", idMain, base, time.Minute, mainMessages(),
		line{"type": "permission-mode", "sessionId": idMain, "permissionMode": "default"},
		line{"type": "summary", "summary": "Parser session"})
	files := listSessionFiles(home)
	if len(files) != 1 || files[0].sessionID != idMain || files[0].path != path {
		t.Fatalf("listSessionFiles = %+v", files)
	}
	logs, err := readSessionLogs(path)
	if err != nil || len(logs) != 1 {
		t.Fatalf("readSessionLogs: %v %d", err, len(logs))
	}
	g := &generator{loc: time.UTC, cmdCache: map[string]bool{}}
	m := g.buildSessionMeta(logs[0])
	checks := []struct {
		name      string
		got, want any
	}{
		{"session_id", m.SessionID, idMain},
		{"project", m.ProjectPath, "/w"},
		{"start", m.StartTime, "2026-03-02T10:00:00.000Z"},
		{"duration", m.DurationMinutes, 9.0},
		{"user msgs", m.UserMessageCount, 3}, // tool results don't count; the interrupt line has text
		{"assistant msgs", m.AssistantMessageCount, 5},
		{"edit count", m.ToolCounts.get("Edit"), 1.0},
		{"bash count", m.ToolCounts.get("Bash"), 1.0},
		{"go lang", m.Languages.get("Go"), 2.0},
		{"lines added", m.LinesAdded, 2 + 3}, // diff c,d + 3 written lines
		{"lines removed", m.LinesRemoved, 1},
		{"files", m.FilesModified, 2},
		{"commits", m.GitCommits, 1},
		{"pushes", m.GitPushes, 1},
		{"tool errors", m.ToolErrors, 1},
		{"error category", m.ToolErrorCategories.get("Command Failed"), 1.0},
		{"interruptions", m.UserInterruptions, 1},
		{"first prompt", m.FirstPrompt, "Please write a parser in main.go"},
		{"summary", m.Summary, "Parser session"},
		{"permission mode", m.PermissionMode, "default"},
		{"bash prompts", *m.BashWouldPromptCount, 1},
		{"edit tools", *m.FileEditToolCount, 2},
		{"hours", fmt.Sprint(m.MessageHours), "[10 10 10]"},
		{"response times", fmt.Sprint(m.UserResponseTimes), "[60 60]"},
	}
	for _, c := range checks {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	lines := transcriptLines(logs[0].messages, 500, 300)
	want := []string{"[User]: Please write a parser in main.go", "[Tool: Edit]", "[Tool: Bash]", "[Assistant]: Done, committed."}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("transcript line %d = %q, want %q", i, lines[i], w)
		}
	}
}

func TestGenerateFilteringAndOutputs(t *testing.T) {
	home := setupHome(t)
	m := newFake()
	rep, err := Generate(context.Background(), opts(home, m))
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Data
	if d.TotalSessionsScanned != 5 {
		t.Errorf("scanned = %d, want 5", d.TotalSessionsScanned)
	}
	if d.TotalSessions != 2 { // short + quick filtered, warmup excluded
		t.Errorf("total sessions = %d, want 2", d.TotalSessions)
	}
	if d.SessionsWithFacets != 2 || len(rep.Facets) != 2 {
		t.Errorf("facets = %d/%d, want 2", d.SessionsWithFacets, len(rep.Facets))
	}
	if _, ok := rep.Facets[idWarmup]; ok {
		t.Error("warmup facet should be excluded")
	}
	if m.count("facet") != 3 { // main, warmup, second
		t.Errorf("facet calls = %d, want 3", m.count("facet"))
	}
	if d.DateRange.Start != "2026-03-02" || d.DateRange.End != "2026-03-04" || d.DaysActive != 2 {
		t.Errorf("date range %+v days %d", d.DateRange, d.DaysActive)
	}
	if d.GoalCategories.get("implement_feature") != 4 || d.Outcomes.get("fully_achieved") != 2 {
		t.Errorf("goal/outcome tallies wrong: %v %v", d.GoalCategories.vals, d.Outcomes.vals)
	}

	// HTML report.
	wantPath := filepath.Join(home, "usage-data", "report-2026-03-05-090807.html")
	wantURL := filepath.ToSlash(wantPath)
	if !strings.HasPrefix(wantURL, "/") {
		wantURL = "/" + wantURL
	}
	wantURL = "file://" + wantURL
	if rep.HTMLPath != wantPath {
		t.Fatalf("HTMLPath = %q, want %q", rep.HTMLPath, wantPath)
	}
	html, err := os.ReadFile(rep.HTMLPath)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(filepath.Join(home, "usage-data", "report.html"))
	if err != nil || string(latest) != string(html) {
		t.Fatalf("report.html missing or different: %v", err)
	}
	for _, want := range []string{
		"<title>gocode Insights</title>",
		"You <strong>iterate</strong> fast &amp; &lt;well&gt;.",
		`<span class="area-name">Parser work</span>`,
		`<span class="area-count">~2 sessions</span>`,
		"<p>You <strong>move fast</strong>.</p>\n<p>• point one</p>",
		`data-text="Add under ## Testing\n\nRun tests"`,
		`"The parser parsed itself"`,
		"const rawHourCounts = {\"10\":",
		"Median: ",
		"No parallel session usage detected.",
	} {
		if !strings.Contains(string(html), want) {
			t.Errorf("html missing %q", want)
		}
	}
	if fi, err := os.Stat(rep.HTMLPath); err == nil && fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("report mode %v not private", fi.Mode().Perm())
	}

	// Final prompt.
	p := rep.Prompt
	if !strings.HasPrefix(p, "The user just ran /insights to generate a usage report analyzing their gocode sessions.\n\nHere is the full insights data:\n{\n  \"project_areas\": {") {
		t.Errorf("prompt prefix wrong:\n%.300s", p)
	}
	for _, want := range []string{
		"\n\nReport URL: " + wantURL + "\nHTML file: " + wantPath + "\nFacets directory: " + filepath.Join(home, "usage-data", "facets") + "\n",
		"# gocode Insights\n\n5 sessions total · 2 analyzed · 5 messages · 0h · 1 commits\n2026-03-02 to 2026-03-04\n\n## At a Glance",
		"## At a Glance\n\n**What's working:** You **iterate** fast & <well>. See _Impressive Things You Did_.",
		"Your shareable insights report is ready:\n" + wantURL + "\n\nWant to dig into any section or try one of the suggestions?",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
	if strings.Contains(p, "interaction_style\": null") {
		t.Error("failed sections must be omitted")
	}
}

func TestFacetAndMetaCaching(t *testing.T) {
	home := setupHome(t)
	m1 := newFake()
	if _, err := Generate(context.Background(), opts(home, m1)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{idMain, idSecond, idWarmup} {
		if _, err := os.Stat(filepath.Join(home, "usage-data", "facets", id+".json")); err != nil {
			t.Errorf("facet cache for %s: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(home, "usage-data", "session-meta", id+".json")); err != nil {
			t.Errorf("meta cache for %s: %v", id, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, "usage-data", "facets", idMain+".json"))
	if !strings.HasPrefix(string(raw), "{\n  \"underlying_goal\": \"Ship the parser\",") || !strings.HasSuffix(string(raw), "\"session_id\": \""+idMain+"\"\n}") {
		t.Errorf("facet file format:\n%s", raw)
	}

	m2 := newFake()
	rep, err := Generate(context.Background(), opts(home, m2))
	if err != nil {
		t.Fatal(err)
	}
	if m2.count("facet") != 0 {
		t.Errorf("second run re-extracted %d facets; want cached", m2.count("facet"))
	}
	if rep.Data.TotalSessions != 2 || rep.Data.SessionsWithFacets != 2 {
		t.Errorf("cached run data %d/%d", rep.Data.TotalSessions, rep.Data.SessionsWithFacets)
	}

	// An invalid cached facet is deleted and re-extracted.
	bad := filepath.Join(home, "usage-data", "facets", idMain+".json")
	if err := os.WriteFile(bad, []byte(`{"outcome": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m3 := newFake()
	if _, err := Generate(context.Background(), opts(home, m3)); err != nil {
		t.Fatal(err)
	}
	// Facets are only extracted for transcripts parsed in
	// this run; the session-meta cache is fresh, so nothing is re-extracted.
	if m3.count("facet") != 0 {
		t.Errorf("fresh meta: facet calls = %d, want 0", m3.count("facet"))
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("invalid facet cache not deleted: %v", err)
	}
	// Touch the transcript so its meta is stale → reparsed → re-extracted.
	main := filepath.Join(home, "projects", "-w", idMain+".jsonl")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(main, later, later); err != nil {
		t.Fatal(err)
	}
	m4 := newFake()
	if _, err := Generate(context.Background(), opts(home, m4)); err != nil {
		t.Fatal(err)
	}
	if m4.count("facet") != 1 {
		t.Errorf("stale meta: facet calls = %d, want 1", m4.count("facet"))
	}
}

func TestLongTranscriptIsChunked(t *testing.T) {
	home := t.TempDir()
	var msgs []map[string]any
	for i := 0; i < 80; i++ {
		msgs = append(msgs, textMsg("user", strings.Repeat("u", 499)), textMsg("assistant", strings.Repeat("a", 299)))
	}
	writeSession(t, home, "/w", idMain, base, time.Minute, msgs)
	m := newFake()
	if _, err := Generate(context.Background(), opts(home, m)); err != nil {
		t.Fatal(err)
	}
	if m.count("chunk") != 3 { // ~65k units → 3 slices of 25000
		t.Errorf("chunk summaries = %d, want 3", m.count("chunk"))
	}
}

func TestSkillPreloadAndErrors(t *testing.T) {
	rep, err := Generate(context.Background(), Options{IsSkillPreload: true})
	if err != nil || rep.Prompt != SkillPreloadText {
		t.Fatalf("preload = %q %v", rep.Prompt, err)
	}
	if _, err := Generate(context.Background(), Options{ConfigHome: t.TempDir()}); err == nil {
		t.Error("missing model should fail")
	}
}

func TestEmptyHome(t *testing.T) {
	home := t.TempDir()
	rep, err := Generate(context.Background(), opts(home, newFake()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Prompt, "# gocode Insights\n\n0 sessions · 0 messages · 0h · 0 commits\n to \n\n") {
		t.Errorf("empty prompt header wrong:\n%s", rep.Prompt)
	}
}

func TestBuildPromptVerbatim(t *testing.T) {
	got := buildPrompt(promptInput{
		insightsJSON: "{}", reportURL: "file:///r.html", htmlPath: "/r.html", facetsDir: "/f",
		header: "H\n", summaryText: "S", recommendationLine: "Tip: x",
	})
	want := "The user just ran /insights to generate a usage report analyzing their gocode sessions.\n\n" +
		"Here is the full insights data:\n{}\n\nReport URL: file:///r.html\nHTML file: /r.html\nFacets directory: /f\n\n" +
		"At-a-glance summary (for your context only — the user has not seen any output yet):\nH\nS\n\n" +
		"Respond with exactly the following, and nothing else. Do not add, omit, or reword any line:\n\n" +
		"Your shareable insights report is ready:\nfile:///r.html\n\nTip: x\n\nWant to dig into any section or try one of the suggestions?"
	if got != want {
		t.Errorf("buildPrompt:\n%q\nwant\n%q", got, want)
	}
}

func TestRecommendAutoMode(t *testing.T) {
	stats := func(auto, non, prompts int, recentAll bool) *AggregatedData {
		return &AggregatedData{AutoModeStats: AutoModeStats{AutoSessions: auto, NonAutoSessions: non, EstimatedPromptsInNonAuto: prompts, DestructiveInNonAuto: 2, RecentAllAuto: recentAll}}
	}
	on := PermissionGates{AutoModeAvailable: true}
	cases := []struct {
		name  string
		d     *AggregatedData
		gates PermissionGates
		want  string
	}{
		{"unavailable", stats(0, 10, 50, false), PermissionGates{}, ""},
		{"too few sessions", stats(0, 4, 50, false), on, ""},
		{"switch", stats(0, 10, 150, false), on, "Tip: auto mode could have handled up to ~150 permission prompts across 10 of your recent sessions — press Shift+Tab until the mode indicator shows auto."},
		{"rounded", stats(1, 9, 1234, false), on, "Tip: auto mode could have handled up to ~1,200 permission prompts across 9 of your recent sessions — press Shift+Tab until the mode indicator shows auto."},
		{"few prompts", stats(0, 10, 2, false), on, ""},
		{"sandbox", stats(0, 10, 50, false), PermissionGates{AutoModeAvailable: true, SandboxAutoAllowActive: true}, ""},
		{"already auto, setup", stats(5, 5, 0, false), PermissionGates{AutoModeAvailable: true, SetupSkillAvailable: true}, autoModeTipSetup},
		{"already auto, env", stats(0, 6, 0, true), PermissionGates{AutoModeAvailable: true, SetupSkillAvailable: true, HasAutoModeEnvironment: true}, ""},
	}
	for _, c := range cases {
		r := recommendAutoMode(c.d, c.gates)
		got := ""
		if r != nil {
			got = recommendationLine(r)
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestAutoModeStatsFromPermissionMode(t *testing.T) {
	home := setupHome(t)
	rep, err := Generate(context.Background(), opts(home, newFake()))
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Data.AutoModeStats
	// Only the main session records a permission mode ("default"): 1 bash
	// prompt + 2 file edits.
	if s.NonAutoSessions != 1 || s.EstimatedPromptsInNonAuto != 3 {
		t.Errorf("auto stats = %+v", s)
	}
}

func TestHelpers(t *testing.T) {
	diffs := []struct {
		old, new string
		add, rem int
	}{
		{"", "a\nb", 2, 0},
		{"a\nb\n", "", 0, 2},
		{"a\nb\n", "a\nc\nd\n", 2, 1},
		{"a\nb", "a\nb\n", 1, 1}, // "b" vs "b\n" are different tokens
		{"x\n", "x\n", 0, 0},
	}
	for _, d := range diffs {
		if a, r := diffLineCounts(d.old, d.new); a != d.add || r != d.rem {
			t.Errorf("diff(%q,%q) = +%d -%d, want +%d -%d", d.old, d.new, a, r, d.add, d.rem)
		}
	}
	destructive := map[string]string{
		"git reset --hard HEAD":      "git_reset_hard",
		"git push origin main -f":    "git_force_push",
		"git clean -fd":              "git_clean_force",
		"git clean -n -f":            "",
		"rm -rf build":               "rm_recursive_force",
		"ls && rm -r x":              "rm_recursive",
		"psql -c 'DROP TABLE users'": "sql_drop_truncate",
		"ls -la":                     "",
	}
	for cmd, want := range destructive {
		if got := destructiveCategory(cmd); got != want {
			t.Errorf("destructiveCategory(%q) = %q, want %q", cmd, got, want)
		}
	}
	for cmd, want := range map[string]bool{"ls -la": true, "git status && git diff": true, "rm x": false, "cat a > b": false, "git push": false} {
		if got := defaultBashAutoAllowed(cmd); got != want {
			t.Errorf("defaultBashAutoAllowed(%q) = %v", cmd, got)
		}
	}
	for in, want := range map[float64]string{0: "0", 999: "999", 1234: "1,234", 1234567: "1,234,567", 12.5: "12.5"} {
		if got := toLocale(in); got != want {
			t.Errorf("toLocale(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{33.333333333333336: "33.333333333333336", 100: "100", 0.5: "0.5"} {
		if got := jsNum(in); got != want {
			t.Errorf("jsNum(%v) = %q", in, got)
		}
	}
	v, err := parseJSValue([]byte(`{"b":1,"a":[true,null,"x<y"],"10":{},"2":2.5}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := stringifyJS(v, ""); got != `{"b":1,"a":[true,null,"x<y"],"10":{},"2":2.5}` {
		t.Errorf("stringify order = %s", got)
	}
	c := newCounts()
	c.add("b", 1)
	c.add("10", 2)
	c.add("2", 3)
	if got := stringifyJS(c, ""); got != `{"2":3,"10":2,"b":1}` {
		t.Errorf("counts JS key order = %s", got)
	}
	if got := titleCase("user_rejected_action2x"); got != "User Rejected Action2x" {
		t.Errorf("titleCase = %q", got)
	}
	if got := truncateUTF16("a\U0001F600b", 2); got != "a" {
		t.Errorf("truncateUTF16 split a surrogate pair: %q", got)
	}
	if got := stripTags("hi <bash-stdout>out</bash-stdout> there <local-command-caveat>c"); got != "hi  there" {
		t.Errorf("stripTags = %q", got)
	}
}

func TestRenderReportAllSections(t *testing.T) {
	full := `{
 "project_areas": {"areas": [{"name": "A", "session_count": 3, "description": "d"}]},
 "interaction_style": {"narrative": "n", "key_pattern": "k"},
 "what_works": {"intro": "i", "impressive_workflows": [{"title": "t", "description": "d"}]},
 "friction_analysis": {"intro": "i", "categories": [{"category": "c", "description": "d", "examples": ["e1", "e2"]}]},
 "suggestions": {"gocode_md_additions": [{"addition": "a", "why": "w"}],
   "features_to_try": [{"feature": "Hooks", "one_liner": "o", "why_for_you": "w", "example_code": "x"}],
   "usage_patterns": [{"title": "t", "suggestion": "s", "detail": "d", "copyable_prompt": "p"}]},
 "on_the_horizon": {"intro": "i", "opportunities": [{"title": "t", "whats_possible": "w", "how_to_try": "h", "copyable_prompt": "p"}]},
 "fun_ending": {"headline": "h", "detail": "d"},
 "at_a_glance": {"whats_working": "a", "whats_hindering": "b", "quick_wins": "c", "ambitious_workflows": "d"}
}`
	v, err := parseJSValue([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	d := aggregate(nil, newFacetMap())
	d.MultiClauding = MultiClauding{OverlapEvents: 2, SessionsInvolved: 2, UserMessagesDuring: 3}
	d.TotalMessages = 10
	d.ToolErrorCategories.add("Command Failed", 2)
	d.UserResponseTimes = []float64{5, 45, 1000}
	html := renderReport(d, v.(*jsObject), &AutoModeRecommendation{Kind: "switch_to_auto", EstimatedPrompts: 1234, NonAutoSessions: 7, DestructiveCommands: 2})
	for _, want := range []string{
		"Auto mode could have handled up to ~1,200 permission prompts for you",
		"It stays careful: 2 of those commands",
		`<div class="feature-title">Hooks</div>`,
		`<ul class="friction-examples"><li>e1</li><li>e2</li></ul>`,
		`<div class="horizon-tip"><strong>Getting started:</strong> h</div>`,
		`data-text="Add to gocode.md\n\na"`,
		`>30%</div>`,
		`<div class="bar-label">Command Failed</div>`,
		`<div class="bar-label">>15m</div>`, // unescaped
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
	setup := renderReport(d, newJSObject(), &AutoModeRecommendation{Kind: "run_setup"})
	if !strings.Contains(setup, "Make auto mode smarter: run /auto-mode-setup") {
		t.Error("setup card missing")
	}
}
