package ui

import "testing"

// A system line added while a reply is still streaming does not split the
// reply: the final render updates the streaming message in place instead of
// adding the reply a second time.
func TestStreamFinalizeAfterSystemLine(t *testing.T) {
	vs := ViewState{}
	vs = Reduce(vs, StreamAssistant{Text: "Added div"})
	vs = Reduce(vs, AppendMessage{Message{Role: RoleSystem, Text: "Agent Plan finished."}})
	vs = Reduce(vs, StreamAssistant{Text: "Added div(a, b).", Done: true})
	if len(vs.Transcript) != 2 {
		t.Fatalf("transcript = %+v, want the reply and the system line", vs.Transcript)
	}
	if r := vs.Transcript[0]; r.Role != RoleAssistant || r.Text != "Added div(a, b)." || r.Streaming {
		t.Fatalf("reply = %+v", r)
	}

	// Once a user turn follows, a new reply starts a new message.
	vs = Reduce(vs, AppendMessage{Message{Role: RoleUser, Text: "next"}})
	vs = Reduce(vs, StreamAssistant{Text: "second", Done: true})
	if n := len(vs.Transcript); n != 4 || vs.Transcript[3].Text != "second" {
		t.Fatalf("transcript = %+v", vs.Transcript)
	}
}
