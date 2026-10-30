// Package insights implements the /insights command: it scans the user's LOCAL
// session transcripts (<ConfigHome>/projects/<escaped-cwd>/<uuid>.jsonl, the
// tree internal/sessionstore writes), computes per-session statistics,
// extracts per-session qualitative "facets" with a model call (cached on disk),
// aggregates them, asks the model for an insights synthesis, renders a
// self-contained HTML report to a local file and returns the prompt text the
// command submits as the user turn.
//
// # Files (all under the gocode config home, mode 0600, dirs 0700)
//
//	<ConfigHome>/usage-data/session-meta/<session-id>.json  per-session stats cache
//	<ConfigHome>/usage-data/facets/<session-id>.json        per-session facet cache
//	<ConfigHome>/usage-data/report-YYYY-MM-DD-HHMMSS.html   the report
//	<ConfigHome>/usage-data/report.html                     copy of the latest report
//
// # Privacy
//
// No telemetry and no network: the only outbound traffic is the caller's
// Options.Model, which uses the session's configured provider. There are no
// analytics events or error reporting, no remote stats collection, no hosted
// storage backend (remote transcript listing, remote facet/meta/report
// storage) and no credentials plumbing for hosted model calls. The report
// HTML links no external resources, so opening it contacts no third party.
package insights

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelFunc performs one non-interactive model call with the given system
// prompt (Generate passes an empty one), user prompt and output-token
// cap, returning the reply's concatenated text. It is called concurrently
// (up to 50 facet extractions, 7 synthesis sections) and must be safe for that.
type ModelFunc func(ctx context.Context, system, prompt string, maxTokens int) (string, error)

// Options configures Generate.
type Options struct {
	// ConfigHome is the gocode config home; transcripts live under
	// ConfigHome/projects and the cache/report under ConfigHome/usage-data.
	ConfigHome string
	// Model is required for facet extraction and synthesis.
	Model ModelFunc
	// Now defaults to time.Now; it stamps the report file name.
	Now func() time.Time
	// Location is the zone used for "time of day" hours and the report
	// stamp. nil = time.Local (the process-local zone).
	Location *time.Location
	// IsSkillPreload: the command is being loaded as a skill, not invoked;
	// Generate returns SkillPreloadText without scanning anything.
	IsSkillPreload bool
	// PermissionGates reports live permission facts for the auto-mode
	// recommendation. nil = all false (no recommendation).
	PermissionGates func() PermissionGates
	// BashAutoAllowed estimates offline whether a Bash command would run
	// without a permission prompt (the permission engine's offline
	// estimate). nil = a conservative built-in read-only allowlist.
	BashAutoAllowed func(command string) bool
}

// Report is Generate's result.
type Report struct {
	// Prompt is the text submitted to the model as the user turn.
	Prompt string
	// HTMLPath is the written report file; ReportURL is its file:// URL.
	HTMLPath  string
	ReportURL string
	// InsightsJSON is the synthesis result, JSON.stringify(…, null, 2).
	InsightsJSON string
	// Data is the cross-session aggregate.
	Data *AggregatedData
	// Facets are the non-warmup facets used in the aggregate, by session id.
	Facets map[string]*Facets
	// Recommendation is the auto-mode tip, when one applies.
	Recommendation *AutoModeRecommendation
}

// Batching limits.
const (
	maxLoadsPerKind = 200
	logLoadBatch    = 10
	maxNewFacets    = 50
	facetBatch      = 50
)

type generator struct {
	opts     Options
	loc      *time.Location
	cacheMu  sync.Mutex
	cmdCache map[string]bool
}

// isInsightsSession reports whether a log is one of /insights' own model
// calls (its first user turns carry the facet/synthesis prompts). String
// contents are inspected, and since transcripts store arrays of text blocks,
// their text is inspected too.
func isInsightsSession(log *sessionLog) bool {
	for i, e := range log.messages {
		if i >= 5 {
			break
		}
		if e.Type != "user" || e.Message == nil {
			continue
		}
		t := messageText(e.Message)
		if strings.Contains(t, "RESPOND WITH ONLY A VALID JSON OBJECT") || strings.Contains(t, "record_facets") {
			return true
		}
	}
	return false
}

