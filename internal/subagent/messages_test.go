package subagent

import (
	"context"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

type ctxKey struct{}

// OnMessage's observer gets the task id, the event and the turn's context;
// nil removes it.
func TestOnMessageObserver(t *testing.T) {
	r := NewTaskRegistry()
	var got []string
	r.OnMessage(func(ctx context.Context, id string, ev agent.MessageEvent) {
		got = append(got, id+":"+ev.Message.Role+":"+ctx.Value(ctxKey{}).(string))
	})
	ctx := context.WithValue(context.Background(), ctxKey{}, "call_1")
	r.observeMessage(ctx, "a1", agent.MessageEvent{Message: agent.Message{Role: agent.RoleAssistant}})
	r.OnMessage(nil)
	r.observeMessage(ctx, "a1", agent.MessageEvent{Message: agent.Message{Role: agent.RoleUser}})
	if len(got) != 1 || got[0] != "a1:assistant:call_1" {
		t.Fatalf("observed %v", got)
	}
}
