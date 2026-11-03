// Package sessionstore is the LOCAL-ONLY persistence layer for gocode
// conversations: it writes each session's turns to an append-only JSONL
// transcript under the user config home and reads them back so a session can be
// resumed (`/resume`, `--resume`, `--continue`). Nothing here ever leaves the
// machine — there is no network, no telemetry, no upload. The only side effects
// are reads and writes beneath <configHome>/projects.
//
// # On-disk format
//
// The layout and the per-line schema follow the Claude-Code-style transcript:
//
//		<configHome>/projects/<escaped-cwd>/<session-id>.jsonl
//
//	  - configHome: the gocode root (internal/confighome: GOCODE_CONFIG_DIR,
//	    else $XDG_CONFIG_HOME/gocode, else ~/.config/gocode), NFC-normalized.
//	    The cli layer resolves it and hands it to New; this package is
//	    base-dir-agnostic.
//	  - "projects": the fixed subdirectory.
//	  - <escaped-cwd>: the absolute cwd with every non-alphanumeric rune replaced
//	    by '-', truncated to 200 chars plus a short FNV hash suffix when longer
//	    (the overflow case is rare and the suffix only needs to disambiguate).
//	  - <session-id>.jsonl: one file per session, named by the session uuid v4.
//
// Each line is one JSON object carrying `type`/`message` plus the base fields
// {parentUuid,isSidechain,uuid,timestamp} and the session fields
// {userType:"external",cwd,sessionId,version}. A user line is {type:"user",message:{role,
// content}}; an assistant line is {type:"assistant",message:{...}}; other
// writers may also add non-turn meta lines (type:"summary"/"custom-title"/"ai-title"/…).
// The fields needed to rebuild the model context are `type`,
// `message` ({role,content}), `uuid`/`parentUuid` (the turn chain) and
// `timestamp`; the rest are carried for compatibility. Meta lines supply the
// picker title (priority: custom-title → ai-title → summary → firstPrompt →
// id[:8]) and tags; see meta.go. Meta lines are append-only and the latest
// one of each kind wins.
//
// # Branches
//
// The turn lines normally form one linear chain in file order. Resuming at an
// earlier message (OpenAt) appends turns whose parentUuid points back at that
// message, so the file then holds a tree; Load follows the parentUuid chain of
// the last line instead of file order (see activeEntries).
//
// # Retention
//
// Cleanup deletes transcripts whose mtime is older than cleanupPeriodDays
// (default 30, 0 disables). There is no count cap/rotation.
//
// # Versioning
//
// Every line carries a `gocodeSchema` integer so a future format change can be
// detected on read; unknown/zero is treated as v1.
package sessionstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/compaction"
	"github.com/umairge66/gocode/internal/ids"
)

// SchemaVersion is the current transcript line schema version, stamped on every
// written line and checked (leniently) on read.
const SchemaVersion = 1

// dirPerm / filePerm are the modes new session dirs and files get. The config
// home is private to the user, so 0o700/0o600 keep transcripts owner-only.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// cwdNameCap is the 200-char cap on the escaped-cwd directory name before a
// hash suffix is appended.
const cwdNameCap = 200

// EntryType discriminates a transcript line.
const (
	typeSummary = "summary"
)

