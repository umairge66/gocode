package workflows

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeRunner is a pure, deterministic StepRunner. It records the steps it ran
// and returns a response computed from the prompt, with no real agents.
type fakeRunner struct {
	mu    sync.Mutex
	steps []Step
	// respond maps prompt -> (value, err). A nil value with nil err models a
	// JS null return (skip / terminal death).
	respond func(step Step) (any, error)
	// peak tracks the maximum observed concurrency.
	live int32
	peak int32
}

func (f *fakeRunner) RunStep(ctx context.Context, step Step) (any, error) {
	n := atomic.AddInt32(&f.live, 1)
	for {
		p := atomic.LoadInt32(&f.peak)
		if n <= p || atomic.CompareAndSwapInt32(&f.peak, p, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.live, -1)

	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(step)
	}
	return "ok:" + step.Prompt, nil
}

func TestConcurrencyCap(t *testing.T) {
	cases := []struct{ cpus, want int }{
		{1, 2}, {3, 2}, {4, 2}, {5, 3}, {18, 16}, {64, 16},
	}
	for _, c := range cases {
		if got := ConcurrencyCap(c.cpus); got != c.want {
			t.Errorf("ConcurrencyCap(%d) = %d, want %d", c.cpus, got, c.want)
		}
	}
}

func TestParallelBarrierAndNulls(t *testing.T) {
	fr := &fakeRunner{respond: func(s Step) (any, error) {
		if s.Prompt == "bad" {
			return nil, fmt.Errorf("boom")
		}
		if s.Prompt == "skip" {
			return nil, nil // JS null
		}
		return s.Prompt, nil
	}}
	ex := NewExecutor(fr, 4)
	ctx := context.Background()

	prompts := []string{"a", "bad", "b", "skip", "c"}
	thunks := make([]func(context.Context) (any, error), len(prompts))
	for i, p := range prompts {
		p := p
		thunks[i] = func(ctx context.Context) (any, error) {
			return ex.Agent(ctx, Step{Prompt: p, Phase: "Find"})
		}
	}
	got, err := ex.Parallel(ctx, thunks)
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	// Positional results; bad and skip are nil.
	want := []any{"a", nil, "b", nil, "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// filter(Boolean) keeps 3.
	var kept []any
	for _, v := range got {
		if v != nil {
			kept = append(kept, v)
		}
	}
	if len(kept) != 3 {
		t.Errorf("filter(Boolean) kept %d, want 3", len(kept))
	}
	if ex.AgentCount() != 5 {
		t.Errorf("AgentCount = %d, want 5", ex.AgentCount())
	}
	if len(ex.Failures()) != 1 {
		t.Errorf("failures = %v", ex.Failures())
	}
}

func TestPipelineNoBarrierAndBreakOnNull(t *testing.T) {
	// Two stages: stage1 reviews each item, stage2 verifies. A review that
	// returns null (skip) must break the chain so stage2 never runs for it.
	var stage2Count int32
	fr := &fakeRunner{respond: func(s Step) (any, error) {
		if s.Phase == "Review" && s.Prompt == "review:skipme" {
			return nil, nil // null review -> break chain
		}
		return "done:" + s.Prompt, nil
	}}
	ex := NewExecutor(fr, 8)
	ctx := context.Background()

	items := []any{"bugs", "skipme", "perf"}
	stage1 := func(ctx context.Context, prev, item any, i int) (any, error) {
		return ex.Agent(ctx, Step{Prompt: "review:" + item.(string), Phase: "Review"})
	}
	stage2 := func(ctx context.Context, prev, item any, i int) (any, error) {
		atomic.AddInt32(&stage2Count, 1)
		return ex.Agent(ctx, Step{Prompt: "verify:" + item.(string), Phase: "Verify"})
	}
	got, err := ex.Pipeline(ctx, items, stage1, stage2)
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}
	// "skipme" broke after stage1 -> nil final; others reached verify.
	if got[0] != "done:verify:bugs" || got[2] != "done:verify:perf" {
		t.Errorf("results = %+v", got)
	}
	if got[1] != nil {
		t.Errorf("skipme should be nil, got %v", got[1])
	}
	if stage2Count != 2 {
		t.Errorf("stage2 ran %d times, want 2 (skipme skipped)", stage2Count)
	}
}

