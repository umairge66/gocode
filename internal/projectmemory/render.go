package projectmemory

import "strings"

// truncationMarker is appended to Text when the total-size cap shortens it.
const truncationMarker = "\n\n[gocode.md memory truncated: total size limit reached]"

// nestedFrame wraps one source as a context block.
func nestedFrame(e Entry) string {
	return "Contents of " + e.Path + ":\n\n" + e.Content
}

// render assembles the collected entries into the final Result, applying the
// total-size cap. maxTotal < 0 disables the cap.
func (l *loader) render(maxTotal int64) Result {
	res := Result{Notes: l.notes}

	if len(l.entries) == 0 {
		return res
	}

	blocks := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		blocks = append(blocks, nestedFrame(e))
		res.Sources = append(res.Sources, e.Path)
	}
	res.Entries = l.entries
	text := strings.Join(blocks, "\n\n")

	if maxTotal >= 0 && int64(len(text)) > maxTotal {
		cut := int(maxTotal)
		if cut < 0 {
			cut = 0
		}
		text = text[:cut] + truncationMarker
		res.Truncated = true
		res.Notes = append(res.Notes, "rendered memory truncated to "+itoa64(maxTotal)+" bytes")
	}

	res.Text = text
	return res
}

// RenderEntries renders entries the way Load renders its block: one
// "Contents of <path>:" frame each, separated by a blank line.
func RenderEntries(es []Entry) string {
	blocks := make([]string, 0, len(es))
	for _, e := range es {
		blocks = append(blocks, nestedFrame(e))
	}
	return strings.Join(blocks, "\n\n")
}

// ManagedOnly keeps the Managed-tier entries and the files they import. It is
// the memory an agent definition with omitgocodeMd still receives.
func ManagedOnly(es []Entry) []Entry {
	kept := map[string]bool{}
	var out []Entry
	for _, e := range es {
		if e.Kind == KindManaged || (e.Kind == KindImport && kept[e.Parent]) {
			kept[e.Path] = true
			out = append(out, e)
		}
	}
	return out
}
