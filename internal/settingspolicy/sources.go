package settingspolicy

// Source is the canonical name of a settings source (the key under which a
// layer's settings are stored and merged). The full precedence ordering of
// these sources is defined elsewhere and is passed into
// ResolveEffectiveSources rather than hardcoded here.
type Source string

const (
	SourceUser    Source = "userSettings"
	SourceProject Source = "projectSettings"
	SourceLocal   Source = "localSettings"
	SourceManaged Source = "managedSettings"
	SourceFlag    Source = "flagSettings"
	SourcePolicy  Source = "policySettings"
	SourceDefault Source = "defaultSettings"
)

// ResolveEffectiveSources returns the subset of the canonical precedence
// ordering that is active for this session. The caller's explicitly-allowed
// sources are always augmented with flagSettings and policySettings (which are
// never opt-out), and the result preserves the canonical order.
//
// ordered is the canonical precedence list; passing it in keeps this function
// free of that dependency.
func ResolveEffectiveSources(allowed []Source, ordered []Source) []Source {
	active := make(map[Source]struct{}, len(allowed)+2)
	for _, s := range allowed {
		active[s] = struct{}{}
	}
	active[SourceFlag] = struct{}{}
	active[SourcePolicy] = struct{}{}

	out := make([]Source, 0, len(ordered))
	for _, s := range ordered {
		if _, ok := active[s]; ok {
			out = append(out, s)
		}
	}
	return out
}
