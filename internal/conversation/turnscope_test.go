package conversation

import (
	"context"
	"reflect"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

// orderModel registers turn-scoped undo funcs while the turn runs.
type orderModel struct {
	c     *Conversation
	order *[]string
}

func (m orderModel) CallModel(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
	m.c.DeferTurnEnd(func() { *m.order = append(*m.order, "first") })
	m.c.DeferTurnEnd(func() { *m.order = append(*m.order, "second") })
	return agent.ModelResponse{Message: agent.AssistantMessage(agent.TextBlock("ok")), StopReason: agent.StopEndTurn}, nil
}

// Funcs deferred during a turn run when it ends, most recent first, exactly
// once.
func TestDeferTurnEndRunsLIFOAtTurnEnd(t *testing.T) {
	var order []string
	c := New(nil, fakeDispatcher{})
	c.Builtins = nil
	c.Model = orderModel{c: c, order: &order}
	if _, err := c.Submit(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"second", "first"}) {
		t.Fatalf("order = %v", order)
	}
	c.Model = &fakeModel{}
	if _, err := c.Submit(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 {
		t.Fatalf("undo funcs ran again on the next turn: %v", order)
	}
}
