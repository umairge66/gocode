package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/plugins"
	"github.com/umairge66/gocode/internal/toolrun"
)

// Language servers.
//
// The enabled plugins declare language servers (`.lsp.json` in the plugin
// root, or `lspServers` in plugin.json). With at least one configured the
// session offers the LSP tool, starts each server the first time a file it
// handles is used, keeps the files the agents edit in sync with it, and
// reports the new diagnostics an edit causes to the model. The servers stop
// when the session ends.

// lspShutdownTimeout bounds stopping the language servers at exit.
const lspShutdownTimeout = 5 * time.Second

// lspTranscriptLines caps the diagnostics listed in the transcript per report.
const lspTranscriptLines = 5

// sessionLSP is the session's language-server state (nil without servers).
type sessionLSP struct {
	manager *lsp.Manager
	tracker *lsp.Tracker
}

// buildSessionLSP loads the enabled plugins' language servers. It returns
// nil (and no options) when none is configured, plus the configuration
// problems to show at startup.
func buildSessionLSP(paths sessionPaths, enabled []*plugins.Plugin, lookup func(string) (string, bool), dataDir func(*plugins.Plugin) string) (*sessionLSP, []string) {
	sources := make([]lsp.PluginSource, 0, len(enabled))
	for _, p := range enabled {
		data := ""
		if dataDir != nil {
			data = dataDir(p)
		}
		v := p.Vars(data, paths.cwd)
		v.LookupEnv = lookup
		sources = append(sources, lsp.PluginSource{Plugin: p, Vars: v})
	}
	cfg, errs := lsp.LoadPluginServers(fsfacade.OSFS{}, sources)
	var notices []string
	for _, e := range errs {
		notices = append(notices, "LSP: "+e.Error())
	}
	if len(cfg) == 0 {
		return nil, notices
	}
	m, warnings := lsp.NewManager(cfg, lsp.Options{Root: paths.cwd, ClientVersion: updateCurrentVersion()})
	for _, w := range warnings {
		if !strings.HasPrefix(w, "LSP") {
			w = "LSP: " + w
		}
		notices = append(notices, w)
	}
	if !m.HasServers() {
		return nil, notices
	}
	return &sessionLSP{manager: m, tracker: lsp.NewTracker(m)}, notices
}

// dispatcherOptions offers the LSP tool and observes the main agent's edits.
func (l *sessionLSP) dispatcherOptions() []toolrun.Option {
	if l == nil {
		return nil
	}
	return []toolrun.Option{toolrun.WithLSP(l.manager), toolrun.WithEditObserver(l.tracker)}
}

// subagentOptions gives a subagent the same servers; its edits are kept in
// sync but their diagnostics are not reported to the main conversation.
func (l *sessionLSP) subagentOptions() []toolrun.Option {
	if l == nil {
		return nil
	}
	return []toolrun.Option{toolrun.WithLSP(l.manager), toolrun.WithEditObserver(l.tracker.SyncOnly())}
}

// bind delivers the diagnostics reminder with the conversation's requests
// and shows each report in the transcript.
func (l *sessionLSP) bind(r *sessionRuntime, cwd string) {
	if l == nil {
		return
	}
	l.tracker.OnReport = func(files []lsp.FileDiagnostics) {
		r.emit(lsp.SummaryText(files, cwd, lspTranscriptLines))
	}
	r.conv.AddContextFeed(l.tracker)
}

// close stops background syncing and the servers.
func (l *sessionLSP) close() {
	if l == nil {
		return
	}
	l.tracker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), lspShutdownTimeout)
	defer cancel()
	_ = l.manager.Shutdown(ctx)
}

// statusValue summarizes the servers for /status.
func (l *sessionLSP) statusValue() string {
	if l == nil {
		return "none configured"
	}
	var parts []string
	for _, s := range l.manager.Status() {
		name := strings.TrimPrefix(s.Name, "plugin:")
		part := fmt.Sprintf("%s (%s", name, s.State)
		if len(s.Extensions) > 0 {
			part += ", " + strings.Join(s.Extensions, " ")
		}
		parts = append(parts, part+")")
	}
	return strings.Join(parts, ", ")
}
