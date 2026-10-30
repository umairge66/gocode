package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// maxOpenDocuments bounds the documents kept open on the servers; the least
// recently used one beyond it is closed (textDocument/didClose).
const maxOpenDocuments = 50

// Options configure a Manager.
type Options struct {
	// Root is the session workspace root (the servers' working directory and
	// default workspace folder).
	Root string
	// ClientVersion is reported in the initialize request.
	ClientVersion string
	// Spawn starts a server's transport; nil runs the configured command
	// over stdio.
	Spawn Spawner
}

// Diagnostic is one diagnostic as gocode reports it.
type Diagnostic struct {
	Message string `json:"message"`
	// Severity is "Error", "Warning", "Info" or "Hint".
	Severity string `json:"severity"`
	Range    Range  `json:"range"`
	Source   string `json:"source,omitempty"`
	Code     string `json:"code,omitempty"`
}

// FileDiagnostics is the latest published diagnostics of one file.
type FileDiagnostics struct {
	// Path is the file's path (or the URI, for a non-file URI).
	Path        string
	Diagnostics []Diagnostic
	// Version is the document version the server tagged them with, if any.
	Version *int
	// Server is the publishing server.
	Server string
	// Seq increases with every publish the manager records.
	Seq uint64
	// Stale marks a publish for an older document version than the one last
	// sent (seen by OnDiagnostics watchers only; it is not recorded).
	Stale bool
}

// ServerStatus describes one configured server.
type ServerStatus struct {
	Name       string
	Plugin     string
	State      ServerState
	Extensions []string
	LastError  string
}

// Manager owns the configured language servers: it routes files to servers
// by extension, starts a server the first time a file it handles is used,
// keeps documents in sync, and records the diagnostics servers publish.
type Manager struct {
	opts    Options
	servers map[string]*Server
	order   []string
	route   map[string]string // lower-cased extension -> server name

	mu       sync.Mutex
	docs     []string          // open document URIs, least recently used first
	docOwner map[string]string // uri -> server name
	versions map[string]int    // uri -> last version sent
	diags    map[string]*FileDiagnostics
	seq      uint64
	changed  chan struct{} // closed and replaced on every publish
	watchers []func(FileDiagnostics)
	// published names the servers that have published diagnostics.
	published map[string]bool
	closed    bool
}

// NewManager builds a Manager. Invalid server entries are left out; the
// warnings name them and any extension conflicts.
func NewManager(cfg Config, opts Options) (*Manager, []string) {
	if opts.Spawn == nil {
		opts.Spawn = spawnStdio
	}
	if opts.Root == "" {
		opts.Root, _ = filepath.Abs(".")
	}
	m := &Manager{
		opts:      opts,
		servers:   map[string]*Server{},
		docOwner:  map[string]string{},
		versions:  map[string]int{},
		diags:     map[string]*FileDiagnostics{},
		changed:   make(chan struct{}),
		published: map[string]bool{},
	}
	valid := Config{}
	var warnings []string
	for _, name := range sortedKeys(cfg) {
		sc := cfg[name]
		if err := sc.Validate(name); err != nil {
			warnings = append(warnings, fmt.Sprintf("Failed to initialize LSP server %s: %v", name, err))
			continue
		}
		valid[name] = sc
		s := newServer(name, sc, opts.Root, opts.ClientVersion, opts.Spawn)
		s.onNotify = m.onNotify
		s.onStarted = m.serverStarted
		m.servers[name] = s
		m.order = append(m.order, name)
	}
	route, rw := valid.Routing()
	m.route = route
	warnings = append(warnings, rw...)
	return m, warnings
}

// HasServers reports whether any server is configured.
func (m *Manager) HasServers() bool { return m != nil && len(m.servers) > 0 }

// Enabled reports whether the LSP tool is offered: at least one valid server
// is configured. A server that later fails keeps the tool offered; the call
// then reports the failure.
func (m *Manager) Enabled() bool { return m.HasServers() }

// Server returns the server with the given name.
func (m *Manager) Server(name string) *Server { return m.servers[name] }

// ServerFor returns the server that handles a file (by extension), or nil.
func (m *Manager) ServerFor(path string) *Server {
	if m == nil {
		return nil
	}
	name := m.route[strings.ToLower(filepath.Ext(path))]
	if name == "" {
		return nil
	}
	return m.servers[name]
}

// EnsureStarted returns the server for a file, starting it if it is stopped
// or crashed. It returns (nil, nil) when no server handles the file.
func (m *Manager) EnsureStarted(ctx context.Context, path string) (*Server, error) {
	s := m.ServerFor(path)
	if s == nil {
		return nil, nil
	}
	if st := s.State(); st == StateStopped || st == StateError {
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return s, fmt.Errorf("Failed to start LSP server for file %s: the session is ending", path)
		}
		if err := s.Start(ctx); err != nil {
			return s, fmt.Errorf("Failed to start LSP server for file %s: %w", path, err)
		}
	}
	return s, nil
}