// entry is one JSONL transcript line. Field names and the user/assistant/summary
// `type` values follow the transcript schema; `message` carries the turn
// the model sees. Fields irrelevant to a line are omitted.
//
// parentUuid is always written (null for the first line of a chain and for a
// compact_boundary).
type entry struct {
	Type         string         `json:"type"`
	Subtype      string         `json:"subtype,omitempty"`
	Content      string         `json:"content,omitempty"`
	IsMeta       *bool          `json:"isMeta,omitempty"`
	Level        string         `json:"level,omitempty"`
	UUID         string         `json:"uuid,omitempty"`
	ParentUUID   *string        `json:"parentUuid"`
	Timestamp    string         `json:"timestamp,omitempty"`
	SessionID    string         `json:"sessionId,omitempty"`
	Cwd          string         `json:"cwd,omitempty"`
	Version      string         `json:"version,omitempty"`
	GitBranch    string         `json:"gitBranch,omitempty"`
	UserType     string         `json:"userType,omitempty"`
	IsSidechain  bool           `json:"isSidechain,omitempty"`
	Message      *agent.Message `json:"message,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	GocodeSchema int            `json:"gocodeSchema,omitempty"`

	// Compaction fields (see AppendCompaction).
	LogicalParentUUID         string           `json:"logicalParentUuid,omitempty"`
	CompactMetadata           *compactMetadata `json:"compactMetadata,omitempty"`
	IsCompactSummary          bool             `json:"isCompactSummary,omitempty"`
	IsVisibleInTranscriptOnly bool             `json:"isVisibleInTranscriptOnly,omitempty"`

	// Command is the "/name args" a custom command's expanded prompt (an
	// isMeta user line) came from; it can title the session.
	Command string `json:"command,omitempty"`

	// Meta-line fields (read only; meta.go writes these lines).
	CustomTitle string   `json:"customTitle,omitempty"`
	AITitle     string   `json:"aiTitle,omitempty"`
	Tag         *string  `json:"tag,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	// DeferredToolUse is a deferred-tool-use line's call (deferred.go).
	DeferredToolUse *deferredToolUse `json:"deferredToolUse,omitempty"`

	// line is the entry's raw transcript line when it was read lazily
	// (parseEntriesLazy) and its Message is not decoded yet; see msg.
	line []byte
}

// msg returns the entry's message, decoding it from the raw line on first
// use when the entry was read lazily.
func (e *entry) msg() *agent.Message {
	if e.Message == nil && e.line != nil {
		var w struct {
			Message *agent.Message `json:"message"`
		}
		if json.Unmarshal(e.line, &w) == nil {
			e.Message = w.Message
		}
		e.line = nil
	}
	return e.Message
}

// compactMetadata is a compact_boundary's metadata (userContext is never set,
// so it is omitted).
type compactMetadata struct {
	Trigger            string `json:"trigger"`
	PreTokens          int    `json:"preTokens"`
	MessagesSummarized int    `json:"messagesSummarized,omitempty"`
}

// Transcript line subtypes / contents for the compaction boundary.
const (
	typeSystem             = "system"
	subtypeCompactBoundary = "compact_boundary"
	compactBoundaryContent = "Conversation compacted"
	compactBoundaryLevel   = "info"
)

func (e entry) isCompactBoundary() bool {
	return e.Type == typeSystem && e.Subtype == subtypeCompactBoundary
}

// Store is the root of the on-disk session tree: a config home over an FS. It is
// cheap to construct and holds no open handles; a Session (returned by Create /
// Open) owns the file handle for appends.
type Store struct {
	// base is the user config home (configHome); sessions live under
	// base/projects/<escaped-cwd>/.
	base string
	fs   FS

	// UserType / AppVersion are stamped on written lines for compatibility. They are
	// optional; empty values are omitted.
	UserType   string
	AppVersion string
	// GitBranch, when set, is stamped on written lines (the host resolves it).
	GitBranch string
	// MovedFrom lists former config homes whose session tree now lives under
	// this store's base. Load rewrites absolute paths beneath
	// <former>/projects/ in a transcript (saved tool results, for example) to
	// the same path beneath <base>/projects/, so a resumed conversation points
	// at files that exist. The file on disk is not changed.
	MovedFrom []string
}

// New builds a Store rooted at the given config home over the real filesystem.
// base is typically settingsload.Context.UserConfigDir(). A "" base yields a
// Store whose operations all fail cleanly (no persistence), so callers need not
// special-case a diskless environment.
func New(base string) *Store { return &Store{base: base, fs: osFS{}, UserType: "external"} }

// ErrNoConfigHome is returned by operations when the Store has no config home
// (base == ""), so a diskless session degrades to "no persistence" rather than
// writing to an unexpected location.
var ErrNoConfigHome = errors.New("sessionstore: no config home")

// projectsDir is <base>/projects.
func (s *Store) projectsDir() string { return join(s.base, "projects") }

// ProjectDir is the per-cwd session directory: <base>/projects/<escaped-cwd>.
func (s *Store) ProjectDir(cwd string) string {
	return join(s.projectsDir(), EscapeCwd(cwd))
}

// sessionPath is the .jsonl file for (cwd, id).
func (s *Store) sessionPath(cwd, id string) string {
	return join(s.ProjectDir(cwd), id+".jsonl")
}

