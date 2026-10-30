package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/protocol"
)

// AmendAgent — redirecting an active subagent (phase 3).
//
// Phase 1 built the resumable-subagent engine (Stop keeps a running turn's
// history; Resume continues a Stopped session with a follow-up). Phase 2 wired
// the interactive UI controls to it. Phase 3 gives the MAIN model a tool to
// redirect a running or stopped subagent when the user adds a relevant
// requirement: the model composes a refined, self-contained instruction and
// calls AmendAgent, and the target agent — which KEEPS its conversation and
// remembers its prior work — continues with that instruction.

// AmendAgentToolName is the model-facing tool name.
const AmendAgentToolName = string(protocol.ToolAmendAgent)

// amendPollInterval / amendPollMax bound the brief wait between Stop and Resume
// when the target is running: Stop cancels the running turn, whose goroutine
// then records StatusStopped and clears the running flag; Resume is valid only
// once the turn is no longer running. Phase 1 exposes no synchronous
// StopThenResume, so the sequencing is done here by polling the session status.
const (
	amendPollInterval = 5 * time.Millisecond
	amendPollMax      = 10 * time.Second
)

// Tool-result / prompt text.
const (
	amendInstructionRequired = "instruction is required: compose a complete, self-contained instruction for the agent to continue with (it keeps its prior context — do not restate the whole task, but give a full, unambiguous directive, not the user's raw words)."

	amendNoActiveAgents = "There are no active subagents to amend right now. If you need the work done, launch a new agent with the Agent tool."

	// amendDescription is the Agent-tool-style prompt for AmendAgent.
	amendDescription = "Redirect a RUNNING or STOPPED subagent that you previously launched, giving it an additional or revised instruction. The agent keeps its full conversation and memory of prior work and CONTINUES from where it is — it does not restart.\n\n" +
		"## When to use\n\n" +
		"Use AmendAgent when the user adds a requirement, correction, or clarification that belongs to work an existing subagent is already doing. For example: a subagent is building \"user CRUD in Laravel\" and the user now says \"also add a phone number field\" — that belongs to the running agent, so amend it rather than starting over.\n\n" +
		"**Prefer AmendAgent over launching a new agent** whenever the new work belongs to an agent that is still active. A fresh Agent call has no memory of the prior run and would duplicate or conflict with it; amending continues the same agent with its context intact.\n\n" +
		"## Writing the instruction\n\n" +
		"Compose a COMPLETE, self-contained, refined instruction for the agent — do NOT forward the user's raw words. The user speaks to you, in shorthand, about the whole conversation; the agent only knows its own task. Translate the user's intent into a precise directive the agent can act on with the context it already has:\n" +
		"- State exactly what to add, change, or fix, in concrete terms (file, field, behavior, acceptance criterion).\n" +
		"- Fit it to what the agent is already building, so it extends its work rather than second-guessing it.\n" +
		"- Keep it to the new/changed requirement; the agent still remembers everything it has done, so do not re-dump the original task.\n\n" +
		"## How it behaves\n\n" +
		"- If the agent is still running, it is interrupted immediately (its progress and history are preserved) and then continued with your instruction.\n" +
		"- If the agent is already stopped, it is continued directly.\n" +
		"- The agent runs the continuation in the background; you will be notified when it finishes, exactly as for a background launch. Do not poll or predict its result.\n\n" +
		"Call it with the agent's id (the agentId from its launch result, also listed in the active-subagents system-reminder) and the refined instruction. If the id is not an active agent, the tool returns the list of agents you can amend so you can retry with the right one."

	amendInstructionParamDesc = "The COMPLETE, self-contained instruction for the agent to continue with. Compose a refined full directive in your own words — do NOT forward the user's raw message. The agent retains its prior context and continues from where it is, so give it a precise, actionable instruction (what to add/change/fix and how it fits the work it is already doing), not a restatement of the whole task and not the user's shorthand."

	amendAgentIDParamDesc = "The id of the active (running or stopped) subagent to redirect — the agentId from the agent's launch result, or from the active-subagents system-reminder. It must be a currently active agent; a finished or unknown id returns an error listing the agents you can amend."
)