func TestPipelineStageReceivesItemAndIndex(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, 4)
	items := []any{"x", "y"}
	var seen sync.Map
	stage := func(ctx context.Context, prev, item any, i int) (any, error) {
		seen.Store(item, i)
		return prev, nil
	}
	if _, err := ex.Pipeline(context.Background(), items, stage); err != nil {
		t.Fatal(err)
	}
	if v, _ := seen.Load("x"); v != 0 {
		t.Errorf("x index = %v", v)
	}
	if v, _ := seen.Load("y"); v != 1 {
		t.Errorf("y index = %v", v)
	}
}

func TestPhaseAndLogTracking(t *testing.T) {
	ex := NewExecutor(&fakeRunner{}, 4)
	ex.Phase("Scan")
	ex.Log("scanning")
	ex.Phase("Fix")
	ex.Phase("Scan") // re-entering an existing phase does not duplicate
	phases := ex.Phases()
	if len(phases) != 2 || phases[0] != "Scan" || phases[1] != "Fix" {
		t.Errorf("phases = %v", phases)
	}
	if logs := ex.Logs(); len(logs) != 1 || logs[0] != "scanning" {
		t.Errorf("logs = %v", logs)
	}
}

func TestAgentInheritsCurrentPhase(t *testing.T) {
	fr := &fakeRunner{}
	ex := NewExecutor(fr, 4)
	ex.Phase("Review")
	if _, err := ex.Agent(context.Background(), Step{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Agent(context.Background(), Step{Prompt: "q", Phase: "Explicit"}); err != nil {
		t.Fatal(err)
	}
	if fr.steps[0].Phase != "Review" {
		t.Errorf("step0 phase = %q, want Review", fr.steps[0].Phase)
	}
	if fr.steps[1].Phase != "Explicit" {
		t.Errorf("step1 phase = %q, want Explicit", fr.steps[1].Phase)
	}
	if fr.steps[0].Index != 0 || fr.steps[1].Index != 1 {
		t.Errorf("indices = %d,%d", fr.steps[0].Index, fr.steps[1].Index)
	}
}

func TestFanoutRespectsConcurrencyCap(t *testing.T) {
	fr := &fakeRunner{respond: func(s Step) (any, error) { return s.Prompt, nil }}
	ex := NewExecutor(fr, 3)
	thunks := make([]func(context.Context) (any, error), 30)
	// Gate so all slots want to run at once.
	start := make(chan struct{})
	for i := range thunks {
		i := i
		thunks[i] = func(ctx context.Context) (any, error) {
			<-start
			return ex.Agent(ctx, Step{Prompt: fmt.Sprintf("p%d", i)})
		}
	}
	go close(start)
	if _, err := ex.Parallel(context.Background(), thunks); err != nil {
		t.Fatal(err)
	}
	if fr.peak > 3 {
		t.Errorf("observed concurrency %d exceeds cap 3", fr.peak)
	}
	if ex.AgentCount() != 30 {
		t.Errorf("AgentCount = %d", ex.AgentCount())
	}
}

func TestFanoutItemCap(t *testing.T) {
	ex := NewExecutor(&fakeRunner{}, 4)
	toomany := make([]any, MaxFanoutItems+1)
	if _, err := ex.Pipeline(context.Background(), toomany); err == nil {
		t.Error("expected pipeline item-cap error")
	}
	thunks := make([]func(context.Context) (any, error), MaxFanoutItems+1)
	if _, err := ex.Parallel(context.Background(), thunks); err == nil {
		t.Error("expected parallel item-cap error")
	}
}

func TestAgentCallCap(t *testing.T) {
	ex := NewExecutor(&fakeRunner{}, 4)
	ctx := context.Background()
	for i := 0; i < MaxAgentCalls; i++ {
		if _, err := ex.Agent(ctx, Step{Prompt: "x"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := ex.Agent(ctx, Step{Prompt: "over"}); err == nil {
		t.Fatal("expected agent-call cap error")
	}
}

// TestEndToEndReviewGraph drives the full review-changes shape from the
// authoring reference: pipeline over dimensions, each reviewed then its
// findings verified in parallel, aggregating confirmed findings — all with the
// fake runner and no real agents.
func TestEndToEndReviewGraph(t *testing.T) {
	type finding struct {
		dim  string
		real bool
	}
	fr := &fakeRunner{respond: func(s Step) (any, error) {
		switch s.Phase {
		case "Review":
			// Each dimension yields two findings (one real, one not); "perf"
			// yields none.
			if s.Prompt == "review:perf" {
				return []finding{}, nil
			}
			return []finding{{dim: s.Prompt, real: true}, {dim: s.Prompt, real: false}}, nil
		case "Verify":
			// Deterministic verdict: the finding's real-ness is encoded in the
			// prompt suffix (":true"/":false"), so the result does not depend
			// on run order.
			return strings.HasSuffix(s.Prompt, ":true"), nil
		}
		return nil, nil
	}}
	ex := NewExecutor(fr, 8)
	ctx := context.Background()

	dims := []any{"bugs", "perf", "db"}
	review := func(ctx context.Context, prev, item any, i int) (any, error) {
		return ex.Agent(ctx, Step{Prompt: "review:" + item.(string), Phase: "Review"})
	}
	verify := func(ctx context.Context, prev, item any, i int) (any, error) {
		fs, _ := prev.([]finding)
		thunks := make([]func(context.Context) (any, error), len(fs))
		for j := range fs {
			j := j
			thunks[j] = func(ctx context.Context) (any, error) {
				v, err := ex.Agent(ctx, Step{Prompt: fmt.Sprintf("verify:%s:%d:%v", item.(string), j, fs[j].real), Phase: "Verify"})
				if err != nil {
					return nil, err
				}
				if v == true {
					return fs[j], nil
				}
				return nil, nil
			}
		}
		verdicts, err := ex.Parallel(ctx, thunks)
		if err != nil {
			return nil, err
		}
		var confirmed []finding
		for _, vd := range verdicts {
			if vd != nil {
				confirmed = append(confirmed, vd.(finding))
			}
		}
		return confirmed, nil
	}

	results, err := ex.Pipeline(ctx, dims, review, verify)
	if err != nil {
		t.Fatalf("Pipeline: %v", err)
	}

	// Aggregate confirmed across dimensions.
	var confirmedDims []string
	total := 0
	for _, r := range results {
		fs, _ := r.([]finding)
		for _, f := range fs {
			confirmedDims = append(confirmedDims, f.dim)
			total++
		}
	}
	sort.Strings(confirmedDims)

	// bugs: 2 findings, perf: 0, db: 2 findings = 4 verify calls plus 3 reviews.
	if ex.AgentCount() != 3+4 {
		t.Errorf("AgentCount = %d, want 7 (3 review + 4 verify)", ex.AgentCount())
	}
	// Exactly the two real findings (bugs, db) survive verification; perf
	// produced none.
	if total != 2 {
		t.Errorf("confirmed total = %d, want 2", total)
	}
	wantDims := []string{"review:bugs", "review:db"}
	if len(confirmedDims) != 2 || confirmedDims[0] != wantDims[0] || confirmedDims[1] != wantDims[1] {
		t.Errorf("confirmed dims = %v, want %v", confirmedDims, wantDims)
	}
}
