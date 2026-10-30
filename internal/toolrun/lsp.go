package toolrun

import (
	"context"
	"encoding/json"
	"os"

	"github.com/umairge66/gocode/internal/lsp"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

// EditObserver is told about each Write / Edit / MultiEdit / NotebookEdit
// call: BeforeFileEdit just before an allowed call runs, AfterFileEdit once
// it succeeded. The language-server diagnostics tracker (lsp.Tracker) is the
// observer: it snapshots the file's diagnostics before the edit and sends
// the new content to the file's server after it.
type EditObserver interface {
	BeforeFileEdit(path string)
	AfterFileEdit(path string)
}

// WithLSP offers the LSP tool over the session's language servers. The tool
// is offered only when the manager has at least one server configured.
func WithLSP(m *lsp.Manager) Option {
	return func(d *Dispatcher) { d.LSP = m }
}

// WithEditObserver installs the file-edit observer.
func WithEditObserver(o EditObserver) Option {
	return func(d *Dispatcher) { d.SetEditObserver(o) }
}

// SetEditObserver installs (or, with nil, removes) the file-edit observer.
func (d *Dispatcher) SetEditObserver(o EditObserver) {
	d.stateMu.Lock()
	d.editObserver = o
	d.stateMu.Unlock()
}

func (d *Dispatcher) observer() EditObserver {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.editObserver
}

// LSPManager returns the language-server manager the LSP tool uses (nil when
// none), so a subagent's dispatcher can share it.
func (d *Dispatcher) LSPManager() *lsp.Manager { return d.LSP }

// registerLSPTool registers the LSP tool when a server is configured.
func (d *Dispatcher) registerLSPTool() {
	if !d.LSP.Enabled() {
		delete(d.adapters, protocol.ToolLSP)
		return
	}
	d.Register(protocol.ToolLSP, LSPAdapter(lsp.Tool{Manager: d.LSP, Cwd: d.WorkDir}))
}

// LSPAdapter adapts the LSP tool. It is a read tool: the rule content and
// path are the file operated on (Read rules and the working-directory check
// apply, as for Read).
func LSPAdapter(t lsp.Tool) adapter {
	cwd := func() string {
		if t.Cwd != "" {
			return t.Cwd
		}
		wd, _ := os.Getwd()
		return wd
	}
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			in, err := lsp.ParseInput(input)
			if err != nil {
				return "", "", "", err
			}
			return in.FilePath, lsp.ResolvePath(in.FilePath, cwd()), pathguard.OpRead, nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			in, err := lsp.ParseInput(input)
			if err != nil {
				return "", err
			}
			return t.Execute(ctx, in)
		},
		description: lsp.ToolDescription,
		inputSchema: lsp.InputSchema,
	}
}

// beginFileEdit tells the observer an allowed file-modifying call is about to
// run and returns the function to call once it succeeded (nil when there is
// nothing to observe).
func (d *Dispatcher) beginFileEdit(tool protocol.Tool, ad adapter, input json.RawMessage, path string, rewritten bool) func() {
	if !fileEditTools[tool] {
		return nil
	}
	o := d.observer()
	if o == nil {
		return nil
	}
	target := d.editTarget(ad, input, path, rewritten)
	if target == "" {
		return nil
	}
	o.BeforeFileEdit(target)
	return func() { o.AfterFileEdit(target) }
}
