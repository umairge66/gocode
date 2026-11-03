package sessionstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

// This file holds the session meta lines (custom titles and tags), the
// branch-aware chain walk, cross-project listing and the read helpers behind
// `gocode sessions`.
//
// Meta lines carry no uuid/parentUuid, so they never join the turn chain:
//
//	{"type":"custom-title","customTitle":"<title>","sessionId":"<id>"}
//	{"type":"tag","tag":"<last added>","tags":["a","b"],"sessionId":"<id>"}
//
// The latest line of each kind wins. Claude Code (checked against v2.1.317)
// writes {"type":"tag","tag":<string>,"sessionId":...} with a single
// latest-wins tag, and {"type":"custom-title","customTitle":...,"sessionId":...}.
// gocode's title line is the same; its tag line adds `tags`, the whole tag
// set, and keeps `tag` as the most recently added tag ("" once none is
// left), so Claude Code, which ignores the extra field, still reads a
// current tag.

// Meta-line types.
const (
	typeCustomTitle = "custom-title"
	typeAITitle     = "ai-title"
	typeTag         = "tag"
)

// ErrNotFound is returned when a message uuid is not in the transcript.
var ErrNotFound = errors.New("sessionstore: not found")

// titleLine / tagLine are the written meta lines.
type titleLine struct {
	Type         string `json:"type"`
	CustomTitle  string `json:"customTitle"`
	SessionID    string `json:"sessionId"`
	GocodeSchema int    `json:"gocodeSchema"`
}

type tagLine struct {
	Type         string   `json:"type"`
	Tag          string   `json:"tag"`
	Tags         []string `json:"tags"`
	SessionID    string   `json:"sessionId"`
	GocodeSchema int      `json:"gocodeSchema"`
}

// tagSet is a tag line's tag set: `tags` when present, else the single `tag`.
func (e entry) tagSet() []string {
	if e.Tags != nil {
		return normalizeTags(e.Tags)
	}
	if e.Tag != nil {
		return normalizeTags([]string{*e.Tag})
	}
	return nil
}

// normalizeTags trims, drops empties and duplicates, and sorts.
func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// --- writing meta lines ----------------------------------------------------

