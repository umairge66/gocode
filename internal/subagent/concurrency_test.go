package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestMaxConcurrentFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 0, "5": 5, " 40 ": 40, "0": 0, "-1": 0, "x": 0, "1.5": 0, "1234567890": 0} {
		if got := MaxConcurrentFromEnv(func(string) string { return v }); got != want {
			t.Errorf("%q → %d, want %d", v, got, want)
		}
	}
	if MaxConcurrentFromEnv(nil) != 0 {
		t.Error("nil getenv")
	}
}

func TestTryTakeConcurrencySlot(t *testing.T) {
	r := NewTaskRegistry()
	a, ok1 := r.TryTakeConcurrencySlot(2)
	b, ok2 := r.TryTakeConcurrencySlot(2)
	if _, ok := r.TryTakeConcurrencySlot(2); !ok1 || !ok2 || ok {
		t.Fatalf("took %v %v, third %v", ok1, ok2, ok)
	}
	a()
	a() // once only
	if r.ConcurrentSubagents() != 1 {
		t.Fatalf("running = %d", r.ConcurrentSubagents())
	}
	if _, ok := r.TryTakeConcurrencySlot(2); !ok {
		t.Fatal("a released slot is not available again")
	}
	b()
	if _, ok := r.TryTakeConcurrencySlot(0); !ok {
		t.Fatal("max 0 is no cap")
	}
}

// blockingModel holds every call until release is closed.
type blockingModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *blockingModel) CallModel(ctx context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	m.started <- struct{}{}
	<-m.release
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("done")), StopReason: agent.StopEndTurn}, nil
}

// A spawn past the concurrency cap is refused with the cap error and leaves
// no task behind; once a running agent ends, spawning works again.
func TestSpawnConcurrentLimit(t *testing.T) {
	sp := newSpawner()
	sp.MaxConcurrent = 2
	m := &blockingModel{started: make(chan struct{}), release: make(chan struct{})}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, m, nopDispatcher{}); err != nil {
				t.Errorf("spawn: %v", err)
			}
		}()
	}
	<-m.started
	<-m.started

	before := len(sp.Registry.List())
	_, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, &scriptedModel{}, nopDispatcher{})
	if !errors.Is(err, ErrConcurrentLimit) || task != nil || !strings.Contains(err.Error(), "Concurrent subagent limit reached") {
		t.Fatalf("third spawn = %v, %v", task, err)
	}
	if len(sp.Registry.List()) != before {
		t.Error("a refused spawn registered a task")
	}

	close(m.release)
	wg.Wait()
	if _, _, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, &scriptedModel{}, nopDispatcher{}); err != nil {
		t.Fatalf("spawn after the others ended: %v", err)
	}
	if n := sp.Registry.ConcurrentSubagents(); n != 0 {
		t.Errorf("slots held after every spawn ended: %d", n)
	}
}

// With the spend cap reached no agent starts; a running child stops on it.
func TestSpawnRespectsBudget(t *testing.T) {
	sp := newSpawner()
	capped := errors.New("Spend cap reached: $1.00 spent of the $1.00 limit")
	sp.Budget = func() error { return capped }
	_, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, &scriptedModel{}, nopDispatcher{})
	if !errors.Is(err, ErrSpendCap) || task != nil || !strings.Contains(err.Error(), "$1.00 spent of the $1.00 limit") ||
		!strings.Contains(err.Error(), "no new agent can be started") {
		t.Fatalf("spawn = %v, %v", task, err)
	}

	// The child loop carries the budget: it is reached after its first response.
	calls := 0
	sp.Budget = func() error {
		if calls > 0 {
			return capped
		}
		return nil
	}
	read := agent.ModelResponse{Message: agent.AssistantMessage(agent.ToolUseBlock("t0", "Read", []byte(`{}`))), StopReason: agent.StopToolUse}
	m := &countingModel{inner: &scriptedModel{responses: []agent.ModelResponse{read, handbackResp("t1", "r")}}, calls: &calls}
	_, task, err = sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, m, nopDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || task.Status == StatusCompleted {
		t.Errorf("child calls = %d, status = %s; want one call and a stopped run", calls, task.Status)
	}

	// The response that crosses the cap is the hand-back: it still runs, so
	// the report is delivered and the agent completes.
	calls = 0
	stopChecks := 0
	sp.StopHook = func(context.Context, StopHookInput) StopHookDecision {
		stopChecks++
		return StopHookDecision{Block: true}
	}
	m = &countingModel{inner: &scriptedModel{responses: []agent.ModelResponse{handbackResp("t1", "the report")}}, calls: &calls}
	report, task, err := sp.Spawn(context.Background(), SpawnRequest{SubagentType: "general-purpose", Prompt: "p"}, m, nopDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || task.Status != StatusCompleted || !report.Delivered || report.Text != "the report" {
		t.Errorf("child calls = %d, status = %s, report = %+v; want the hand-back delivered", calls, task.Status, report)
	}
	// Past the cap a blocking stop hook cannot start another request.
	if stopChecks != 1 {
		t.Errorf("stop hook ran %d times, want once", stopChecks)
	}
}

type countingModel struct {
	inner agent.ModelCaller
	calls *int
}

func (m *countingModel) CallModel(ctx context.Context, req agent.ModelRequest) (agent.ModelResponse, error) {
	*m.calls++
	return m.inner.CallModel(ctx, req)
}
