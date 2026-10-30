package agenttool

import (
	"context"

	"github.com/umairge66/gocode/internal/inbox"
	"github.com/umairge66/gocode/internal/subagent"
)

// SendMessage's view of the agents spawned through the Agent tool: a running
// agent takes a message at its next tool round, a finished one is resumed
// with it, one the user stopped is not resumed.

// MessageDirectory returns the Agent tool's agents as SendMessage reaches them.
func (t *AgentTool) MessageDirectory() inbox.AgentDirectory { return agentDirectory{t: t} }

type agentDirectory struct{ t *AgentTool }

func (d agentDirectory) LookupAgent(key string) (inbox.AgentTarget, bool) {
	reg := d.t.Tasks
	if reg == nil || key == "" {
		return inbox.AgentTarget{}, false
	}
	task, ok := reg.Get(key)
	if !ok {
		task, ok = reg.ByName(key)
	}
	if !ok {
		return inbox.AgentTarget{}, false
	}
	target := inbox.AgentTarget{ID: task.ID, Name: key, Status: string(task.Status)}
	switch {
	case task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending || task.Status == subagent.StatusQueued:
		target.State = inbox.AgentLive
	case task.StoppedByUser || task.Status == subagent.StatusStopped:
		target.State = inbox.AgentStoppedByUser
	default:
		target.State = inbox.AgentStopped
	}
	return target, true
}

func (d agentDirectory) AgentNames() []string {
	if d.t.Tasks == nil {
		return nil
	}
	return d.t.Tasks.Names()
}

func (d agentDirectory) QueueAgentMessage(id string, m inbox.AgentMessage) (bool, bool) {
	status, ok := d.t.Tasks.QueueMessage(id, m)
	if !ok {
		return false, false
	}
	running := status == subagent.StatusRunning || status == subagent.StatusPending || status == subagent.StatusQueued
	if !running {
		// Stranded: the agent finished before the message could be
		// delivered, so it resumes the agent.
		go d.sessions().ReviveStranded(context.Background(), id)
	}
	return running, true
}

func (d agentDirectory) ResumeAgent(ctx context.Context, id string, m inbox.AgentMessage) error {
	return d.sessions().Revive(ctx, id, m)
}

// sessions is the shared session manager (read without the launch lock a
// background spawn holds while its first model call starts).
func (d agentDirectory) sessions() *subagent.SessionManager {
	if d.t.sessions != nil {
		return d.t.sessions
	}
	return d.t.sessionManager()
}
