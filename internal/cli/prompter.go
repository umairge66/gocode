package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/settingsload"
	"github.com/umairge66/gocode/internal/settingspolicy"
	"github.com/umairge66/gocode/internal/toolreg"
	"github.com/umairge66/gocode/internal/toolrun"
	"github.com/umairge66/gocode/internal/ui"
)

// This file wires the interactive permission prompt: the concrete
// toolrun.PermissionPrompter that drives the ui permission Dialog, mirroring the
// dialogAsker seam. When a tool call resolves to a permission "ask", the toolrun
// dispatcher calls dialogPrompter.Prompt, which opens the
// "Do you want to …?" dialog, blocks until the user answers (Enter) or dismisses
// (esc/ctrl+c = deny), and maps the choice to a toolrun.PermissionResponse:
//
//	Yes                                       -> allow this one call
//	Yes, and don't ask again                  -> persist a rule + allow always
//	Yes, allow all edits during this session  -> switch to acceptEdits (edit tools)
//	No, and tell gocode what to do differently -> deny, with the typed reason
//
// The "don't ask again" branch persists the suggested rule string to the local
// project settings (.gocode/settings.local.json, permissions.allow) through
// internal/settingsload and returns the compiled rule so toolrun also adds it to
// the live session rule set. There is NO telemetry here.

// dialogPrompter is the ui-driven toolrun.PermissionPrompter. It shares the
// dialogReply / submit / cancel mechanism with dialogAsker: the interactive
// wiring routes the model's OnDialogSubmit / OnDialogCancel to BOTH, and each
// delivers only when it has a pending prompt. toolrun serializes permission
// prompts (and an AskUserQuestion dialog opens only after a permission prompt
// has closed), so at most one of the two is ever pending at a time.
type dialogPrompter struct {
	mu      sync.Mutex
	send    func(tea.Msg)
	pending chan dialogReply

	// gate serializes the DISPLAY of prompts across every dispatcher that shares
	// this prompter (the main dispatcher and each subagent's child dispatcher).
	// Each dispatcher has its own toolrun promptMu, so without this a second
	// concurrent caller would overwrite `pending` and strand the first. A
	// buffered-size-1 channel is a context-aware mutex: callers queue, and a
	// caller whose turn is interrupted abandons the wait instead of blocking.
	gate chan struct{}

	// persist writes a granted "don't ask again" rule to the user's settings.
	// Nil skips persistence (the rule still takes effect for the session via the
	// live rule set). Injected by the wiring (captures the settings context) so a
	// test can supply a fake.
	persist func(ruleString string) error

	// status reports the open prompt into a background job's state (nil
	// outside a background worker).
	status *jobStatus
}

// newDialogPrompter returns an unbound prompter. persist may be nil.
func newDialogPrompter(persist func(string) error) *dialogPrompter {
	return &dialogPrompter{
		send:    func(tea.Msg) {},
		gate:    make(chan struct{}, 1),
		persist: persist,
	}
}

// childPrompter returns a toolrun.PermissionPrompter that presents the SAME
// dialog on the SAME (shared) main program, but attributes the request to a
// subagent with a leading note line (e.g. "Subagent request"). All serialization
// and reply state stays on the shared dialogPrompter, so subagent prompts queue
// behind the main turn's prompts and each other.
func (p *dialogPrompter) childPrompter(note string) toolrun.PermissionPrompter {
	return attributedPrompter{inner: p, note: note}
}

// attributedPrompter wraps a dialogPrompter to label a prompt with its origin.
type attributedPrompter struct {
	inner *dialogPrompter
	note  string
}

func (a attributedPrompter) Prompt(ctx context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	return a.inner.prompt(ctx, req, a.note)
}

var _ toolrun.PermissionPrompter = attributedPrompter{}

func (p *dialogPrompter) bind(send func(tea.Msg)) {
	p.mu.Lock()
	p.send = send
	p.mu.Unlock()
}

// submit / cancel are the ui.Model dialog seams, mirroring dialogAsker.
func (p *dialogPrompter) submit(values []string) { p.deliver(dialogReply{values: values}) }
func (p *dialogPrompter) cancel()                { p.deliver(dialogReply{cancelled: true}) }

