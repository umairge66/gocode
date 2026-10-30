package planmode

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestSlugDeterministic(t *testing.T) {
	a, b := Slug("session-a"), Slug("session-a")
	if a != b {
		t.Fatalf("slug not stable: %q vs %q", a, b)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z]+$`).MatchString(a) {
		t.Fatalf("slug %q is not three words", a)
	}
	seen := map[string]bool{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8"} {
		seen[Slug(id)] = true
	}
	if len(seen) < 6 {
		t.Fatalf("slugs barely vary: %v", seen)
	}
}

func TestNewPlans(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	p := NewPlans(home, proj, "", "s1")
	if p.Dir != filepath.Join(home, "plans") {
		t.Fatalf("dir = %q", p.Dir)
	}
	if got, want := p.Path(""), filepath.Join(home, "plans", Slug("s1")+".md"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if got, want := p.Path("a1"), filepath.Join(home, "plans", Slug("s1")+"-agent-a1.md"); got != want {
		t.Fatalf("agent path = %q, want %q", got, want)
	}
	if p := NewPlans(home, proj, "docs/plans", "s1"); p.Dir != filepath.Join(proj, "docs", "plans") {
		t.Fatalf("plansDirectory dir = %q", p.Dir)
	}
	if p := NewPlans(home, proj, "../outside", "s1"); p.Dir != filepath.Join(home, "plans") {
		t.Fatalf("plansDirectory outside the project was used: %q", p.Dir)
	}
	if NewPlans("", "", "", "s1") != nil {
		t.Fatal("plans without any directory")
	}
	if _, ok := p.Read(""); ok || p.Exists("") {
		t.Fatal("plan exists before it was written")
	}
	if err := p.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Path(""), []byte("# plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.Read(""); !ok || got != "# plan" {
		t.Fatalf("Read = %q, %v", got, ok)
	}
	var nilPlans *Plans
	if nilPlans.Path("") != "" || nilPlans.Exists("") {
		t.Fatal("nil plans has a plan file")
	}
}

func TestReminderTexts(t *testing.T) {
	full := FullReminder(FullOptions{PlanFilePath: "/p/plan.md"})
	for _, want := range []string{
		"Plan mode is active. The user indicated that they do not want you to execute yet -- you MUST NOT make any edits (with the exception of the plan file mentioned below), run any non-readonly tools (including changing configs or making commits), or otherwise make any changes to the system. This supercedes any other instructions you have received.",
		"## Plan File Info:\nNo plan file exists yet. You should create your plan at /p/plan.md using the Write tool.",
		"### Phase 1: Initial Understanding",
		"### Phase 5: Call ExitPlanMode",
		"your turn should only end with either using the AskUserQuestion tool OR calling ExitPlanMode",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("full reminder misses %q", want)
		}
	}
	if !strings.Contains(FullReminder(FullOptions{PlanFilePath: "/p/plan.md", PlanExists: true}), "A plan file already exists at /p/plan.md. You can read it and make incremental edits using the Edit tool.") {
		t.Error("existing plan file note")
	}
	if !strings.Contains(FullReminder(FullOptions{PlanFilePath: "/p", Agents: true}), "Launch up to 3 Explore agents IN PARALLEL") {
		t.Error("agent workflow variant")
	}
	if got, want := SparseReminder("/p/plan.md", false), "Plan mode still active (see full instructions earlier in conversation). Read-only except plan file (/p/plan.md). Follow 5-phase workflow. End turns with AskUserQuestion (for clarifications) or ExitPlanMode (for plan approval). Never ask about plan approval via text or AskUserQuestion."; got != want {
		t.Errorf("sparse =\n%s\nwant\n%s", got, want)
	}
	if got, want := ExitReminder("/p/plan.md", true), "## Exited Plan Mode\n\nYou have exited plan mode. You can now make edits, run tools, and take actions. The plan file is located at /p/plan.md if you need to reference it."; got != want {
		t.Errorf("exit = %q", got)
	}
	if got := Wrap("x"); got != "<system-reminder>\nx\n</system-reminder>" {
		t.Errorf("Wrap = %q", got)
	}
}

// fakeTurn appends a user turn (prompt + reminders) and an assistant reply.
func fakeTurn(h []agent.Message, prompt string, reminders []string) []agent.Message {
	blocks := []agent.ContentBlock{agent.TextBlock(prompt)}
	for _, r := range reminders {
		blocks = append(blocks, agent.TextBlock(r))
	}
	return append(h, agent.UserMessage(blocks...), agent.AssistantMessage(agent.TextBlock("ok")))
}

func kinds(rs []string) []string {
	var out []string
	for _, r := range rs {
		switch {
		case strings.Contains(r, "Plan mode is active."):
			out = append(out, "full")
		case strings.Contains(r, "Plan mode still active"):
			out = append(out, "sparse")
		case strings.Contains(r, "## Re-entering Plan Mode"):
			out = append(out, "reentry")
		case strings.Contains(r, "## Exited Plan Mode"):
			out = append(out, "exit")
		default:
			out = append(out, "?")
		}
	}
	return out
}

func TestTrackerCadence(t *testing.T) {
	var plan atomic.Bool
	plans := NewPlans(t.TempDir(), "", "", "s")
	tr := &Tracker{Plans: plans, InPlanMode: plan.Load}

	var h []agent.Message
	if got := tr.TurnReminders(h); got != nil {
		t.Fatalf("default mode reminders = %q", got)
	}
	h = fakeTurn(h, "hi", nil)

	plan.Store(true)
	tr.ModeChanged(false, true)
	var got []string
	for turn := 1; turn <= 31; turn++ {
		rs := tr.TurnReminders(h)
		if len(rs) > 0 {
			got = append(got, strings.Join(kinds(rs), "+")+"@"+strconv.Itoa(turn))
		}
		h = fakeTurn(h, "prompt", rs)
	}
	want := []string{"full@1", "sparse@7", "sparse@13", "sparse@19", "sparse@25", "full@31"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("cadence = %v, want %v", got, want)
	}
	if !strings.Contains(h[len(h)-2].Content[1].Text, plans.Path("")) {
		t.Fatal("reminder does not name the plan file")
	}
	if _, err := os.Stat(plans.Dir); err != nil {
		t.Fatalf("plans dir not created: %v", err)
	}

	// Leaving plan mode: one exit note, then nothing.
	plan.Store(false)
	tr.ModeChanged(true, false)
	rs := tr.TurnReminders(h)
	if strings.Join(kinds(rs), ",") != "exit" {
		t.Fatalf("after leaving = %v", kinds(rs))
	}
	h = fakeTurn(h, "go", rs)
	if rs := tr.TurnReminders(h); rs != nil {
		t.Fatalf("second turn after leaving = %v", kinds(rs))
	}
}

func TestTrackerMidTurnAndReentry(t *testing.T) {
	var plan atomic.Bool
	plans := NewPlans(t.TempDir(), "", "", "s")
	tr := &Tracker{Plans: plans, InPlanMode: plan.Load}
	var h []agent.Message
	h = fakeTurn(h, "hi", tr.TurnReminders(h))

	// EnterPlanMode mid-turn: the full reminder arrives before the next call.
	plan.Store(true)
	tr.ModeChanged(false, true)
	mid := tr.TakeMidTurn()
	if strings.Join(kinds(mid), ",") != "full" {
		t.Fatalf("mid-turn after entering = %v", kinds(mid))
	}
	if tr.TakeMidTurn() != nil {
		t.Fatal("mid-turn reminder repeated")
	}
	h = fakeTurn(h, "plan", mid)
	if rs := tr.TurnReminders(h); rs != nil {
		t.Fatalf("next turn repeats the reminder: %v", kinds(rs))
	}

	// The plan is approved mid-turn: the exit note follows.
	if err := plans.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plans.Path(""), []byte("# p"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr.PlanApproved()
	plan.Store(false)
	tr.ModeChanged(true, false)
	mid = tr.TakeMidTurn()
	if strings.Join(kinds(mid), ",") != "exit" || !strings.Contains(mid[0], plans.Path("")) {
		t.Fatalf("mid-turn after approval = %q", mid)
	}
	h = fakeTurn(h, "x", mid)
	if rs := tr.TurnReminders(h); rs != nil {
		t.Fatalf("exit repeated: %v", kinds(rs))
	}

	// Entering plan mode again with the old plan on disk: re-entry first.
	plan.Store(true)
	tr.ModeChanged(false, true)
	rs := tr.TurnReminders(h)
	if strings.Join(kinds(rs), ",") != "reentry,full" {
		t.Fatalf("re-entry = %v", kinds(rs))
	}
	if tr.HasPending() || tr.TakePending() != nil {
		t.Fatal("tracker queues next-turn context")
	}
}
