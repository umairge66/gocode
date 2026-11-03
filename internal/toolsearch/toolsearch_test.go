package toolsearch

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		val, betas string
		want       Mode
	}{
		{"", "", ModeOn},
		{"true", "", ModeOn},
		{"1", "", ModeOn},
		{"false", "", ModeStandard},
		{"0", "", ModeStandard},
		{"auto", "", ModeAuto},
		{"auto:25", "", ModeAuto},
		{"auto:0", "", ModeOn},
		{"auto:100", "", ModeStandard},
		{"auto:x", "", ModeAuto},
		{"weird", "", ModeOn},
		{"", "1", ModeStandard},
		{"true", "1", ModeStandard},
		{"force", "1", ModeOn},
	}
	for _, c := range cases {
		got := ResolveMode(env(map[string]string{EnvVar: c.val, DisableBetasEnvVar: c.betas}))
		if got != c.want {
			t.Errorf("ENABLE_TOOL_SEARCH=%q betasOff=%q: %v, want %v", c.val, c.betas, got, c.want)
		}
	}
	if ResolveMode(nil) != ModeOn {
		t.Error("default should be on")
	}
}

func TestAutoThresholdChars(t *testing.T) {
	if got := AutoThresholdChars(env(nil), 200000); got != 50000 {
		t.Errorf("10%% of 200k tokens = %d chars, want 50000", got)
	}
	if got := AutoThresholdChars(env(map[string]string{EnvVar: "auto:50"}), 100000); got != 125000 {
		t.Errorf("auto:50 of 100k = %d, want 125000", got)
	}
}

func TestIsDeferred(t *testing.T) {
	for _, n := range []string{"NotebookEdit", "WebFetch", "WebSearch", "SendMessage", "navigate", "mcp__a__b"} {
		if !IsDeferred(n, false) {
			t.Errorf("%s should be deferred", n)
		}
	}
	for _, n := range []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "Agent", "Skill", "Workflow", "ListAgents", "AskUserQuestion", "TodoWrite", Name} {
		if IsDeferred(n, false) {
			t.Errorf("%s should always be loaded", n)
		}
	}
	if !IsDeferred("custom", true) {
		t.Error("an MCP tool is always deferred")
	}
}

func catalog() []Tool {
	return []Tool{
		{Name: "Read", Description: "Reads a file from the local filesystem."},
		{Name: "NotebookEdit", Description: "Edit, insert, or delete a cell in a Jupyter notebook.", Deferred: true},
		{Name: "WebFetch", Description: "Fetches content from a URL and processes it.", Deferred: true},
		{Name: "WebSearch", Description: "Searches the web.", Deferred: true},
		{Name: "TodoWrite", Description: "Create and manage a structured task list.", Deferred: true},
		{Name: "mcp__slack__send_message", Description: "Send a message to a Slack channel.", Deferred: true},
		{Name: "mcp__slack__list_channels", Description: "List Slack channels.", Deferred: true},
		{Name: "mcp__mail__send_message", Description: "Send an email.", Deferred: true},
		{Name: "mcp__calendar__create_event", Description: "Create a calendar event.", Deferred: true},
	}
}

func TestSearchSelect(t *testing.T) {
	got := Search("select:WebFetch,NotebookEdit,Missing,WebFetch", catalog(), 5)
	if strings.Join(got, ",") != "WebFetch,NotebookEdit" {
		t.Fatalf("select = %v", got)
	}
	// Not-deferred tools are found too; case-insensitive prefix; spaces.
	got = Search("SELECT: Read , TodoWrite", catalog(), 5)
	if strings.Join(got, ",") != "Read,TodoWrite" {
		t.Fatalf("select = %v", got)
	}
	// A bare MCP tool name selects every server's tool of that name.
	got = Search("select:send_message", catalog(), 5)
	if strings.Join(got, ",") != "mcp__slack__send_message,mcp__mail__send_message" {
		t.Fatalf("bare MCP select = %v", got)
	}
	if got := Search("select:Nope", catalog(), 5); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}
	// select ignores max_results.
	if got := Search("select:WebFetch,WebSearch,TodoWrite", catalog(), 1); len(got) != 3 {
		t.Fatalf("select capped by max_results: %v", got)
	}
}