// ActiveAgentInfo is a concise snapshot of one active (running or stopped)
// subagent, used both for the AmendAgent error listing and by the host to build
// the active-subagents system-reminder.
type ActiveAgentInfo struct {
	ID       string
	Type     string
	Activity string
	Running  bool
}

// ActiveAgents returns a snapshot of every currently active (running or
// stopped) subagent spawned through this tool, sorted by id for stable output.
// Type/Activity are read from the task registry; Running reflects whether a
// turn is in flight.
func (t *AgentTool) ActiveAgents() []ActiveAgentInfo {
	mgr := t.sessionManager()
	ids := mgr.ActiveIDs()
	sort.Strings(ids)
	out := make([]ActiveAgentInfo, 0, len(ids))
	for _, id := range ids {
		info := ActiveAgentInfo{ID: id}
		if sess, ok := mgr.Session(id); ok {
			info.Type = sess.AgentType()
			info.Running = sess.Running()
		}
		if t.Tasks != nil {
			if tk, ok := t.Tasks.Get(id); ok {
				info.Activity = tk.Activity
				if info.Type == "" {
					info.Type = tk.AgentType
				}
			}
		}
		out = append(out, info)
	}
	return out
}

// activeAgentLines renders the active agents as model-facing listing lines:
//
//   - <id> (<type>): <running|stopped> — <activity>
func (t *AgentTool) activeAgentLines() []string {
	infos := t.ActiveAgents()
	lines := make([]string, 0, len(infos))
	for _, in := range infos {
		typ := in.Type
		if typ == "" {
			typ = "subagent"
		}
		state := "stopped"
		if in.Running {
			state = "running"
		}
		line := fmt.Sprintf("- %s (%s): %s", in.ID, typ, state)
		if act := strings.TrimSpace(in.Activity); act != "" {
			line += " — " + act
		}
		lines = append(lines, line)
	}
	return lines
}

// unknownAgentError builds the model-facing error for an id that is not an
// active session, listing the agents that CAN be amended so the model can
// retry with the right id.
func (t *AgentTool) unknownAgentError(agentID string) error {
	lines := t.activeAgentLines()
	if len(lines) == 0 {
		return fmt.Errorf("No active subagent with id %q. %s", agentID, amendNoActiveAgents)
	}
	return fmt.Errorf("No active subagent with id %q. Active subagents you can amend (pass one of these ids):\n%s", agentID, strings.Join(lines, "\n"))
}

// AmendAgent redirects an active subagent with a refined instruction. It
// validates that agentID is an ACTIVE session (else it returns the active-agent
// listing error); if the session is running it Stops it first (an immediate
// interrupt, history preserved), then Resumes it with the instruction.
//
// Because Resume blocks for a whole turn, the Stop→Resume sequence runs on a
// goroutine (like a background launch) and AmendAgent returns immediately with
// the "amended; continuing in the background" result. The resumed turn's
// completion fires OnResumeDone → OnSubagentUpdate (phases 1/2), so the UI
// updates and the model is notified, exactly as for a background spawn.
//
// In a non-interactive host (-p / headless) no completion notifier is wired;
// there the goroutine would outlive the one-shot process, so the Stop→Resume is
// run SYNCHRONOUSLY instead and AmendAgent returns once the continuation turn
// has finished.
func (t *AgentTool) AmendAgent(ctx context.Context, agentID, instruction string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return "", errors.New(amendInstructionRequired)
	}
	mgr := t.sessionManager()
	sess, ok := mgr.Session(agentID)
	if !ok {
		return "", t.unknownAgentError(agentID)
	}
	agentType := sess.AgentType()
	running := sess.Running()

	// Headless (-p): no notifier means nobody consumes an async completion and
	// the process is one-shot, so run the sequence synchronously and report the
	// finished continuation.
	if t.OnSubagentUpdate == nil && t.OnBackgroundDone == nil {
		if err := t.stopThenResume(ctx, agentID, instruction, running); err != nil {
			return "", err
		}
		return formatAmendedSync(agentType), nil
	}

	// Interactive: continue in the background and return at once.
	bg := context.WithoutCancel(ctx)
	go func() { _ = t.stopThenResume(bg, agentID, instruction, running) }()
	return formatAmended(agentType), nil
}

