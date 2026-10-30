package configimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// Picker policy shared by the interactive and non-interactive import surfaces:
// which items are pre-selected, which are "held back" from an unattended
// --yes, the display order, and the scan digest that binds a --yes=<digest>
// confirmation to exactly the preview the user saw.

// DefaultSelected reports whether an item is checked by default and imported
// by an unattended --yes: user scope, no warning, and not a skill (skills copy
// whole directories with scripts, so they always need an explicit choice).
func DefaultSelected(it ImportItem) bool {
	return it.Scope == ScopeUser && it.Warning == "" && it.Kind != KindSkill
}

// HoldBack classifies an item an unattended --yes will not import: "project"
// (repo-authored config), "warned" (warning-flagged or a skill), or "".
func HoldBack(it ImportItem) string {
	if it.Scope == ScopeProject {
		return "project"
	}
	if it.Warning != "" || it.Kind == KindSkill {
		return "warned"
	}
	return ""
}

// kindOrder is the picker's grouping order.
var kindOrder = map[ItemKind]int{
	KindInstructions: 0, KindSetting: 1, KindSubagent: 2, KindCommand: 3, KindMCP: 4, KindSkill: 5,
}

// LessItems orders items for display: by kind, then project before user, then
// by label.
func LessItems(a, b ImportItem) bool {
	if d := kindOrder[a.Kind] - kindOrder[b.Kind]; d != 0 {
		return d < 0
	}
	if a.Scope != b.Scope {
		return a.Scope == ScopeProject
	}
	return a.Label < b.Label
}

// ScanDigest is a 32-hex-char SHA-256 over the scans' items and unmappable
// entries (sorted, so it is independent of discovery order). A --yes=<digest>
// confirmation is refused unless the current scan still hashes to it.
func ScanDigest(scans []ImportResult) string {
	h := sha256.New()
	sorted := append([]ImportResult(nil), scans...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SourceID < sorted[j].SourceID })
	for _, sc := range sorted {
		h.Write([]byte(string(sc.SourceID)))
		h.Write([]byte{0})
		items := append([]ImportItem(nil), sc.Items...)
		sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		for _, it := range items {
			h.Write(jsonArray(it.ID, string(it.Kind), string(it.Scope), it.Label, it.Description, it.Warning, it.Fingerprint))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
		un := append([]UnmappableItem(nil), sc.Unmappable...)
		sort.SliceStable(un, func(i, j int) bool {
			a, b := un[i], un[j]
			if a.Label != b.Label {
				return a.Label < b.Label
			}
			if a.Reason != b.Reason {
				return a.Reason < b.Reason
			}
			return a.Scope < b.Scope
		})
		for _, u := range un {
			h.Write(jsonArray(string(u.Scope), u.Label, u.Reason))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func jsonArray(vals ...string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(vals)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// IsDigest reports whether s has the shape of a scan digest.
func IsDigest(s string) bool {
	if len(s) != 32 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}