func TestSearchKeyword(t *testing.T) {
	if got := Search("notebook jupyter", catalog(), 5); len(got) == 0 || got[0] != "NotebookEdit" {
		t.Fatalf("notebook jupyter = %v", got)
	}
	// Exact name.
	if got := Search("webfetch", catalog(), 5); strings.Join(got, ",") != "WebFetch" {
		t.Fatalf("exact name = %v", got)
	}
	// Server prefix.
	if got := Search("mcp__slack", catalog(), 5); strings.Join(got, ",") != "mcp__slack__send_message,mcp__slack__list_channels" {
		t.Fatalf("server prefix = %v", got)
	}
	// "+slack" is required; "send" ranks.
	got := Search("+slack send", catalog(), 5)
	if len(got) != 2 || got[0] != "mcp__slack__send_message" {
		t.Fatalf("+slack send = %v", got)
	}
	for _, n := range got {
		if !strings.Contains(n, "slack") {
			t.Fatalf("required term ignored: %v", got)
		}
	}
	// max_results caps keyword results.
	if got := Search("send message", catalog(), 1); len(got) != 1 {
		t.Fatalf("max_results 1 = %v", got)
	}
	if got := Search("send message", catalog(), 0); len(got) > DefaultMaxMatch {
		t.Fatalf("default max = %v", got)
	}
	// Search hints match: "url" only appears in WebFetch's hint and description.
	if got := Search("url", catalog(), 5); len(got) == 0 || got[0] != "WebFetch" {
		t.Fatalf("url = %v", got)
	}
	// Keyword search only returns deferred tools (Read is not).
	for _, n := range Search("file filesystem", catalog(), 5) {
		if n == "Read" {
			t.Fatalf("keyword search returned a loaded tool")
		}
	}
	if got := Search("zzzz", catalog(), 5); len(got) != 0 {
		t.Fatalf("nonsense = %v", got)
	}
}

func TestQueryCaps(t *testing.T) {
	var names []string
	for i := 0; i < 80; i++ {
		names = append(names, "WebFetch")
	}
	q := normalizeQuery("select:" + strings.Join(names, ","))
	if strings.Count(q, ",")+1 != maxSelectNames {
		t.Fatalf("select names not capped: %d", strings.Count(q, ",")+1)
	}
	q = normalizeQuery(strings.Repeat("word ", 40))
	if len(strings.Fields(q)) != maxQueryWords {
		t.Fatalf("keyword words not capped: %d", len(strings.Fields(q)))
	}
}

func TestResultText(t *testing.T) {
	if ResultText(nil) != NoMatches {
		t.Fatal("no-match text")
	}
	if got := ResultText([]string{"A", "B"}); !strings.Contains(got, "A, B") {
		t.Fatal(got)
	}
}

func TestNativeAllowed(t *testing.T) {
	const def = "https://api.anthropic.com"
	if !NativeAllowed("", def, nil) || !NativeAllowed(def+"/", def, env(nil)) {
		t.Fatal("default endpoint should be native")
	}
	if NativeAllowed("http://127.0.0.1:9", def, env(nil)) {
		t.Fatal("custom endpoint is native only on request")
	}
	if !NativeAllowed("http://127.0.0.1:9", def, env(map[string]string{EnvVar: "true"})) {
		t.Fatal("ENABLE_TOOL_SEARCH=true lifts the custom-endpoint default")
	}
	if NativeAllowed("", def, env(map[string]string{EnvVar: "false"})) {
		t.Fatal("off means off")
	}
}

func TestModelSupportsToolReference(t *testing.T) {
	if ModelSupportsToolReference("claude-3-5-sonnet-20241022") || ModelSupportsToolReference("gocode-3-7-sonnet") {
		t.Fatal("3.x models do not read tool_reference blocks")
	}
	if !ModelSupportsToolReference("claude-sonnet-4-5") || !ModelSupportsToolReference("gocode-opus-4-1") {
		t.Fatal("4.x models do")
	}
}

func TestIsDeferredHeadless(t *testing.T) {
	for _, n := range []string{"Workflow", "ListAgents"} {
		if IsDeferred(n, false) {
			t.Errorf("%s deferred in an interactive session", n)
		}
		if !IsDeferredCompact(n, false) {
			t.Errorf("%s not deferred in a headless session", n)
		}
	}
	for _, n := range []string{"WebFetch", "mcp__srv__tool"} {
		if !IsDeferredCompact(n, false) {
			t.Errorf("%s should stay deferred in a headless session", n)
		}
	}
	for _, n := range []string{Name, "Agent", "TodoWrite", "Read", "Bash"} {
		if IsDeferredCompact(n, false) {
			t.Errorf("%s should stay loaded in a headless session", n)
		}
	}
}

func TestIncomplete(t *testing.T) {
	for _, c := range []struct {
		q     string
		names []string
		want  bool
	}{
		{"notebook", nil, true},
		{"notebook", []string{"NotebookEdit"}, false},
		{"select:A,B", []string{"A"}, true},
		{"select:A, B,", []string{"A", "B"}, false},
		{"SELECT:A", []string{"mcp__x__A", "mcp__y__A"}, false},
	} {
		if got := Incomplete(c.q, c.names); got != c.want {
			t.Errorf("Incomplete(%q, %v) = %v, want %v", c.q, c.names, got, c.want)
		}
	}
}