// Rename records a custom title for the session (append-only; the latest
// wins). An empty title is rejected.
func (s *Session) Rename(title string) error {
	if s == nil {
		return ErrNoConfigHome
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	return s.store.appendTitle(s.path, s.id, title)
}

// Tags returns the session's current tags (nil before any tag line).
func (s *Session) Tags() []string {
	if s == nil {
		return nil
	}
	data, err := s.store.fs.ReadFile(s.path)
	if err != nil {
		return nil
	}
	return tagsIn(data)
}

// SetTags records tags as the session's tag set; latest is the tag reported
// in the single-tag `tag` field (the one just added, "" for none).
func (s *Session) SetTags(tags []string, latest string) error {
	if s == nil {
		return ErrNoConfigHome
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	return s.store.appendTags(s.path, s.id, tags, latest)
}

// Rename records a custom title for the stored session m.
func (s *Store) Rename(m Meta, title string) error {
	if s == nil || s.base == "" {
		return ErrNoConfigHome
	}
	return s.appendTitle(m.Path, m.ID, title)
}

// SetTags records tags as the stored session m's tag set.
func (s *Store) SetTags(m Meta, tags []string, latest string) error {
	if s == nil || s.base == "" {
		return ErrNoConfigHome
	}
	return s.appendTags(m.Path, m.ID, tags, latest)
}

func (s *Store) appendTitle(path, id, title string) error {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return errors.New("sessionstore: empty title")
	}
	return s.appendLine(path, titleLine{Type: typeCustomTitle, CustomTitle: title, SessionID: id, GocodeSchema: SchemaVersion})
}

func (s *Store) appendTags(path, id string, tags []string, latest string) error {
	tags = normalizeTags(tags)
	if tags == nil {
		tags = []string{}
	}
	found := false
	for _, t := range tags {
		found = found || t == latest
	}
	if !found {
		latest = ""
		if len(tags) > 0 {
			latest = tags[len(tags)-1]
		}
	}
	return s.appendLine(path, tagLine{Type: typeTag, Tag: latest, Tags: tags, SessionID: id, GocodeSchema: SchemaVersion})
}

func (s *Store) appendLine(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w, err := s.fs.OpenAppend(path, filePerm)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := bufio.NewWriter(w)
	if _, err := buf.Write(append(data, '\n')); err != nil {
		return err
	}
	return buf.Flush()
}

// tagsIn is the tag set of the latest tag line in data.
func tagsIn(data []byte) []string {
	var tags []string
	forEachEntry(data, func(e entry) {
		if e.Type == typeTag {
			tags = e.tagSet()
		}
	})
	return tags
}

// --- the active chain -------------------------------------------------------

// activeEntries returns the lines that make up the resumable history ending at
// the line with uuid at (the end of the transcript when at is ""): a resume
// loads the turns among them. ok is false when at is not in the transcript.
//
// A linear transcript (no line has two children) is read in file order and
// restarts after the last compact_boundary, exactly as before branches
// existed. A branched one follows the parentUuid links back from the target
// (the last chain line by default) to the chain's root — a compact_boundary
// or a first line has a null parent. Sidechain lines are ignored.
func activeEntries(es []entry, at string) ([]entry, bool) {
	target := -1
	children := map[string]int{}
	branched := false
	for i, e := range es {
		if e.IsSidechain || e.UUID == "" {
			continue
		}
		if e.ParentUUID != nil && *e.ParentUUID != "" {
			children[*e.ParentUUID]++
			if children[*e.ParentUUID] > 1 {
				branched = true
			}
		}
		if at != "" && e.UUID == at {
			target = i
		}
	}
	if at != "" && target < 0 {
		return nil, false
	}
	if !branched {
		upto := es
		if target >= 0 {
			upto = es[:target+1]
		}
		var out []entry
		for _, e := range upto {
			if e.isCompactBoundary() {
				out = nil
				continue
			}
			out = append(out, e)
		}
		return out, true
	}
	if target < 0 {
		for i := len(es) - 1; i >= 0; i-- {
			if es[i].UUID != "" && !es[i].IsSidechain {
				target = i
				break
			}
		}
	}
	byUUID := map[string]int{}
	for i, e := range es {
		if e.UUID != "" && !e.IsSidechain {
			byUUID[e.UUID] = i
		}
	}
	var rev []entry
	seen := map[int]bool{}
	for i := target; i >= 0 && !seen[i]; {
		seen[i] = true
		e := es[i]
		if e.isCompactBoundary() {
			break
		}
		rev = append(rev, e)
		if e.ParentUUID == nil {
			break
		}
		next, ok := byUUID[*e.ParentUUID]
		if !ok {
			break
		}
		i = next
	}
	out := make([]entry, len(rev))
	for i := range rev {
		out[len(rev)-1-i] = rev[i]
	}
	return out, true
}

// --- loading at a message ---------------------------------------------------

// LoadAt is Load truncated at the message with uuid at: the history a resume
// at that message continues from (the message itself included). It returns
// ErrNotFound when at is not in the transcript.
func (s *Store) LoadAt(cwd, id, at string) ([]agent.Message, error) {
	if s == nil || s.base == "" {
		return nil, ErrNoConfigHome
	}
	data, err := s.fs.ReadFile(s.sessionPath(cwd, id))
	if err != nil {
		return nil, err
	}
	chain, ok := activeEntries(parseEntriesLazy(s.rewriteMovedPaths(data)), at)
	if !ok {
		return nil, fmt.Errorf("%w: message %s in session %s", ErrNotFound, at, id)
	}
	return chainMessages(chain), nil
}

// Record is one transcript line as `gocode sessions show` reports it.
type Record struct {
	UUID             string         `json:"uuid,omitempty"`
	ParentUUID       string         `json:"parentUuid,omitempty"`
	Type             string         `json:"type"`
	Subtype          string         `json:"subtype,omitempty"`
	Timestamp        time.Time      `json:"timestamp,omitzero"`
	IsCompactSummary bool           `json:"isCompactSummary,omitempty"`
	IsMeta           bool           `json:"isMeta,omitempty"`
	Message          *agent.Message `json:"message,omitempty"`
}

// Records reads the transcript at m.Path. With full=false it returns the
// active chain (what a resume loads: the post-compaction, current-branch
// turns); with full=true every chain line in file order, compaction
// boundaries included. compactions counts the boundaries in the file.
func (s *Store) Records(m Meta, full bool) (recs []Record, compactions int, err error) {
	if s == nil || s.base == "" {
		return nil, 0, ErrNoConfigHome
	}
	data, err := s.fs.ReadFile(m.Path)
	if err != nil {
		return nil, 0, err
	}
	es := parseEntries(s.rewriteMovedPaths(data))
	for _, e := range es {
		if e.isCompactBoundary() {
			compactions++
		}
	}
	lines := es
	if !full {
		lines, _ = activeEntries(es, "")
	}
	for _, e := range lines {
		if e.UUID == "" || e.IsSidechain {
			continue // meta lines
		}
		if !full && (e.Message == nil || e.Message.Role == "") {
			continue
		}
		r := Record{
			UUID: e.UUID, Type: e.Type, Subtype: e.Subtype,
			Timestamp: parseTime(e.Timestamp), IsCompactSummary: e.IsCompactSummary,
			IsMeta: e.IsMeta != nil && *e.IsMeta, Message: e.Message,
		}
		if e.ParentUUID != nil {
			r.ParentUUID = *e.ParentUUID
		}
		recs = append(recs, r)
	}
	return recs, compactions, nil
}

// --- cross-project listing --------------------------------------------------

// ListAll returns every stored session of every project, most recent first.
// Each Meta's Cwd comes from its transcript.
func (s *Store) ListAll() ([]Meta, error) {
	if s == nil || s.base == "" {
		return nil, ErrNoConfigHome
	}
	dirs, err := s.fs.ReadDir(s.projectsDir())
	if err != nil {
		return nil, nil // no projects yet
	}
	var out []Meta
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := join(s.projectsDir(), d.Name())
		out = append(out, s.listDir(dir, "")...)
	}
	sort.SliceStable(out, func(i, j int) bool { return newerFirst(out[i], out[j]) })
	return out, nil
}

// listDir reads the metas of the transcripts in one project directory.
func (s *Store) listDir(dir, cwd string) []Meta {
	ents, err := s.fs.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Meta
	for _, de := range ents {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if m, ok := s.readMeta(cwd, strings.TrimSuffix(name, ".jsonl"), join(dir, name)); ok {
			out = append(out, m)
		}
	}
	return out
}

// FindAll is Find across every project.
func (s *Store) FindAll(value string) (Meta, bool) {
	metas, err := s.ListAll()
	if err != nil {
		return Meta{}, false
	}
	return findIn(metas, value)
}

// Delete removes the stored session m's transcript file.
func (s *Store) Delete(m Meta) error {
	if s == nil || s.base == "" {
		return ErrNoConfigHome
	}
	return s.fs.Remove(m.Path)
}

// CustomTitle returns the session's latest custom title ("" when never
// renamed).
func (s *Session) CustomTitle() string {
	if s == nil {
		return ""
	}
	data, err := s.store.fs.ReadFile(s.path)
	if err != nil {
		return ""
	}
	title := ""
	forEachEntry(data, func(e entry) {
		if e.Type == typeCustomTitle {
			title = strings.TrimSpace(e.CustomTitle)
		}
	})
	return title
}