// EscapeCwd turns an absolute working directory into its project-directory name:
// every non-alphanumeric rune becomes '-', capped at 200 chars with a short hash
// suffix beyond that.
func EscapeCwd(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) <= cwdNameCap {
		return name
	}
	// Overflow: keep the first 200 chars and append a stable hash so two long
	// paths sharing a 200-char prefix don't collide.
	h := fnv.New32a()
	_, _ = h.Write([]byte(cwd))
	return fmt.Sprintf("%s-%08x", name[:cwdNameCap], h.Sum32())
}

// Session is an open, writable transcript for one session. It appends turns as
// they happen (crash-tolerant: every AppendMessages flushes to disk, so a kill
// leaves a complete, resumable file up to the last completed turn). The file and
// its directory are created lazily on the first write, so an abandoned empty
// session never litters the tree.
type Session struct {
	store     *Store
	id        string
	cwd       string
	path      string
	created   bool // the dir/file has been created
	lastUUID  string
	seededLen int
	// pendingCommand marks the next user prompt line as a slash command's
	// expansion (MarkCommandTurn).
	pendingCommand *commandMark
}

// commandMark is the slash command the next user prompt line came from.
type commandMark struct {
	name, args string
	builtin    bool
}

// MarkCommandTurn marks the next user prompt written as the prompt a slash
// command produced: it is stored as an isMeta line, so it never becomes the
// session title. A custom command run with arguments titles the session as
// "/name args". Implements conversation.CommandTurnRecorder.
func (s *Session) MarkCommandTurn(name, args string, builtin bool) {
	if s == nil {
		return
	}
	s.pendingCommand = &commandMark{name: name, args: strings.TrimSpace(args), builtin: builtin}
}

// ID returns the session id.
func (s *Session) ID() string { return s.id }

// Path returns the session's .jsonl path.
func (s *Session) Path() string { return s.path }

// Create begins a brand-new session (id, cwd). Nothing is written until the
// first AppendMessages, so the file appears only once there is content.
func (s *Store) Create(cwd, id string) (*Session, error) {
	if s == nil || s.base == "" {
		return nil, ErrNoConfigHome
	}
	if id == "" {
		return nil, errors.New("sessionstore: empty session id")
	}
	return &Session{store: s, id: id, cwd: cwd, path: s.sessionPath(cwd, id)}, nil
}

// Open reopens an existing session for appending. seededLen is the number of
// messages already in the file that the caller has loaded into History (so the
// continuation appends only the new turns). The file is assumed to exist; writes
// still create the dir/file if it was removed.
func (s *Store) Open(cwd, id string, seededLen int) (*Session, error) {
	return s.OpenAt(cwd, id, "", seededLen)
}

// OpenAt is Open for a session resumed at an earlier message (LoadAt): the
// next appended turn links to atUUID rather than to the last line, branching
// the transcript there. The lines after atUUID stay on disk; Load follows the
// new branch. An empty atUUID is Open.
func (s *Store) OpenAt(cwd, id, atUUID string, seededLen int) (*Session, error) {
	sess, err := s.Create(cwd, id)
	if err != nil {
		return nil, err
	}
	sess.created = fileExists(s.fs, sess.path)
	sess.seededLen = seededLen
	// Continue the parent-uuid chain from the last line already on disk, so the
	// resumed turns link to the transcript they extend.
	if sess.created {
		sess.lastUUID = s.lastChainUUIDAt(sess.path)
	}
	if atUUID != "" {
		sess.lastUUID = atUUID
	}
	return sess, nil
}