func (p *dialogPrompter) deliver(r dialogReply) {
	p.mu.Lock()
	ch := p.pending
	p.pending = nil
	p.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// Prompt presents the permission dialog and maps the user's choice to a
// PermissionResponse. A dismissed dialog (esc / ctrl+c) is a denial without
// feedback; a cancelled context (interrupted turn) returns ctx.Err().
func (p *dialogPrompter) Prompt(ctx context.Context, req toolrun.PermissionRequest) (toolrun.PermissionResponse, error) {
	return p.prompt(ctx, req, "")
}

// prompt presents the permission dialog with an optional attribution note (""
// for the main turn, e.g. "Subagent" for a subagent). It holds the shared gate
// for the whole exchange so concurrent callers (the main dispatcher and any
// subagent child dispatchers, each with its own toolrun promptMu) queue instead
// of racing on `pending`. The gate wait is context-aware, so an interrupted
// subagent turn abandons the queue rather than deadlocking.
func (p *dialogPrompter) prompt(ctx context.Context, req toolrun.PermissionRequest, note string) (toolrun.PermissionResponse, error) {
	select {
	case <-ctx.Done():
		return toolrun.PermissionResponse{}, ctx.Err()
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	}

	detail := permissionDetail(req)
	if req.Notice != "" {
		// Why the prompt is shown: a read outside the working directories, or
		// the auto mode classifier handing the call to the user.
		detail = append(detail, req.Notice)
	}
	if req.Origin != "" {
		// A `!`-block of a custom slash command: name the command the shell
		// comes from (it runs before the prompt reaches the model).
		detail = append([]string{"Shell block in slash command " + req.Origin}, detail...)
	}
	if note != "" {
		// Prepend the origin so the user can attribute the request (e.g. a
		// background subagent) before deciding.
		detail = append([]string{note + " request"}, detail...)
	}
	dlg := ui.PermissionPromptDialog(permissionTitle(req), allowAlwaysLabel(req), req.IsEdit, detail...)
	if req.NoAlwaysAllow {
		// allowManagedPermissionRulesOnly: only managed rules count, so the
		// "Yes, and don't ask again" option is not offered.
		dlg.Options = withoutOption(dlg.Options, ui.PermAllowAlways)
	}

	ch := make(chan dialogReply, 1)
	p.mu.Lock()
	p.pending = ch
	send := p.send
	p.mu.Unlock()

	p.status.setBlock(permissionBlockText(req.ToolName, req.Input))
	defer p.status.clearBlock()

	send(ui.OpenDialogMsg{Dialog: dlg})

	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.pending = nil
		p.mu.Unlock()
		send(ui.CloseDialogMsg{})
		return toolrun.PermissionResponse{}, ctx.Err()
	case r := <-ch:
		if r.cancelled {
			// esc / ctrl+c: deny, no feedback.
			return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied}, nil
		}
		return p.decide(req, dlg, firstValue(r.values)), nil
	}
}

// decide maps the confirmed option value to a PermissionResponse.
func (p *dialogPrompter) decide(req toolrun.PermissionRequest, dlg *ui.Dialog, value string) toolrun.PermissionResponse {
	switch value {
	case ui.PermAllowOnce:
		return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedOnce}
	case ui.PermAllowEdits:
		return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedEditsSession}
	case ui.PermAllowAlways:
		if req.NoAlwaysAllow {
			// Never offered under allowManagedPermissionRulesOnly; if it is
			// chosen anyway it allows this one call and saves nothing.
			return toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedOnce}
		}
		ruleStr := firstSuggested(req)
		if ruleStr != "" && p.persist != nil && !req.SessionOnly {
			// Best-effort: a failed save still grants the rule for this session.
			_ = p.persist(ruleStr)
		}
		source := string(settingspolicy.SourceLocal)
		if req.SessionOnly {
			// A session rule: live only.
			source = "session"
		}
		rule, ok := parsePermissionRule(ruleStr, toolreg.RuleAllow, source)
		resp := toolrun.PermissionResponse{Kind: toolrun.PermissionAllowedAlways}
		if ok {
			resp.Rule = &rule
		}
		return resp
	default: // ui.PermDeny
		return toolrun.PermissionResponse{Kind: toolrun.PermissionDenied, Feedback: dlg.Feedback}
	}
}

// firstValue returns the first chosen option value, or "" (treated as deny).
func firstValue(values []string) string {
	if len(values) == 0 {
		return ui.PermDeny
	}
	return values[0]
}

// firstSuggested returns the most specific suggested rule string.
func firstSuggested(req toolrun.PermissionRequest) string {
	if len(req.Suggested) > 0 {
		return req.Suggested[0]
	}
	return ""
}

