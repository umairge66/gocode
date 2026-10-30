package ui

import (
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
)

// Ranking for the "@" file-mention menu.
//
// A candidate path is matched against the query in tiers, best first:
//
//	0  exact      — the path equals the query
//	1  prefix     — the path, or one of its "/" segments, starts with the query
//	2  substring  — the query occurs contiguously in the path
//	3  fuzzy      — the query's characters occur in order (fzf's FuzzyMatchV2
//	                scoring with the path scheme: bonuses at word boundaries
//	                after / _ - . and at the path start, camelCase and digit
//	                transitions and consecutive runs; penalties for gaps)
//
// Within the prefix and substring tiers a match in the file name (the last
// segment) ranks above one only in a directory part; the fuzzy tier orders by
// score, with a bonus when the match lies wholly in the file name. Remaining
// ties go to the shorter path, then to the host's order.
//
// Matching is smart-case: a query without upper-case letters ignores case, a
// query with any upper-case letter matches case exactly.

// fileHit is one ranked candidate and the rune positions of its path the query
// matched (nil when they are not known).
type fileHit struct {
	c   FileCandidate
	pos []int
}

// fileMatchTier values (lower is better).
const (
	tierExact = iota
	tierPrefix
	tierSubstring
	tierFuzzy
)

var fuzzyInit sync.Once

// initFuzzy selects fzf's path scoring scheme once, before the first match.
func initFuzzy() { fuzzyInit.Do(func() { algo.Init("path") }) }

// rankCache memoises the last ranking: the menu reads the filtered set several
// times per keystroke (activity check, selection, window, render), and the
// candidate slice is replaced, never mutated, when the host refreshes it.
var rankCache struct {
	mu    sync.Mutex
	query string
	first *FileCandidate
	n     int
	hits  []fileHit
}

// rankFilesCached is rankFiles, reusing the previous result for the same query
// over the same candidate slice.
func rankFilesCached(query string, files []FileCandidate) []fileHit {
	var first *FileCandidate
	if len(files) > 0 {
		first = &files[0]
	}
	rankCache.mu.Lock()
	if rankCache.hits != nil && rankCache.query == query && rankCache.first == first && rankCache.n == len(files) {
		hits := rankCache.hits
		rankCache.mu.Unlock()
		return hits
	}
	rankCache.mu.Unlock()
	hits := rankFiles(query, files)
	if hits == nil {
		hits = []fileHit{}
	}
	rankCache.mu.Lock()
	rankCache.query, rankCache.first, rankCache.n, rankCache.hits = query, first, len(files), hits
	rankCache.mu.Unlock()
	return hits
}

// rankFiles returns the candidates matching query, best first, capped at
// maxFilteredFiles.
func rankFiles(query string, files []FileCandidate) []fileHit {
	if query == "" {
		return nil
	}
	caseSensitive := hasUpper(query)
	q := query
	if !caseSensitive {
		q = strings.ToLower(query)
	}
	qRunes := []rune(q)

	type scored struct {
		hit   fileHit
		tier  int
		sub   int // name match (0) before directory-only match (1); fuzzy: -score
		width int
		idx   int
	}
	var matched []scored
	var slab *util.Slab
	for i, c := range files {
		p := c.Path
		if !caseSensitive {
			p = strings.ToLower(p)
		}
		tier, sub, off, ok := literalMatch(q, p)
		var pos []int
		if ok {
			pos = literalPositions(c.Path, p, off, len(qRunes))
		} else {
			if slab == nil {
				initFuzzy()
				slab = util.MakeSlab(100*1024, 2048)
			}
			score, fpos, inName, ok := fuzzyMatch(c.Path, qRunes, caseSensitive, slab)
			if !ok {
				continue
			}
			tier, pos = tierFuzzy, fpos
			if inName {
				score += fuzzyNameBonus
			}
			sub = -score
		}
		matched = append(matched, scored{
			hit:   fileHit{c: c, pos: pos},
			tier:  tier,
			sub:   sub,
			width: utf8.RuneCountInString(c.Path),
			idx:   i,
		})
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.sub != b.sub {
			return a.sub < b.sub
		}
		if a.width != b.width {
			return a.width < b.width
		}
		return a.idx < b.idx
	})
	if len(matched) > maxFilteredFiles {
		matched = matched[:maxFilteredFiles]
	}
	out := make([]fileHit, len(matched))
	for i := range matched {
		out[i] = matched[i].hit
	}
	return out
}

