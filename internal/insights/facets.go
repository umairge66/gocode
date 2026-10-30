package insights

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Facets is one session's model-extracted qualitative record (the parsed JSON
// object the facet prompt asks for, plus session_id), kept insertion-ordered
// so it round-trips to disk exactly as JSON.stringify writes it.
type Facets struct {
	obj *jsObject
}

// SessionID is the facet's session id.
func (f *Facets) SessionID() string { return f.obj.str("session_id") }

// JSON renders the facet as JSON.stringify(f, null, 2).
func (f *Facets) JSON() string { return stringifyJS(f.obj, "  ") }

// validFacets is the facet shape check.
func validFacets(v any) (*jsObject, bool) {
	o, ok := v.(*jsObject)
	if !ok || o == nil {
		return nil, false
	}
	for _, k := range []string{"underlying_goal", "outcome", "brief_summary"} {
		if _, ok := o.vals[k].(string); !ok {
			return nil, false
		}
	}
	for _, k := range []string{"goal_categories", "user_satisfaction_counts", "friction_counts"} {
		switch o.vals[k].(type) {
		case *jsObject, []any:
		default:
			return nil, false
		}
	}
	return o, true
}

// firstJSONObject is a greedy match from the first { to the last }.
var firstJSONObject = regexp.MustCompile(`\{[\s\S]*\}`)

// formatTranscript renders a session for facet extraction: a short header and
// the [User]/[Assistant]/[Tool] lines (user text ≤500, assistant ≤300 units).
func (g *generator) formatTranscript(log *sessionLog) string {
	m := g.buildSessionMeta(log)
	lines := []string{
		"Session: " + truncateUTF16(m.SessionID, 8),
		"Date: " + m.StartTime,
		"Project: " + m.ProjectPath,
		"Duration: " + jsNum(m.DurationMinutes) + " min",
		"",
	}
	lines = append(lines, transcriptLines(log.messages, 500, 300)...)
	return strings.Join(lines, "\n")
}

// summarizeChunk asks the model to condense one 25000-unit transcript slice
// (max 500 output tokens); any failure falls back to the slice's first 2000
// units.
func (g *generator) summarizeChunk(ctx context.Context, chunk string) string {
	fallback := truncateUTF16(chunk, 2000)
	out, err := g.opts.Model(ctx, "", chunkSummaryPrompt+chunk, 500)
	if err != nil || out == "" {
		return fallback
	}
	return out
}

// transcriptForFacets returns the formatted transcript, or for one longer than
// 30000 units, a header plus per-25000-unit model summaries joined by
// "\n\n---\n\n".
func (g *generator) transcriptForFacets(ctx context.Context, log *sessionLog) string {
	n := g.formatTranscript(log)
	total := utf16Len(n)
	if total <= 30000 {
		return n
	}
	const step = 25000
	var chunks []string
	for i := 0; i < total; i += step {
		chunks = append(chunks, utf16Slice(n, i, i+step))
	}
	sums := make([]string, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			sums[i] = g.summarizeChunk(ctx, c)
		}(i, c)
	}
	wg.Wait()
	m := g.buildSessionMeta(log)
	head := strings.Join([]string{
		"Session: " + truncateUTF16(m.SessionID, 8),
		"Date: " + m.StartTime,
		"Project: " + m.ProjectPath,
		"Duration: " + jsNum(m.DurationMinutes) + " min",
		fmt.Sprintf("[Long session - %d parts summarized]", len(chunks)),
		"",
	}, "\n")
	return head + strings.Join(sums, "\n\n---\n\n")
}

// extractFacets runs the facet-extraction model call (max 4096 output
// tokens) and parses the first {...} block of its reply. Returns nil on any
// failure.
func (g *generator) extractFacets(ctx context.Context, log *sessionLog, sessionID string) *Facets {
	transcript := g.transcriptForFacets(ctx, log)
	prompt := fill(facetPromptTpl, facetPromptIntro, transcript)
	out, err := g.opts.Model(ctx, "", prompt, 4096)
	if err != nil {
		return nil
	}
	match := firstJSONObject.FindString(out)
	if match == "" {
		return nil
	}
	v, err := parseJSValue([]byte(match))
	if err != nil {
		return nil
	}
	o, ok := validFacets(v)
	if !ok {
		return nil
	}
	o.set("session_id", sessionID)
	return &Facets{obj: o}
}

// facetsDir is <configHome>/usage-data/facets.
func facetsDir(configHome string) string { return filepath.Join(usageDataDir(configHome), "facets") }

// sessionMetaDir is <configHome>/usage-data/session-meta.
func sessionMetaDir(configHome string) string {
	return filepath.Join(usageDataDir(configHome), "session-meta")
}

// usageDataDir is <configHome>/usage-data.
func usageDataDir(configHome string) string { return filepath.Join(configHome, "usage-data") }

// loadCachedFacets reads usage-data/facets/<id>.json; an invalid record is
// deleted and treated as missing.
func (g *generator) loadCachedFacets(sessionID string) *Facets {
	path := filepath.Join(facetsDir(g.opts.ConfigHome), sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := parseJSValue(data)
	if err != nil {
		return nil
	}
	o, ok := validFacets(v)
	if !ok {
		_ = os.Remove(path)
		return nil
	}
	return &Facets{obj: o}
}

// saveFacets writes usage-data/facets/<id>.json (mode 0600).
func (g *generator) saveFacets(f *Facets) error {
	dir := facetsDir(g.opts.ConfigHome)
	_ = os.MkdirAll(dir, 0o700)
	return os.WriteFile(filepath.Join(dir, f.SessionID()+".json"), []byte(f.JSON()), 0o600)
}

// loadCachedMeta reads usage-data/session-meta/<id>.json.
func (g *generator) loadCachedMeta(sessionID string) *SessionMeta {
	data, err := os.ReadFile(filepath.Join(sessionMetaDir(g.opts.ConfigHome), sessionID+".json"))
	if err != nil {
		return nil
	}
	var m SessionMeta
	if err := jsonUnmarshalStrict(data, &m); err != nil {
		return nil
	}
	m.normalize()
	return &m
}

// saveSessionMeta writes usage-data/session-meta/<id>.json (mode 0600);
// failures are swallowed.
func (g *generator) saveSessionMeta(m *SessionMeta) {
	data, err := m.marshal()
	if err != nil {
		return
	}
	dir := sessionMetaDir(g.opts.ConfigHome)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, m.SessionID+".json"), data, 0o600)
}
