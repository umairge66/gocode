package settingsload

import "path"

// MacManagedPreferencesDomain is the preferences domain the macOS managed
// configuration profile targets.
//
// UNVERIFIED: the domain is derived from the product id
// (com.<vendor>.<product>_code → "com.gocode.gocode_code") rather than
// confirmed. Confirm against a macOS build before relying on this constant.
const MacManagedPreferencesDomain = "com.gocode.gocodecode"

// macManagedPreferencesRoot is where macOS materializes MDM-delivered
// configuration profiles; only an administrator / the MDM agent can write
// below it.
const macManagedPreferencesRoot = "/Library/Managed Preferences"

// MacManagedPlistPaths lists the managed-preferences documents in priority
// order: the per-user managed profile first, then the device-wide one. Both
// are administrator-controlled (userWritable false). A user-writable
// ~/Library/Preferences document, which the composition supports as a
// userWritable entry, is NOT read: its path is not known either, and
// adding one would let the user account author policy.
func MacManagedPlistPaths(username string) []string {
	file := MacManagedPreferencesDomain + ".plist"
	var out []string
	if username != "" {
		out = append(out, path.Join(macManagedPreferencesRoot, username, file))
	}
	return append(out, path.Join(macManagedPreferencesRoot, file))
}

// plistDocumentsFrom builds the readout's plist documents: every existing
// path is converted to JSON; a document that exists but cannot be read or
// converted is carried with its reason, reported as "could not be read".
func plistDocumentsFrom(paths []string, read func(string) ([]byte, error), exists func(string) bool) []PlistDocument {
	var docs []PlistDocument
	for _, p := range paths {
		if !exists(p) {
			continue
		}
		doc := PlistDocument{Label: p}
		data, err := read(p)
		if err == nil {
			var js string
			if js, err = PlistToJSON(data); err == nil {
				doc.JSON = &js
			}
		}
		if err != nil {
			doc.UnreadReason = err.Error()
		}
		docs = append(docs, doc)
	}
	return docs
}