// fuzzyNameBonus is added to the score of a fuzzy match that lies wholly in
// the file name (in fzf's units a matched character scores 16 and a word
// boundary 8), so a file whose own name carries the query beats one whose
// match leans on a shared directory prefix such as "internal/", without
// overriding a clearly better path match.
const fuzzyNameBonus = 12

// fuzzyMatch scores the query against path with fzf's FuzzyMatchV2 (path
// scheme). When the query also matches within the file name alone, that
// alignment is used if it scores at least as well, and inName reports it, so a
// file whose own name carries the query is preferred over one whose match is
// spread over shared directory prefixes. pos are rune positions in path.
func fuzzyMatch(path string, q []rune, caseSensitive bool, slab *util.Slab) (score int, pos []int, inName, ok bool) {
	chars := util.ToChars([]byte(path))
	res, fpos := algo.FuzzyMatchV2(caseSensitive, false, true, &chars, q, true, slab)
	if res.Start < 0 {
		return 0, nil, false, false
	}
	score, pos = res.Score, sortedCopy(fpos)
	nameAt := strings.LastIndexByte(path, '/') + 1
	if nameAt == 0 {
		return score, pos, true, true
	}
	name := util.ToChars([]byte(path[nameAt:]))
	nres, npos := algo.FuzzyMatchV2(caseSensitive, false, true, &name, q, true, slab)
	if nres.Start < 0 {
		return score, pos, false, true
	}
	if nres.Score >= score {
		shift := utf8.RuneCountInString(path[:nameAt])
		pos = sortedCopy(npos)
		for i := range pos {
			pos[i] += shift
		}
		score = nres.Score
	}
	return score, pos, true, true
}

// sortedCopy returns the positions fzf reported, ascending.
func sortedCopy(p *[]int) []int {
	if p == nil {
		return nil
	}
	out := append([]int(nil), (*p)...)
	sort.Ints(out)
	return out
}

// literalMatch finds q in path (both already case-folded as the query
// demands) as an exact, prefix or substring match. sub is 0 when the match
// lies in the file name (last segment) and 1 when it only touches directory
// parts; off is the byte offset of the match in path.
func literalMatch(q, path string) (tier, sub, off int, ok bool) {
	if path == q {
		return tierExact, 0, 0, true
	}
	nameAt := strings.LastIndexByte(path, '/') + 1
	name := path[nameAt:]
	if strings.HasPrefix(name, q) {
		return tierPrefix, 0, nameAt, true
	}
	if strings.HasPrefix(path, q) {
		return tierPrefix, 1, 0, true
	}
	for i := 0; i < nameAt; i++ {
		if path[i] == '/' && strings.HasPrefix(path[i+1:], q) {
			return tierPrefix, 1, i + 1, true
		}
	}
	if j := strings.Index(name, q); j >= 0 {
		return tierSubstring, 0, nameAt + j, true
	}
	if j := strings.Index(path, q); j >= 0 {
		return tierSubstring, 1, j, true
	}
	return 0, 0, 0, false
}

// literalPositions converts a literal match at byte offset off of the
// case-folded path into rune positions of the unfolded path. When case
// folding changed the path's byte layout the positions are unknown (nil).
func literalPositions(raw, folded string, off, n int) []int {
	if len(raw) != len(folded) {
		return nil
	}
	start := utf8.RuneCountInString(folded[:off])
	pos := make([]int, n)
	for i := range pos {
		pos[i] = start + i
	}
	return pos
}

// hasUpper reports whether s holds an upper-case letter.
func hasUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
