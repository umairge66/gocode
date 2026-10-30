package settingsload

import (
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/settingspolicy"
)

// CanonicalSourceOrder is the settings-source precedence, lowest priority
// first. Later entries override earlier ones when the sources are merged.
var CanonicalSourceOrder = []settingspolicy.Source{
	settingspolicy.SourceUser,
	settingspolicy.SourceProject,
	settingspolicy.SourceLocal,
	settingspolicy.SourceFlag,
	settingspolicy.SourcePolicy,
}

// fileSources are the sources backed by a single on-disk file (as opposed to
// the admin tiers, which fan out over managed-settings.json + drop-ins).
var fileSources = []settingspolicy.Source{
	settingspolicy.SourceUser,
	settingspolicy.SourceProject,
	settingspolicy.SourceLocal,
	settingspolicy.SourceFlag,
	settingspolicy.SourcePolicy,
}

// LoadAll loads every file-backed settings source for ctx, keyed by Source.
// Sources with no backing file in ctx come back as LoadAbsent.
func LoadAll(fsys fsfacade.FS, ctx *Context) map[settingspolicy.Source]settingspolicy.LoadResult {
	out := make(map[settingspolicy.Source]settingspolicy.LoadResult, len(fileSources))
	for _, src := range fileSources {
		out[src] = LoadSource(fsys, ctx, src)
	}
	return out
}

// OrderedResult pairs a source with its load result, used to feed the source
// precedence merge in canonical (low→high) order.
type OrderedResult struct {
	Source settingspolicy.Source
	Result settingspolicy.LoadResult
}

// EffectiveSources loads the sources active for this session and returns them
// in canonical precedence order (lowest priority first), ready to be layered
// for settingspolicy. allowed is the session's opt-in source set;
// flagSettings and policySettings are always included
// (ResolveEffectiveSources).
func EffectiveSources(fsys fsfacade.FS, ctx *Context, allowed []settingspolicy.Source) []OrderedResult {
	order := settingspolicy.ResolveEffectiveSources(allowed, CanonicalSourceOrder)
	out := make([]OrderedResult, 0, len(order))
	for _, src := range order {
		out = append(out, OrderedResult{Source: src, Result: LoadSource(fsys, ctx, src)})
	}
	// Workspace trust (trust.go): an untrusted folder's project/local sources
	// lose the settings that run code or widen permissions, and a git-tracked
	// settings.local.json is MCP-gated like project settings.
	applyTrust(ctx, out)
	return out
}

// dropInFiles lists the managed-settings.d drop-in filenames in sorted order,
// filtered to non-dot ".json" regular (or symlinked) entries.
func dropInFiles(fsys fsfacade.FS, dir string) []string {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil // absent directory is not an error (ENOENT tolerated)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if e.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Store is a concrete settings store over an fsfacade.FS. It satisfies the
// envsec.SettingsStore seam that internal/settingspolicy / internal/envsec left
// as a TODO (the headline "load, merge and validate the layered settings
// sources" responsibility).
type Store struct {
	FS      fsfacade.FS
	Context *Context
	// Allowed is the session's opt-in source set; flagSettings and
	// policySettings are always consulted regardless.
	Allowed []settingspolicy.Source
}

var _ envsec.SettingsStore = (*Store)(nil)

// NewStore builds a Store. A nil fsys defaults to the real filesystem
// (fsfacade.OSFS).
func NewStore(fsys fsfacade.FS, ctx *Context, allowed []settingspolicy.Source) *Store {
	if fsys == nil {
		fsys = fsfacade.OSFS{}
	}
	return &Store{FS: fsys, Context: ctx, Allowed: allowed}
}

// RawKeyPresence reports "present", "absent" or "unknowable" for a top-level
// settings key across the configured sources.
//
//   - "present"    — a readable source object contains the key.
//   - "unknowable" — a source exists but could not be read or parsed, or was
//     not a JSON object, so presence cannot be decided.
//   - "absent"     — every consulted source was readable and lacked the key.
//
// The user-source keys "attribution" and "includeCoAuthoredBy" are not counted
// as present.
func (s *Store) RawKeyPresence(key string) (string, error) {
	present := false
	unknowable := false

	for _, src := range settingspolicy.ResolveEffectiveSources(s.Allowed, CanonicalSourceOrder) {
		res := LoadSource(s.FS, s.Context, src)
		switch res.LoadState {
		case settingspolicy.LoadUnreadable, settingspolicy.LoadUnparsable, settingspolicy.LoadMalformed:
			unknowable = true
		case settingspolicy.LoadLoaded:
			if _, ok := res.Settings[key]; ok {
				if !(src == settingspolicy.SourceUser && (key == "attribution" || key == "includeCoAuthoredBy")) {
					present = true
				}
			}
		case settingspolicy.LoadAbsent, settingspolicy.LoadDidNotLoad:
			// absent / empty contribute nothing
		}
	}

	if unknowable {
		return "unknowable", nil
	}
	if present {
		return "present", nil
	}
	return "absent", nil
}
