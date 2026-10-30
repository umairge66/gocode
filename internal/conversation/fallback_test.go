package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// triggerErr mimics provider.FallbackTriggeredError.
type triggerErr struct{ from, to, reason string }

func (e *triggerErr) Error() string {
	return fmt.Sprintf("Model fallback triggered: %s -> %s", e.from, e.to)
}
func (e *triggerErr) FallbackTarget() (string, string, string, error) {
	return e.from, e.to, e.reason, errors.New("overloaded")
}

// fallbackFake answers with the model it is set to; models in down fail with a
// fallback trigger toward the armed fallback model.
type fallbackFake struct {
	mu       sync.Mutex
	model    string
	armed    string
	down     map[string]bool
	served   []string
	armedLog []string
}

func (f *fallbackFake) SetModel(m string)         { f.mu.Lock(); f.model = m; f.mu.Unlock() }
func (f *fallbackFake) CurrentModel() string      { f.mu.Lock(); defer f.mu.Unlock(); return f.model }
func (f *fallbackFake) SetFallbackModel(m string) { f.mu.Lock(); f.armed = m; f.mu.Unlock() }

func (f *fallbackFake) CallModel(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armedLog = append(f.armedLog, f.armed)
	if f.down[f.model] {
		if f.armed == "" {
			return agent.ModelResponse{}, errors.New("plain failure: " + f.model)
		}
		return agent.ModelResponse{}, &triggerErr{from: f.model, to: f.armed, reason: "overloaded"}
	}
	f.served = append(f.served, f.model)
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("from " + f.model)), StopReason: agent.StopEndTurn}, nil
}

func TestFallbackSwitchesModelAndRestoresPrimary(t *testing.T) {
	m := &fallbackFake{model: "primary", down: map[string]bool{"primary": true}}
	c := New(m, fakeDispatcher{})
	c.FallbackModels = []string{"primary", "second", "third"}
	c.FallbackNotice = func(reason, original, fallback string, _ error) string {
		return reason + ":" + original + "->" + fallback
	}
	res, err := c.Submit(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.AssistantText != "from second" {
		t.Fatalf("text = %q, want served by the fallback", res.AssistantText)
	}
	if len(res.Notices) != 1 || res.Notices[0] != "overloaded:primary->second" {
		t.Fatalf("notices = %v", res.Notices)
	}
	// After the turn the primary is selected again and nothing is armed.
	if m.CurrentModel() != "primary" || m.armed != "" {
		t.Fatalf("after turn model=%q armed=%q, want primary and none", m.model, m.armed)
	}
	// The first request armed "second", the retry armed "third".
	if strings.Join(m.armedLog, ",") != "second,third" {
		t.Fatalf("armed log = %v", m.armedLog)
	}

	// The next turn retries the primary first (it is healthy now).
	m.down = map[string]bool{}
	res, _ = c.Submit(context.Background(), "again")
	if res.AssistantText != "from primary" || len(res.Notices) != 0 {
		t.Fatalf("second turn = %q notices=%v, want primary", res.AssistantText, res.Notices)
	}
}

func TestFallbackChainExhaustedSurfacesOriginalError(t *testing.T) {
	m := &fallbackFake{model: "a", down: map[string]bool{"a": true, "b": true}}
	c := New(m, fakeDispatcher{})
	c.FallbackModels = []string{"b"}
	var live []string
	c.OnNotice = func(s string) { live = append(live, s) }
	res, err := c.Submit(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "plain failure: b") {
		t.Fatalf("err = %v, want the last model's failure", res.Err)
	}
	if len(live) != 1 || len(res.Notices) != 0 {
		t.Fatalf("live=%v notices=%v, want one live notice", live, res.Notices)
	}
}

func TestFallbackChain(t *testing.T) {
	got := fallbackChain("a", []string{"", "a", "b", "b", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("chain = %v", got)
	}
}

func TestNoFallbackWithoutModels(t *testing.T) {
	m := &fallbackFake{model: "a", down: map[string]bool{"a": true}}
	c := New(m, fakeDispatcher{})
	res, _ := c.Submit(context.Background(), "hi")
	if res.Err == nil || m.armedLog[0] != "" {
		t.Fatalf("err=%v armed=%v, want a plain failure with nothing armed", res.Err, m.armedLog)
	}
}