// AppendMessages appends msgs as transcript lines and flushes. It is safe to call
// with an empty slice (no-op). Implements conversation.SessionRecorder.
func (s *Session) AppendMessages(msgs []agent.Message) error {
	if s == nil || len(msgs) == 0 {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	w, err := s.store.fs.OpenAppend(s.path, filePerm)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := bufio.NewWriter(w)
	for i := range msgs {
		if err := s.writeEntry(buf, s.lineFor(msgs[i])); err != nil {
			return err
		}
	}
	return buf.Flush()
}

// AppendCompaction records a compaction APPEND-ONLY: it appends a
// compact_boundary system line followed by msgs — the rebuilt history, whose
// first message is the compact-summary user turn (flagged isCompactSummary) and
// the rest any kept tail — chained after the boundary. Earlier lines are never
// rewritten, so the pre-compaction transcript survives; Load rebuilds the model
// history from the LAST boundary onward. Implements
// conversation.SessionRecorder.
func (s *Session) AppendCompaction(marker compaction.BoundaryMarker, msgs []agent.Message) error {
	if s == nil {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	w, err := s.store.fs.OpenAppend(s.path, filePerm)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := bufio.NewWriter(w)

	content := marker.Content
	if content == "" {
		content = compactBoundaryContent
	}
	notMeta := false
	id := newUUID()
	b := s.baseEntry(typeSystem, id)
	b.Subtype = subtypeCompactBoundary
	b.Content = content
	b.IsMeta = &notMeta
	b.Level = compactBoundaryLevel
	b.ParentUUID = nil
	b.LogicalParentUUID = s.lastUUID
	b.CompactMetadata = &compactMetadata{
		Trigger:            string(marker.Trigger),
		PreTokens:          marker.PreTokens,
		MessagesSummarized: marker.MessagesSummarized,
	}
	if err := s.writeEntry(buf, b); err != nil {
		return err
	}
	prevLast := s.lastUUID
	s.lastUUID = id
	for i := range msgs {
		e := s.lineFor(msgs[i])
		if i == 0 && compaction.IsSummaryMessage(compaction.Message{Role: msgs[i].Role, Content: msgs[i].Content}) {
			e.IsCompactSummary = true
			e.IsVisibleInTranscriptOnly = true
		}
		if err := s.writeEntry(buf, e); err != nil {
			s.lastUUID = prevLast
			return err
		}
	}
	return buf.Flush()
}

// ReplaceHistory rewrites the whole file from msgs. It is DESTRUCTIVE and is
// used only to seed a brand-new file (a forked resume); compaction never calls
// it — see AppendCompaction.
func (s *Session) ReplaceHistory(msgs []agent.Message) error {
	if s == nil {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	s.lastUUID = ""
	w, err := s.store.fs.Create(s.path, filePerm)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := bufio.NewWriter(w)
	for i := range msgs {
		if err := s.writeEntry(buf, s.lineFor(msgs[i])); err != nil {
			return err
		}
	}
	s.created = true
	return buf.Flush()
}

func (s *Session) ensureDir() error {
	if s.created {
		return nil
	}
	if err := s.store.fs.MkdirAll(s.store.ProjectDir(s.cwd), dirPerm); err != nil {
		return err
	}
	s.created = true
	return nil
}

// lineFor builds a transcript entry for one message, threading the parent-uuid
// chain so a reader can reconstruct turn order.
func (s *Session) lineFor(m agent.Message) entry {
	msg := m
	id := newUUID()
	e := s.baseEntry(m.Role, id)
	e.Message = &msg
	s.lastUUID = id
	if mark := s.pendingCommand; mark != nil && m.Role == agent.RoleUser && messageText(m) != "" {
		s.pendingCommand = nil
		meta := true
		e.IsMeta = &meta
		if !mark.builtin && mark.name != "" && mark.args != "" {
			e.Command = "/" + mark.name + " " + mark.args
		}
	}
	return e
}

// baseEntry is the common envelope of a written line: parentUuid links to the
// previous line (null at the head of the chain).
func (s *Session) baseEntry(typ, id string) entry {
	var parent *string
	if s.lastUUID != "" {
		p := s.lastUUID
		parent = &p
	}
	return entry{
		Type:         typ,
		UUID:         id,
		ParentUUID:   parent,
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		SessionID:    s.id,
		Cwd:          s.cwd,
		Version:      s.store.AppVersion,
		GitBranch:    s.store.GitBranch,
		UserType:     s.store.UserType,
		GocodeSchema: SchemaVersion,
	}
}

func (s *Session) writeEntry(w io.Writer, e entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// --- reading --------------------------------------------------------------

// Load reads a session's transcript and reconstructs the conversation history in
// order — the same message list the model saw — skipping summary/meta lines.
// When the transcript contains compact_boundary lines, only the lines after the
// LAST boundary form the history (the compact summary plus everything after
// it), exactly the context the live session had; the pre-compaction lines stay
// on disk but are not loaded (the loader discards everything it has
// accumulated when it meets a compact_boundary without preserved segments).
// A file with no boundary loads
// in full, as before. A
// missing file yields (nil, nil) is NOT returned; it returns an error so the
// caller can distinguish "no such session". Malformed lines are skipped so one
// corrupt line never loses the whole transcript.
func (s *Store) Load(cwd, id string) ([]agent.Message, error) {
	if s == nil || s.base == "" {
		return nil, ErrNoConfigHome
	}
	data, err := s.fs.ReadFile(s.sessionPath(cwd, id))
	if err != nil {
		return nil, err
	}
	return decodeMessages(s.rewriteMovedPaths(data)), nil
}

// rewriteMovedPaths replaces <former>/projects/ with <base>/projects/ in raw
// transcript bytes, in the JSON-escaped spelling the transcript stores.
func (s *Store) rewriteMovedPaths(data []byte) []byte {
	if len(s.MovedFrom) == 0 || s.base == "" {
		return data
	}
	to := jsonEscapedPath(join(s.base, "projects") + pathSep)
	for _, from := range s.MovedFrom {
		if from == "" || from == s.base {
			continue
		}
		old := jsonEscapedPath(join(from, "projects") + pathSep)
		if bytes.Contains(data, old) {
			data = bytes.ReplaceAll(data, old, to)
		}
	}
	return data
}

// jsonEscapedPath is p as it appears inside a JSON string.
func jsonEscapedPath(p string) []byte {
	b, err := json.Marshal(p)
	if err != nil || len(b) < 2 {
		return []byte(p)
	}
	return b[1 : len(b)-1]
}

// decodeMessages parses JSONL transcript bytes into the ordered message list
// the model sees, tolerating blank and malformed lines (a truncated last line
// from a crash is simply skipped) and restarting at each compact_boundary.
func decodeMessages(data []byte) []agent.Message {
	chain, _ := activeEntries(parseEntriesLazy(data), "")
	return chainMessages(chain)
}

// chainMessages is the model turns of a chain, skipping summary / system /
// meta lines.
func chainMessages(chain []entry) []agent.Message {
	var out []agent.Message
	for i := range chain {
		m := chain[i].msg()
		if m == nil || m.Role == "" {
			continue
		}
		out = append(out, *m)
	}
	return out
}

// parseEntries decodes every well-formed line (see forEachEntry).
func parseEntries(data []byte) []entry {
	var out []entry
	forEachEntry(data, func(e entry) { out = append(out, e) })
	return out
}

// parseEntriesLazy is parseEntries without the message bodies: each entry
// keeps its raw line and decodes its message only when msg is called. A long
// transcript is mostly messages that a compaction already summarized, and
// decoding them all just to follow the active chain multiplied the memory a
// resume needed.
func parseEntriesLazy(data []byte) []entry {
	var out []entry
	eachLine(data, func(line []byte) {
		var le struct {
			entry
			Message lazyMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &le); err != nil {
			return
		}
		e := le.entry
		if le.Message {
			e.line = line
		}
		out = append(out, e)
	})
	return out
}

// lazyMessage records that a line has a non-null message without decoding it.
type lazyMessage bool

func (m *lazyMessage) UnmarshalJSON(b []byte) error {
	*m = lazyMessage(string(b) != "null")
	return nil
}

// eachLine calls fn with each non-blank line of data, split in place.
func eachLine(data []byte, fn func([]byte)) {
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		if line = bytes.TrimSpace(line); len(line) > 0 {
			fn(line)
		}
	}
}

// forEachEntry decodes each well-formed JSONL line in order, skipping blank
// and corrupt ones. Lines are split in place, with no length limit: a
// bufio.Scanner cap used to end the scan at the first oversized line, which
// silently dropped every later line of the transcript.
func forEachEntry(data []byte, fn func(entry)) {
	eachLine(data, func(line []byte) {
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			return // skip a corrupt line rather than failing the whole load
		}
		fn(e)
	})
}

// Meta summarizes one stored session for the picker and for latest-resolution.
type Meta struct {
	ID           string
	Cwd          string
	Path         string
	Title        string
	Created      time.Time
	Modified     time.Time
	MessageCount int
	GitBranch    string

	// CustomTitle is the latest custom-title (/rename); "" when never renamed.
	CustomTitle string
	// Summary is the transcript's summary line, FirstPrompt the first prompt
	// that titles it ("" when absent).
	Summary     string
	FirstPrompt string
	// Tags is the session's current tag set (latest tag line), sorted.
	Tags []string
}

// List returns every stored session for cwd, most-recently-modified first, each
// with its title, timestamps, message count and branch. A missing project
// directory yields an empty slice (not an error) so a first-ever run lists
// nothing cleanly.
func (s *Store) List(cwd string) ([]Meta, error) {
	if s == nil || s.base == "" {
		return nil, ErrNoConfigHome
	}
	// A missing or unreadable project dir lists as "no sessions".
	out := s.listDir(s.ProjectDir(cwd), cwd)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Modified.After(out[j].Modified)
	})
	return out, nil
}