// stopThenResume performs the ordered Stop (if running) → wait-until-idle →
// Resume with the instruction. When the target is already stopped it resumes
// directly. A Stop that races a Close (the session disappears) is reported so a
// synchronous caller sees why nothing happened.
func (t *AgentTool) stopThenResume(ctx context.Context, agentID, instruction string, running bool) error {
	mgr := t.sessionManager()
	if running {
		if err := mgr.Stop(agentID); err != nil {
			return err
		}
		if !t.waitStopped(agentID) {
			return fmt.Errorf("subagent %q did not stop in time to be amended", agentID)
		}
	}
	return mgr.Resume(ctx, agentID, instruction)
}

// waitStopped polls until the session's running turn has aborted (so Resume is
// valid) or the session is gone. It returns true once the session is idle and
// still active, false if it vanished (closed/finished) or never settled.
func (t *AgentTool) waitStopped(agentID string) bool {
	mgr := t.sessionManager()
	deadline := time.Now().Add(amendPollMax)
	for {
		sess, ok := mgr.Session(agentID)
		if !ok {
			return false
		}
		if !sess.Running() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(amendPollInterval)
	}
}

func formatAmended(agentType string) string {
	return fmt.Sprintf("Amended the %s agent; it will continue in the background with your instruction. You'll be notified when it finishes.", amendAgentLabel(agentType))
}

func formatAmendedSync(agentType string) string {
	return fmt.Sprintf("Amended the %s agent; it continued with your instruction and has finished.", amendAgentLabel(agentType))
}

func amendAgentLabel(agentType string) string {
	if strings.TrimSpace(agentType) == "" {
		return "subagent"
	}
	return agentType
}

// AmendTool is the model-facing AmendAgent tool. It is a thin front for
// AgentTool.AmendAgent that satisfies the toolrun ModelTool interface
// (Name/Description/InputSchema/Execute), mirroring how the Agent and Skill
// tools are wired. Construct it with the owning *AgentTool.
type AmendTool struct {
	// Agent is the live Agent tool whose session manager AmendAgent drives.
	Agent *AgentTool
}

// NewAmendTool returns an AmendAgent tool over the given Agent tool.
func NewAmendTool(a *AgentTool) *AmendTool { return &AmendTool{Agent: a} }

// Name returns "AmendAgent".
func (t *AmendTool) Name() string { return AmendAgentToolName }

// Description returns the model-facing tool prompt.
func (t *AmendTool) Description() string { return amendDescription }

// AmendInput is the decoded AmendAgent tool input.
type AmendInput struct {
	AgentID     string `json:"agent_id"`
	Instruction string `json:"instruction"`
}

// InputSchema returns the JSON schema: agent_id and instruction, both required.
func (t *AmendTool) InputSchema() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent_id":    map[string]any{"type": "string", "description": amendAgentIDParamDesc},
			"instruction": map[string]any{"type": "string", "description": amendInstructionParamDesc},
		},
		"required":             []string{"agent_id", "instruction"},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// Execute runs one AmendAgent call. A returned error is a tool-level failure
// whose message the model sees (e.g. the active-agent listing for a bad id).
func (t *AmendTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Agent == nil {
		return "", errors.New("AmendAgent tool is not configured (no Agent tool)")
	}
	var in AmendInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("invalid AmendAgent input: %v", err)
	}
	return t.Agent.AmendAgent(ctx, in.AgentID, in.Instruction)
}

// Static assertion: AmendTool is wired exactly like the Agent/Skill model tools.
var _ interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(context.Context, json.RawMessage) (string, error)
} = (*AmendTool)(nil)