// isWarmupOnly reports whether a session's only positive goal category is
// warmup_minimal.
func isWarmupOnly(f *Facets) bool {
	if f == nil {
		return false
	}
	gc := f.obj.obj("goal_categories")
	var pos []string
	for _, k := range objectEntries(gc) {
		if v, ok := gc.vals[k].(float64); ok && v > 0 {
			pos = append(pos, k)
		}
	}
	return len(pos) == 1 && pos[0] == "warmup_minimal"
}

type loadItem struct {
	file   sessionFile
	cached *SessionMeta
}

// Generate runs the whole /insights pipeline and returns the prompt to submit.
func Generate(ctx context.Context, opts Options) (Report, error) {
	if opts.IsSkillPreload {
		return Report{Prompt: SkillPreloadText}, nil
	}
	if opts.ConfigHome == "" {
		return Report{}, errors.New("insights: no config home")
	}
	if opts.Model == nil {
		return Report{}, errors.New("insights: no model configured")
	}
	g := &generator{opts: opts, loc: opts.Location, cmdCache: map[string]bool{}}
	if g.loc == nil {
		g.loc = time.Local
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	files := listSessionFiles(opts.ConfigHome)
	scanned := len(files)

	// 1. Reuse fresh cached session-meta; queue the rest for (re)computation.
	var metas []*SessionMeta
	var queue []loadItem
	newCount, staleCount := 0, 0
	for idx, f := range files {
		cached := g.loadCachedMeta(f.sessionID)
		fresh := cached != nil && (cached.BashWouldPromptCount != nil || idx >= metaRecomputeThreshold)
		switch {
		case cached != nil && fresh && (cached.TranscriptMtime == nil || *cached.TranscriptMtime >= f.mtime):
			metas = append(metas, cached)
		case cached == nil:
			if newCount < maxLoadsPerKind {
				queue = append(queue, loadItem{file: f})
				newCount++
			}
		case staleCount < maxLoadsPerKind:
			queue = append(queue, loadItem{file: f, cached: cached})
			staleCount++
		default:
			metas = append(metas, cached)
		}
	}

	// 2. Parse queued transcripts (10 at a time) and compute their metadata.
	logs := map[string]*sessionLog{}
	for start := 0; start < len(queue); start += logLoadBatch {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		batch := queue[start:min(start+logLoadBatch, len(queue))]
		loaded := make([][]*sessionLog, len(batch))
		var wg sync.WaitGroup
		for i, it := range batch {
			wg.Add(1)
			go func(i int, path string) {
				defer wg.Done()
				l, err := readSessionLogs(path)
				if err == nil {
					loaded[i] = l
				}
			}(i, it.file.path)
		}
		wg.Wait()
		best := map[string]*SessionMeta{}
		var bestOrder []string
		for i, it := range batch {
			got := false
			for _, lg := range loaded[i] {
				if isInsightsSession(lg) || lg.created.IsZero() || lg.modified.IsZero() {
					continue
				}
				se := g.buildSessionMeta(lg)
				mt := it.file.mtime
				se.TranscriptMtime = &mt
				metas = append(metas, se)
				got = true
				if betterMeta(se, best[se.SessionID]) {
					if _, ok := best[se.SessionID]; !ok {
						bestOrder = append(bestOrder, se.SessionID)
					}
					best[se.SessionID] = se
				}
				logs[se.SessionID] = lg
			}
			if !got && it.cached != nil {
				metas = append(metas, it.cached)
			}
		}
		for _, id := range bestOrder {
			g.saveSessionMeta(best[id])
		}
	}

	// 3. Deduplicate by session id, newest first, and keep substantive sessions.
	byID := map[string]*SessionMeta{}
	var order []string
	for _, k := range metas {
		cur, ok := byID[k.SessionID]
		if !ok {
			order = append(order, k.SessionID)
		}
		if betterMeta(k, cur) {
			byID[k.SessionID] = k
		}
	}
	metas = metas[:0]
	for _, id := range order {
		metas = append(metas, byID[id])
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].StartTime > metas[j].StartTime })
	var kept []*SessionMeta
	for _, k := range metas {
		if k.UserMessageCount < 2 || k.DurationMinutes < 1 {
			continue
		}
		kept = append(kept, k)
	}

	// 4. Facets: cached, else extracted (at most 50 new per run, 50 in parallel).
	facets := newFacetMap()
	type pending struct {
		log *sessionLog
		id  string
	}
	var todo []pending
	for _, k := range kept {
		if f := g.loadCachedFacets(k.SessionID); f != nil {
			facets.set(k.SessionID, f)
		} else if lg := logs[k.SessionID]; lg != nil && len(todo) < maxNewFacets {
			todo = append(todo, pending{lg, k.SessionID})
		}
	}
	for start := 0; start < len(todo); start += facetBatch {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		batch := todo[start:min(start+facetBatch, len(todo))]
		out := make([]*Facets, len(batch))
		var wg sync.WaitGroup
		for i, p := range batch {
			wg.Add(1)
			go func(i int, p pending) {
				defer wg.Done()
				out[i] = g.extractFacets(ctx, p.log, p.id)
			}(i, p)
		}
		wg.Wait()
		for i, f := range out {
			if f != nil {
				facets.set(batch[i].id, f)
				_ = g.saveFacets(f)
			}
		}
	}

	// 5. Drop warmup-only sessions, aggregate, synthesize, recommend, render.
	var analyzed []*SessionMeta
	for _, k := range kept {
		if !isWarmupOnly(facets.get(k.SessionID)) {
			analyzed = append(analyzed, k)
		}
	}
	used := newFacetMap()
	for _, id := range facets.keys {
		if f := facets.get(id); !isWarmupOnly(f) {
			used.set(id, f)
		}
	}
	data := aggregate(analyzed, used)
	data.TotalSessionsScanned = scanned
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	insightsObj := g.synthesize(ctx, data, facets)
	var gates PermissionGates
	if opts.PermissionGates != nil {
		gates = opts.PermissionGates()
	}
	reco := recommendAutoMode(data, gates)
	html := renderReport(data, insightsObj, reco)
	t := now().In(g.loc)
	stamp := fmt.Sprintf("%04d-%02d-%02d-%02d%02d%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	htmlPath, err := writeReport(opts.ConfigHome, html, stamp)
	if err != nil {
		return Report{}, fmt.Errorf("insights: writing report: %w", err)
	}

	// 6. The prompt.
	reportURL := fileURL(htmlPath)
	insightsJSON := stringifyJS(insightsObj, "  ")
	recLine := ""
	if reco != nil {
		recLine = recommendationLine(reco)
	}
	prompt := buildPrompt(promptInput{
		insightsJSON:       insightsJSON,
		reportURL:          reportURL,
		htmlPath:           htmlPath,
		facetsDir:          facetsDir(opts.ConfigHome),
		header:             buildHeader(data),
		summaryText:        glanceSummary(insightsObj),
		recommendationLine: recLine,
	})
	fm := make(map[string]*Facets, used.len())
	for _, id := range used.keys {
		fm[id] = used.vals[id]
	}
	return Report{
		Prompt:         prompt,
		HTMLPath:       htmlPath,
		ReportURL:      reportURL,
		InsightsJSON:   insightsJSON,
		Data:           data,
		Facets:         fm,
		Recommendation: reco,
	}, nil
}

// fileURL is the file:// URL for an absolute local path; Windows drive paths
// become file:///C:/... with forward slashes.
func fileURL(p string) string {
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return (&url.URL{Scheme: "file", Path: s}).String()
}