// readMeta derives a session's Meta from its transcript. An empty or unreadable
// file is skipped (ok=false). A "" cwd is filled from the transcript's lines
// (the escaped directory name cannot be turned back into a path).
func (s *Store) readMeta(cwd, id, path string) (Meta, bool) {
	data, err := s.fs.ReadFile(path)
	if err != nil {
		return Meta{}, false
	}
	m := Meta{ID: id, Cwd: cwd, Path: path}
	entries := parseEntriesLazy(data)
	var firstUser, summary, aiTitle string
	for i := range entries {
		e := &entries[i]
		if ts := parseTime(e.Timestamp); !ts.IsZero() {
			if m.Created.IsZero() {
				m.Created = ts
			}
			m.Modified = ts
		}
		if e.GitBranch != "" {
			m.GitBranch = e.GitBranch
		}
		if m.Cwd == "" && e.Cwd != "" {
			m.Cwd = e.Cwd
		}
		switch e.Type {
		case typeSummary:
			if summary == "" {
				summary = strings.TrimSpace(e.Summary)
			}
			continue
		case typeCustomTitle:
			m.CustomTitle = strings.TrimSpace(e.CustomTitle)
			continue
		case typeAITitle:
			aiTitle = strings.TrimSpace(e.AITitle)
			continue
		case typeTag:
			m.Tags = e.tagSet()
			continue
		}
		// Only the first prompt (the title) needs a message decoded.
		if firstUser != "" {
			continue
		}
		msg := e.msg()
		if msg == nil || msg.Role == "" {
			continue
		}
		if msg.Role == agent.RoleUser && e.IsMeta != nil && *e.IsMeta {
			// A slash command's injected prompt never titles the session; a
			// custom command run with arguments titles it as typed.
			if firstUser == "" && e.Command != "" {
				firstUser = firstLine(e.Command)
			}
			continue
		}
		if msg.Role == agent.RoleUser && !e.IsCompactSummary &&
			!compaction.IsSummaryMessage(compaction.Message{Role: msg.Role, Content: msg.Content}) {
			firstUser = promptTitle(*msg)
		}
	}
	// Count only what a resume will load: the history restarts at a
	// compaction boundary and follows the active branch (the title keeps the
	// session's first prompt).
	chain, _ := activeEntries(entries, "")
	m.MessageCount = len(chainMessages(chain))
	if m.MessageCount == 0 && summary == "" {
		return Meta{}, false // empty transcript — nothing to resume
	}
	// Fill timestamps from the file mtime when the transcript carried none.
	if m.Modified.IsZero() {
		if fi, err := s.fs.Stat(path); err == nil {
			m.Modified = fi.ModTime()
			if m.Created.IsZero() {
				m.Created = fi.ModTime()
			}
		}
	}
	m.Summary, m.FirstPrompt = summary, firstUser
	title := m.CustomTitle
	if title == "" {
		title = aiTitle
	}
	if title == "" {
		title = summary
	}
	m.Title = pickTitle(title, firstUser, id)
	return m, true
}

