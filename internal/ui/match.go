package ui

import (
	"sort"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/list"
)

// Search for the modern pickers and the command palette.
//
// A pure fuzzy (subsequence) match over an entry's whole text is far too
// permissive for lists with long descriptions: OpenRouter's catalog describes
// every model in a paragraph, so "free" is a subsequence of almost every
// entry and the list barely narrows past the first letter. So entries are
// matched word by word on contiguous text: every query word must occur in the
// entry's name, id or description, with name/id hits ranked first (prefix hits
// above the rest), description-only hits after. Only when nothing contains the
// query does the Bubbles list's fuzzy matcher (subsequences, typo-tolerant)
// run, and then only over the names and ids — never the descriptions.

// searchEntry is one entry's searchable text; low holds it lower-cased
// (filled by prepare, so a list is lower-cased once, not per keystroke).
type searchEntry struct {
	label, id, detail string
	low               *[3]string
}

// prepare lower-cases the entries' text once.
func prepare(entries []searchEntry) []searchEntry {
	for i := range entries {
		e := &entries[i]
		e.low = &[3]string{strings.ToLower(e.label), strings.ToLower(e.id), strings.ToLower(e.detail)}
	}
	return entries
}

// searchHit is a ranked match: the entry index and the matched label runes.
type searchHit struct {
	index int
	match []int
}

// searchEntries ranks entries for query (see above). An empty query matches
// every entry in order.
func searchEntries(query string, entries []searchEntry) []searchHit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		out := make([]searchHit, len(entries))
		for i := range entries {
			out[i] = searchHit{index: i}
		}
		return out
	}
	type ranked struct {
		searchHit
		tier int
	}
	var hits []ranked
	for i, e := range entries {
		if e.low == nil {
			prepare(entries[i : i+1])
			e = entries[i]
		}
		label, id, detail := e.low[0], e.low[1], e.low[2]
		tier := 0
		for _, w := range words {
			switch {
			case wordPrefix(label, w) || wordPrefix(id, w):
				// tier stays (0 when every word starts a word in the name/id)
			case strings.Contains(label, w) || strings.Contains(id, w):
				tier = max(tier, 1)
			case strings.Contains(detail, w):
				tier = max(tier, 2)
			default:
				tier = -1
			}
			if tier < 0 {
				break
			}
		}
		if tier >= 0 {
			hits = append(hits, ranked{searchHit{index: i, match: labelMatches(e.label, words)}, tier})
		}
	}
	if len(hits) == 0 {
		// Nothing contains the query: fall back to fuzzy subsequences over the
		// names and ids (a typo, or initials such as "cs45").
		targets := make([]string, len(entries))
		for i, e := range entries {
			targets[i] = e.label + "  " + e.id
		}
		var out []searchHit
		for _, r := range list.DefaultFilter(query, targets) {
			out = append(out, searchHit{index: r.Index, match: runeIndexes(entries[r.Index].label, r.MatchedIndexes)})
		}
		return out
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].tier < hits[b].tier })
	out := make([]searchHit, len(hits))
	for i, h := range hits {
		out[i] = h.searchHit
	}
	return out
}

// wordPrefix reports whether w starts s or a word inside s (after a space or
// punctuation such as "/", "-", ":" or "(").
func wordPrefix(s, w string) bool {
	for i := 0; i <= len(s)-len(w); {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 {
			return true
		}
		prev := rune(s[at-1])
		if !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
			return true
		}
		i = at + 1
	}
	return false
}

// labelMatches returns the rune indexes of the query words' first occurrence
// in label (case-insensitive), for highlighting.
func labelMatches(label string, words []string) []int {
	lr := []rune(strings.ToLower(label))
	if len(lr) != len([]rune(label)) {
		return nil // case mapping changed the length; skip the highlight
	}
	var out []int
	for _, w := range words {
		wr := []rune(w)
		for i := 0; i+len(wr) <= len(lr); i++ {
			if string(lr[i:i+len(wr)]) == w {
				for k := range wr {
					out = append(out, i+k)
				}
				break
			}
		}
	}
	sort.Ints(out)
	return out
}