// serverStarted forgets the documents a server had open before (re)starting.
func (m *Manager) serverStarted(s *Server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.docs[:0]
	for _, uri := range m.docs {
		if m.docOwner[uri] == s.Name {
			delete(m.docOwner, uri)
			continue
		}
		kept = append(kept, uri)
	}
	m.docs = kept
}

// touch marks a document most recently used and returns the documents
// evicted beyond maxOpenDocuments. Called with m.mu held.
func (m *Manager) touch(uri, server string) []string {
	for i, u := range m.docs {
		if u == uri {
			m.docs = append(m.docs[:i], m.docs[i+1:]...)
			break
		}
	}
	m.docs = append(m.docs, uri)
	m.docOwner[uri] = server
	var evicted []string
	for len(m.docs) > maxOpenDocuments {
		evicted = append(evicted, m.docs[0])
		m.docs = m.docs[1:]
	}
	return evicted
}

// nextVersion bumps a document's version. Called with m.mu held.
func (m *Manager) nextVersion(uri string) int {
	m.versions[uri]++
	return m.versions[uri]
}

// closeEvicted sends didClose for documents pushed out of the open set.
func (m *Manager) closeEvicted(evicted []string) {
	for _, uri := range evicted {
		m.mu.Lock()
		owner := m.docOwner[uri]
		delete(m.docOwner, uri)
		m.mu.Unlock()
		if s := m.servers[owner]; s != nil && s.State() == StateRunning {
			_ = s.Notify("textDocument/didClose", map[string]any{"textDocument": TextDocumentIdentifier{URI: uri}})
		}
	}
}

// OpenFile sends didOpen for a file, starting its server if needed. It does
// nothing when no server handles the file or the file is already open.
func (m *Manager) OpenFile(ctx context.Context, path, text string) error {
	s, err := m.EnsureStarted(ctx, path)
	if err != nil || s == nil {
		return err
	}
	uri := PathToURI(path)
	m.mu.Lock()
	if m.docOwner[uri] == s.Name {
		m.touch(uri, s.Name)
		m.mu.Unlock()
		return nil
	}
	version := m.nextVersion(uri)
	m.mu.Unlock()
	lang := s.Config.LanguageID(strings.ToLower(filepath.Ext(path)))
	if err := s.Notify("textDocument/didOpen", map[string]any{"textDocument": TextDocumentItem{URI: uri, LanguageID: lang, Version: version, Text: text}}); err != nil {
		return fmt.Errorf("Failed to sync file open %s: %w", path, err)
	}
	m.mu.Lock()
	evicted := m.touch(uri, s.Name)
	m.mu.Unlock()
	m.closeEvicted(evicted)
	return nil
}

// ChangeFile sends didChange with the full new text, or opens the file when
// it is not open on a running server.
func (m *Manager) ChangeFile(ctx context.Context, path, text string) error {
	s := m.ServerFor(path)
	if s == nil || s.State() != StateRunning {
		return m.OpenFile(ctx, path, text)
	}
	uri := PathToURI(path)
	m.mu.Lock()
	if m.docOwner[uri] != s.Name {
		m.mu.Unlock()
		return m.OpenFile(ctx, path, text)
	}
	m.touch(uri, s.Name)
	version := m.nextVersion(uri)
	m.mu.Unlock()
	if err := s.Notify("textDocument/didChange", map[string]any{
		"textDocument":   VersionedTextDocumentIdentifier{URI: uri, Version: version},
		"contentChanges": []ContentChange{{Text: text}},
	}); err != nil {
		return fmt.Errorf("Failed to sync file change %s: %w", path, err)
	}
	return nil
}

// SaveFile sends didSave for an open file on a running server.
func (m *Manager) SaveFile(path string) error {
	s := m.ServerFor(path)
	if s == nil || s.State() != StateRunning {
		return nil
	}
	uri := PathToURI(path)
	m.mu.Lock()
	open := m.docOwner[uri] == s.Name
	m.mu.Unlock()
	if !open {
		return nil
	}
	if err := s.Notify("textDocument/didSave", map[string]any{"textDocument": TextDocumentIdentifier{URI: uri}}); err != nil {
		return fmt.Errorf("Failed to sync file save %s: %w", path, err)
	}
	return nil
}

// IsFileOpen reports whether a file is open on a running server.
func (m *Manager) IsFileOpen(path string) bool {
	uri := PathToURI(path)
	m.mu.Lock()
	owner, ok := m.docOwner[uri]
	m.mu.Unlock()
	if !ok {
		return false
	}
	s := m.servers[owner]
	return s != nil && s.State() == StateRunning
}

// DocumentVersion returns the last version sent for a document URI.
func (m *Manager) DocumentVersion(uri string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[uri]
	return v, ok
}