// allowAlwaysLabel builds the "don't ask again" option label, naming the rule
// that would be persisted when one is known.
func allowAlwaysLabel(req toolrun.PermissionRequest) string {
	if req.AlwaysAllowLabel != "" {
		return req.AlwaysAllowLabel
	}
	if r := firstSuggested(req); r != "" {
		return "Yes, and don't ask again (" + r + ")"
	}
	return "Yes, and don't ask again"
}

// permissionTitle renders the "Do you want to …?" question per tool.
func permissionTitle(req toolrun.PermissionRequest) string {
	switch req.Tool {
	case protocol.ToolBash:
		return "Do you want to proceed?"
	case protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolNotebookEdit:
		if f := inputField(req.Input, "file_path", "notebook_path"); f != "" {
			return "Do you want to make this edit to " + f + "?"
		}
		return "Do you want to make this edit?"
	case protocol.ToolWrite:
		f := inputField(req.Input, "file_path")
		verb := "write to"
		if f != "" {
			if _, err := os.Stat(f); err != nil {
				verb = "create"
			} else {
				verb = "overwrite"
			}
			return "Do you want to " + verb + " " + f + "?"
		}
		return "Do you want to write this file?"
	case protocol.ToolWebFetch:
		return "Do you want to allow gocode to fetch this content?"
	default:
		return "Do you want to proceed?"
	}
}

// permissionDetail renders the per-tool detail body lines shown under the title:
// the Bash command, the edited file path, the WebFetch domain, or the MCP
// server/tool label.
func permissionDetail(req toolrun.PermissionRequest) []string {
	var lines []string
	switch req.Tool {
	case protocol.ToolBash:
		if c := inputField(req.Input, "command"); c != "" {
			lines = append(lines, "Bash command", c)
		}
	case protocol.ToolWrite, protocol.ToolEdit, protocol.ToolMultiEdit, protocol.ToolNotebookEdit:
		if f := inputField(req.Input, "file_path", "notebook_path"); f != "" {
			lines = append(lines, f)
		}
	case protocol.ToolWebFetch:
		if host := strings.TrimPrefix(req.RuleContent, "domain:"); host != "" && host != req.RuleContent {
			lines = append(lines, host)
		} else if u := inputField(req.Input, "url"); u != "" {
			lines = append(lines, u)
		}
	default:
		if strings.HasPrefix(req.ToolName, "mcp__") {
			server, tool := mcpServerTool(req.ToolName)
			lines = append(lines, server+"/"+tool)
		} else {
			lines = append(lines, req.ToolName)
		}
		// A browser call asking for a site (gocodeInChromeDomain rule): name it.
		if strings.HasPrefix(firstSuggested(req), "gocodeInChromeDomain(") && req.RuleContent != "" {
			lines = append(lines, "gocode in Chrome on "+req.RuleContent)
		}
	}
	if len(lines) == 0 && req.RuleContent != "" {
		lines = append(lines, req.RuleContent)
	}
	return lines
}

// inputField pulls the first present string field from the raw tool input.
func inputField(input json.RawMessage, keys ...string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// mcpServerTool splits "mcp__server__tool" into its server and tool parts.
func mcpServerTool(name string) (server, tool string) {
	rest := strings.TrimPrefix(name, "mcp__")
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[:i], rest[i+2:]
	}
	return rest, name
}

// localSettingsPersist returns a persist func that appends a granted rule to the
// local project settings (.gocode/settings.local.json, permissions.allow),
// resolving the settings context from the environment.
func localSettingsPersist(env *Env) func(string) error {
	return func(ruleString string) error {
		// Under allowManagedPermissionRulesOnly no rule is saved ("enterprise
		// managed settings allow only managed permission rules; rules cannot
		// be added or removed").
		if managedPermissionRulesOnly(env) {
			return errManagedRulesOnly
		}
		_, err := settingsload.AddPermissionRule(settingsContext(env), settingspolicy.SourceLocal, settingsload.PermissionAllow, ruleString)
		return err
	}
}

// errManagedRulesOnly refuses a permission-rule write under the policy's
// allowManagedPermissionRulesOnly.
var errManagedRulesOnly = errors.New("enterprise managed settings allow only managed permission rules; rules cannot be added or removed")

var _ toolrun.PermissionPrompter = (*dialogPrompter)(nil)

// withoutOption returns opts without the option whose Value is value.
func withoutOption(opts []ui.DialogOption, value string) []ui.DialogOption {
	out := make([]ui.DialogOption, 0, len(opts))
	for _, o := range opts {
		if o.Value != value {
			out = append(out, o)
		}
	}
	return out
}
