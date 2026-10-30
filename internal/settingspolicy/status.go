package settingspolicy

// LoadState is the outcome of attempting to load a settings file or directory.
// It appears as the `loadState` field of the loader result
// {settings, errors, loadState}.
type LoadState string

const (
	// LoadLoaded: the settings were read and parsed successfully.
	LoadLoaded LoadState = "loaded"
	// LoadAbsent: the settings file/dir did not exist.
	LoadAbsent LoadState = "absent"
	// LoadUnreadable: the path existed but could not be read.
	LoadUnreadable LoadState = "unreadable"
	// LoadUnparsable: the content was not valid JSON.
	LoadUnparsable LoadState = "unparsable"
	// LoadMalformed: parsed, but not the expected shape / failed validation.
	LoadMalformed LoadState = "malformed"
	// LoadDidNotLoad: a terminal failure that aborted the load.
	LoadDidNotLoad LoadState = "didNotLoad"
)

// LoadError is one diagnostic emitted while loading a settings source.
type LoadError struct {
	File     string
	Path     string
	Message  string
	Severity string // e.g. "fatal", "error", "warning"
}

// LoadResult is the shape returned by the settings loader for a single source.
type LoadResult struct {
	Settings  Settings
	Errors    []LoadError
	LoadState LoadState

	// Warnings holds advisory schema diagnostics (unknown top-level keys, wrong
	// value types) gathered while loading. They are additive and non-fatal: the
	// settings still load and every key is still applied. A fatal/unreadable/
	// unparsable/malformed result carries no warnings. nil when there are none.
	Warnings []string
}

// ParentSettingsBehavior controls how a layer of settings combines with lower
// (parent) layers under the same admin tier.
type ParentSettingsBehavior string

const (
	// FirstWins is the default (except in gateway sessions): the highest-
	// priority layer wins outright and parents are not merged in.
	FirstWins ParentSettingsBehavior = "first-wins"
	// MergeBehavior deep-merges this layer over its parents.
	MergeBehavior ParentSettingsBehavior = "merge"
)
