package insights

import (
	"regexp"
	"strings"
	"testing"
)

// The report page must open fully offline: no stylesheet, script, font or
// image is loaded from the network.
func TestReportPageLoadsNothingRemote(t *testing.T) {
	page := strings.Join(reportPageTpl, "")
	remote := regexp.MustCompile(`(?i)(<link[^>]+href=|<script[^>]+src=|<img[^>]+src=|url\(|@import)\s*["']?(https?:)?//`)
	if m := remote.FindString(page); m != "" {
		t.Fatalf("report page loads a remote resource: %q", m)
	}
}
