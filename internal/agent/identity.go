package agent

import "context"

// Identity names the agent a turn runs for: a subagent's task id and its
// addressable name ("" when it was spawned without one). The main
// conversation's turns carry no Identity.
//
// It rides on the context a subagent's loop runs under, so the tools that
// loop dispatches (SendMessage) know which agent is calling without the
// dispatcher, which is built before the task exists, having to know.
type Identity struct {
	ID   string
	Name string
}

type identityKey struct{}

// WithIdentity returns ctx carrying who.
func WithIdentity(ctx context.Context, who Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, who)
}

// IdentityFrom returns the Identity ctx carries (ok=false for the main
// conversation).
func IdentityFrom(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	who, ok := ctx.Value(identityKey{}).(Identity)
	return who, ok
}

// From is the sender label of the agent: its name, else its id.
func (i Identity) From() string {
	if i.Name != "" {
		return i.Name
	}
	return i.ID
}