// Request sends a request to the server that handles path, starting it if
// needed. found is false when no server handles the file.
func (m *Manager) Request(ctx context.Context, path, method string, params any) (res json.RawMessage, found bool, err error) {
	s, err := m.EnsureStarted(ctx, path)
	if s == nil && err == nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	uri := PathToURI(path)
	m.mu.Lock()
	if m.docOwner[uri] == s.Name {
		m.touch(uri, s.Name)
	}
	m.mu.Unlock()
	res, err = s.Request(ctx, method, params)
	return res, true, err
}

// publishParams is the textDocument/publishDiagnostics body.
type publishParams struct {
	URI         string `json:"uri"`
	Version     *int   `json:"version,omitempty"`
	Diagnostics []struct {
		Message  string          `json:"message"`
		Severity int             `json:"severity"`
		Range    Range           `json:"range"`
		Source   string          `json:"source,omitempty"`
		Code     json.RawMessage `json:"code,omitempty"`
	} `json:"diagnostics"`
}

// onNotify records published diagnostics (unless the server's diagnostics
// are turned off) and ignores other notifications.
func (m *Manager) onNotify(server, method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	s := m.servers[server]
	if s == nil || !s.Config.DiagnosticsEnabled() {
		return
	}
	var p publishParams
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return
	}
	m.mu.Lock()
	fd := &FileDiagnostics{Path: URIToPath(p.URI), Version: p.Version, Server: server}
	for _, d := range p.Diagnostics {
		fd.Diagnostics = append(fd.Diagnostics, Diagnostic{
			Message:  d.Message,
			Severity: severityName(d.Severity),
			Range:    d.Range,
			Source:   d.Source,
			Code:     codeString(d.Code),
		})
	}
	watchers := append([]func(FileDiagnostics){}, m.watchers...)
	// A publish for an older version than the one last sent is stale: it is
	// not recorded, but watchers still see it (marked Stale).
	if p.Version != nil {
		if cur, ok := m.versions[p.URI]; ok && *p.Version < cur {
			fd.Stale = true
			m.mu.Unlock()
			for _, w := range watchers {
				w(*fd)
			}
			return
		}
	}
	m.published[server] = true
	m.seq++
	fd.Seq = m.seq
	m.diags[diagKey(fd.Path)] = fd
	close(m.changed)
	m.changed = make(chan struct{})
	snapshot := *fd
	m.mu.Unlock()
	for _, w := range watchers {
		w(snapshot)
	}
}

// OnDiagnostics registers a function told of every recorded publish.
func (m *Manager) OnDiagnostics(f func(FileDiagnostics)) {
	m.mu.Lock()
	m.watchers = append(m.watchers, f)
	m.mu.Unlock()
}

// Diagnostics returns the latest diagnostics recorded for a file.
func (m *Manager) Diagnostics(path string) (FileDiagnostics, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fd, ok := m.diags[diagKey(path)]
	if !ok {
		return FileDiagnostics{}, false
	}
	return *fd, true
}

// WaitDiagnostics waits until a publish newer than afterSeq is recorded for
// path, or ctx ends. It reports whether one arrived.
func (m *Manager) WaitDiagnostics(ctx context.Context, path string, afterSeq uint64) bool {
	key := diagKey(path)
	for {
		m.mu.Lock()
		if fd, ok := m.diags[key]; ok && fd.Seq > afterSeq {
			m.mu.Unlock()
			return true
		}
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// HasPublished reports whether a server has published any diagnostics.
func (m *Manager) HasPublished(server string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.published[server]
}

// Seq returns the sequence number of the latest recorded publish.
func (m *Manager) Seq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// Status lists the configured servers in name order.
func (m *Manager) Status() []ServerStatus {
	if m == nil {
		return nil
	}
	exts := map[string][]string{}
	for ext, name := range m.route {
		exts[name] = append(exts[name], ext)
	}
	out := make([]ServerStatus, 0, len(m.order))
	for _, name := range m.order {
		s := m.servers[name]
		st := ServerStatus{Name: name, Plugin: s.Config.Plugin, State: s.State()}
		st.Extensions = exts[name]
		sort.Strings(st.Extensions)
		if err := s.LastError(); err != nil {
			st.LastError = err.Error()
		}
		out = append(out, st)
	}
	return out
}

// Shutdown stops every running server. Safe to call more than once.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.docs = nil
	m.docOwner = map[string]string{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var failed []string
	for _, name := range m.order {
		s := m.servers[name]
		if st := s.State(); st != StateRunning && st != StateStarting && st != StateError {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Stop(ctx); err != nil {
				errMu.Lock()
				failed = append(failed, fmt.Sprintf("%s: %v", s.Name, err))
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("Failed to stop %d LSP server(s): %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// diagKey normalizes a path for the diagnostics table.
func diagKey(path string) string {
	if strings.Contains(path, "://") {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// severityName maps an LSP DiagnosticSeverity to its label; a missing or
// unknown severity counts as an error.
func severityName(sev int) string {
	switch sev {
	case 2:
		return "Warning"
	case 3:
		return "Info"
	case 4:
		return "Hint"
	default:
		return "Error"
	}
}

// codeString renders a diagnostic code (number or string).
func codeString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}
