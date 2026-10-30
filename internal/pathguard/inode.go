// This file holds the inode / filesystem-identity helpers used by path vetting.

package pathguard

import (
	"path/filepath"
	"strings"
)

// splitNonEmpty splits an absolute path into its non-empty components
// (splitting on runs of "/" and dropping empties).
func splitNonEmpty(p string) []string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
