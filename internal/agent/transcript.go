package agent

import "context"

// The running transcript rides on the context a tool round is dispatched
// under, so a permission check that needs the conversation so far — the auto
// mode safety classifier — can read it without the dispatcher (built before
// any turn runs) holding a reference to the loop.

type transcriptKey struct{}

// WithTranscript returns ctx carrying the messages of the running turn, up to
// and including the assistant message whose tool calls are being dispatched.
func WithTranscript(ctx context.Context, msgs []Message) context.Context {
	return context.WithValue(ctx, transcriptKey{}, msgs)
}

// TranscriptFrom returns the transcript ctx carries (nil when none).
func TranscriptFrom(ctx context.Context) []Message {
	if ctx == nil {
		return nil
	}
	msgs, _ := ctx.Value(transcriptKey{}).([]Message)
	return msgs
}
