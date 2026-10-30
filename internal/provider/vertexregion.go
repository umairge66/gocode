// Vertex AI region helpers: region token validation and endpoint
// construction.

package provider

import "regexp"

// regionRe matches a region-like token: 2+ letters, then up to 4 "-segment"
// groups of letters/digits (case-insensitive).
var regionRe = regexp.MustCompile(`(?i)^[a-z]{2,}(?:-[a-z0-9]+){0,4}$`)

// IsRegionLike reports whether e is a non-empty region-like token.
func IsRegionLike(e string) bool { return e != "" && regionRe.MatchString(e) }

// VertexEndpoint returns the Vertex AI API endpoint URL for the given region:
//
//	global      https://aiplatform.googleapis.com
//	us / eu     https://aiplatform.<region>.rep.googleapis.com
//	otherwise   https://<region>-aiplatform.googleapis.com
func VertexEndpoint(region string) string {
	switch region {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return "https://aiplatform." + region + ".rep.googleapis.com"
	default:
		return "https://" + region + "-aiplatform.googleapis.com"
	}
}