// Latest resolves the most-recently-modified session for cwd (the `--continue`
// target). ok is false when there is none.
func (s *Store) Latest(cwd string) (Meta, bool) {
	metas, err := s.List(cwd)
	if err != nil || len(metas) == 0 {
		return Meta{}, false
	}
	return metas[0], true
}

// Find resolves a user-supplied resume value to a session for cwd. It matches,
// in order: an exact session id, a unique id prefix, a custom title (exact,
// then substring; case-insensitive), then a case-insensitive substring of the
// title. ok is false when nothing (or more than one id prefix) matches.
func (s *Store) Find(cwd, value string) (Meta, bool) {
	// An exact session id needs only that file, not a read of every session
	// in the project (List parses each transcript in full).
	if v := strings.TrimSpace(value); v != "" && !strings.ContainsAny(v, `/\\`) && v != "." && v != ".." {
		if p := s.sessionPath(cwd, v); fileExists(s.fs, p) {
			if m, ok := s.readMeta(cwd, v, p); ok {
				return m, true
			}
		}
	}
	metas, err := s.List(cwd)
	if err != nil {
		return Meta{}, false
	}
	return findIn(metas, value)
}

// findIn is Find over an already-listed, most-recent-first slice.
func findIn(metas []Meta, value string) (Meta, bool) {
	v := strings.TrimSpace(value)
	if v == "" || len(metas) == 0 {
		return Meta{}, false
	}
	for _, m := range metas {
		if m.ID == v {
			return m, true
		}
	}
	// Unique id prefix.
	var pfx []Meta
	for _, m := range metas {
		if strings.HasPrefix(m.ID, v) {
			pfx = append(pfx, m)
		}
	}
	if len(pfx) == 1 {
		return pfx[0], true
	}
	// A name given with /rename wins over a derived title: exact first, then
	// substring (first, i.e. most recent, match).
	lv := strings.ToLower(v)
	for _, m := range metas {
		if m.CustomTitle != "" && strings.ToLower(m.CustomTitle) == lv {
			return m, true
		}
	}
	for _, m := range metas {
		if m.CustomTitle != "" && strings.Contains(strings.ToLower(m.CustomTitle), lv) {
			return m, true
		}
	}
	for _, m := range metas {
		if strings.Contains(strings.ToLower(m.Title), lv) {
			return m, true
		}
	}
	return Meta{}, false
}

