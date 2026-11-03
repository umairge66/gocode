package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/headless"
	"github.com/umairge66/gocode/internal/provider"
	"github.com/umairge66/gocode/internal/subagent"
)

// errInterrupted is the error of a turn an interrupt control request ended.
var errInterrupted = errors.New("Request interrupted by user")

// wireEvents emits the session's system records: compact_boundary on every
// compaction, api_retry before each retried model request, and
// task_started / task_notification for background commands and subagents.
func (h *headlessRun) wireEvents() {
	conv := h.rt.conv
	conv.OnCompactBoundary = func(trigger compaction.CompactTrigger, preTokens int) {
		h.emit(headless.NewCompactBoundary(h.rt.currentSessionID(), string(trigger), preTokens))
	}

	// Retries keep their stderr note and add the api_retry record.
	base := h.s.observer()
	h.s.setRetryObserver(retryObserver{
		onRetry: func(attempt, max int, delay time.Duration, err error) {
			if base.onRetry != nil {
				base.onRetry(attempt, max, delay, err)
			}
			status := 0
			var apiErr *provider.APIError
			if errors.As(err, &apiErr) {
				status = apiErr.Status
			}
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			h.emit(headless.NewAPIRetry(h.rt.currentSessionID(), attempt, max, delay.Milliseconds(), status, msg))
		},
		onStart: base.onStart,
	})

	if bg := conv.BackgroundTasks; bg != nil {
		bg.SetOnStart(func(info bgtask.TaskInfo) {
			if info.Owner != "" {
				return // a subagent's command is not the session's task
			}
			ev := headless.NewTaskStarted(h.rt.currentSessionID(), info.ID, headless.TaskTypeLocalBash, info.Label())
			ev.ToolUseID, ev.IsBackgrounded = info.ToolUseID, true
			h.emit(ev)
		})
		h.rt.setBackgroundObserver(func(info bgtask.TaskInfo) {
			if info.Owner != "" {
				return
			}
			ev := headless.NewTaskNotification(h.rt.currentSessionID(), info.ID, bashTaskStatus(info.Status), info.OutputFile, info.Summary())
			ev.ToolUseID = info.ToolUseID
			h.emit(ev)
		})
	}
	if h.rt.tasks != nil {
		var mu sync.Mutex
		running := map[string]bool{}
		h.rt.tasks.OnStatusChange(func(t *subagent.Task) {
			mu.Lock()
			wasRunning := running[t.ID]
			running[t.ID] = t.Status == subagent.StatusRunning
			mu.Unlock()
			switch {
			case t.Status == subagent.StatusRunning && !wasRunning:
				ev := headless.NewTaskStarted(h.rt.currentSessionID(), t.ID, headless.TaskTypeLocalAgent, t.Description)
				ev.IsBackgrounded, ev.SpawnDepth, ev.ParentTaskID = t.Background, t.Depth, t.ParentID
				h.emit(ev)
			case t.Status.IsTerminal():
				h.emit(headless.NewTaskNotification(h.rt.currentSessionID(), t.ID, agentTaskStatus(t.Status), "", agentTaskSummary(t)))
			}
		})
	}
}

// wirePrompter gives the dispatcher a prompter that lets the host decide
// permission asks: --permission-prompt-tool's MCP tool, or the stream-json
// host. Without either an ask stays a denial.
func (h *headlessRun) wirePrompter() {
	d := h.rt.dispatcher
	if d == nil {
		return
	}
	tool := h.opts.permissionPromptTool
	switch {
	case tool != "" && tool != permissionPromptStdio:
		d.SetPermissionPrompter(mcpPromptTool{tool: tool, call: func(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
			if h.rt.mcp == nil || !h.rt.mcp.Has(name) {
				return "", false, fmt.Errorf("no MCP tool named %s is connected", name)
			}
			return h.rt.mcp.Call(ctx, name, input)
		}})
	case h.in != nil:
		d.SetPermissionPrompter(hostPrompter{in: h.in})
	}
	if h.in != nil {
		// Messages that arrive during a turn ride its next model call.
		h.rt.conv.AddContextFeed(h.in)
	}
	// initialize reports the slash commands; read them now, on the session
	// goroutine.
	var cmds []map[string]any
	for _, sg := range commandSuggestions(h.rt.conv) {
		c := map[string]any{"name": strings.TrimPrefix(sg.Name, "/"), "description": sg.Description, "argumentHint": sg.ArgumentHint}
		cmds = append(cmds, c)
	}
	h.mu.Lock()
	h.initCommands = cmds
	h.mu.Unlock()
}

func bashTaskStatus(s bgtask.Status) string {
	switch s {
	case bgtask.StatusCompleted:
		return headless.TaskCompleted
	case bgtask.StatusKilled:
		return headless.TaskStopped
	}
	return headless.TaskFailed
}

func agentTaskStatus(s subagent.Status) string {
	switch s {
	case subagent.StatusCompleted:
		return headless.TaskCompleted
	case subagent.StatusKilled:
		return headless.TaskStopped
	}
	return headless.TaskFailed
}

// agentTaskSummary is a subagent's one-line outcome.
func agentTaskSummary(t *subagent.Task) string {
	label := strings.TrimSpace(t.Description)
	if label == "" {
		label = t.AgentType
	}
	head := fmt.Sprintf("Agent %q", label)
	switch t.Status {
	case subagent.StatusCompleted:
		return head + " completed"
	case subagent.StatusKilled:
		return head + " was stopped"
	}
	if t.Err != "" {
		return head + " failed: " + t.Err
	}
	return head + " failed"
}
