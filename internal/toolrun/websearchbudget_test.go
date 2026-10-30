package toolrun

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/subagent"
	"github.com/umairge66/gocode/internal/toolreg"
)

// A used-up session budget stops WebSearch before the backend is called, and
// tells the model to carry on rather than work around the limit.
func TestWebSearchBudgetUsedUp(t *testing.T) {
	be := &fakeBackend{}
	two := func() int64 { return 2 }
	budget := subagent.NewWebSearchBudget(two, nil, false)
	d := New(toolreg.ModeBypassPermissions, nil, WithSearchBackend(be), WithWebSearchBudget(budget))
	for i := range 2 {
		be.query = ""
		res := dispatch(t, d, string(protocol.ToolWebSearch), map[string]any{"query": "golang"})
		if res.IsError || be.query != "golang" {
			t.Fatalf("search %d within budget: %+v", i+1, res)
		}
	}
	be.query = ""
	res := dispatch(t, d, string(protocol.ToolWebSearch), map[string]any{"query": "golang"})
	if be.query != "" {
		t.Fatal("backend called after the budget was used up")
	}
	for _, want := range []string{
		"Web search was not performed: this session's web search budget is used up (limit: 2 web search calls, shared by every agent in this session; it does not refill)",
		"do not work around the limit",
		"GOCODE_CODE_MAX_WEB_SEARCHES_PER_SESSION",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result missing %q:\n%s", want, res.Content)
		}
	}

	// A subagent dispatcher sharing the budget is refused too.
	child := New(toolreg.ModeBypassPermissions, nil, WithSearchBackend(be), WithWebSearchBudget(d.WebSearchBudget))
	dispatch(t, child, string(protocol.ToolWebSearch), map[string]any{"query": "golang"})
	if be.query != "" {
		t.Fatal("subagent searched past the shared budget")
	}
}

func TestWebSearchCapMessageRefill(t *testing.T) {
	got := webSearchCapMessage(200, 1)
	if !strings.Contains(got, "refills at about 1 call per hour") || !strings.Contains(got, "Do not pause or wait for the refill") {
		t.Fatalf("refill message wrong:\n%s", got)
	}
}