// Cleanup deletes transcripts for cwd whose last-modified time is older than
// cutoff, implementing age-based retention (cleanupPeriodDays,
// default 30; a zero cutoff disables and deletes nothing). It returns the number
// of transcripts removed. It is best-effort: an unreadable dir or a failed
// remove is skipped, never surfaced. Only the .jsonl files this package writes
// are pruned.

func (s *Store) Cleanup(cwd string, cutoff time.Time) int {
	if s == nil || s.base == "" || cutoff.IsZero() {
		return 0
	}
	dir := s.ProjectDir(cwd)
	ents, err := s.fs.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, de := range ents {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := join(dir, name)
		fi, err := s.fs.Stat(path)
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			if s.fs.Remove(path) == nil {
				removed++
			}
		}
	}
	return removed
}

// --- helpers ---------------------------------------------------------------

func fileExists(fsys FS, path string) bool {
	_, err := fsys.Stat(path)
	return err == nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func messageText(m agent.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == agent.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// structuralTextRE matches text that starts with a context tag or the
// interruption marker: injected context rather than something the user typed.
var structuralTextRE = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)

// shellInputRE extracts the command of a shell-mode <bash-input> block.
var shellInputRE = regexp.MustCompile(`<bash-input>([\s\S]*?)</bash-input>`)

// promptTitle is the title a user message gives the session: a shell-mode
// command titles it "! <command>" (its output never does), text that starts
// with a context tag is skipped, and otherwise the first line of the first
// remaining text block is used. "" means the message gives no title.
func promptTitle(m agent.Message) string {
	for _, blk := range m.Content {
		if blk.Type != agent.BlockText {
			continue
		}
		t := strings.TrimSpace(blk.Text)
		if t == "" {
			continue
		}
		if sm := shellInputRE.FindStringSubmatch(t); sm != nil {
			if cmd := strings.Join(strings.Fields(sm[1]), " "); cmd != "" {
				return "! " + cmd
			}
			continue
		}
		if structuralTextRE.MatchString(t) {
			continue
		}
		return firstLine(t)
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// pickTitle chooses a display title: a custom title / summary if present, else
// the first user message (truncated), else a short form of the id.
func pickTitle(summary, firstUser, id string) string {
	title := summary
	if title == "" {
		title = firstUser
	}
	if title == "" {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	const limit = 72
	if len(title) > limit {
		return title[:limit-1] + "…"
	}
	return title
}

// newUUID returns a fresh session-entry uuid. It is a package var so a test can
// make ids deterministic.
var newUUID = ids.RandomUUID
