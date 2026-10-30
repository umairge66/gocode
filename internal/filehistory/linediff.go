package filehistory

import "strings"

// lcsBudget bounds the line-LCS work (rows × columns) of one diff count; a
// larger change falls back to counting every differing line in the window.
const lcsBudget = 4_000_000

// lineDiffCounts returns how many lines going from a to b inserts and deletes
// (a line-level diff; only the counts are needed for the "+N -M" stats).
func lineDiffCounts(a, b string) (insertions, deletions int) {
	if a == b {
		return 0, 0
	}
	al, bl := splitLines(a), splitLines(b)
	// Trim the common prefix and suffix.
	for len(al) > 0 && len(bl) > 0 && al[0] == bl[0] {
		al, bl = al[1:], bl[1:]
	}
	for len(al) > 0 && len(bl) > 0 && al[len(al)-1] == bl[len(bl)-1] {
		al, bl = al[:len(al)-1], bl[:len(bl)-1]
	}
	if len(al) == 0 || len(bl) == 0 || len(al)*len(bl) > lcsBudget {
		return len(bl), len(al)
	}
	// Two-row LCS length.
	prev := make([]int, len(bl)+1)
	cur := make([]int, len(bl)+1)
	for i := 1; i <= len(al); i++ {
		for j := 1; j <= len(bl); j++ {
			switch {
			case al[i-1] == bl[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	common := prev[len(bl)]
	return len(bl) - common, len(al) - common
}

// splitLines splits on newlines, ignoring a trailing one.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
