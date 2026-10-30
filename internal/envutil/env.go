// This file holds the environment/argv helpers (GOCODE_ variable lookups,
// KEY=value assignment parsing, NODE_OPTIONS / argv flag checks,
// project-dir-name validation, the max-turns limit and two hard-wired feature
// gates) and the process-cwd accessor. Number parsers are in internal/textutil
// (numbers.go) and Vertex-region helpers in internal/provider (vertexregion.go).

package envutil

import (
	"fmt"
	"math"
	"strings"

	"github.com/umairge66/gocode/internal/textutil"
)

// MaxTurnsFromEnv parses GOCODE_CODE_MAX_TURNS (used when no --max-turns
// value is given): unset or blank means no limit (0); otherwise the value, read with
// ParseGroupedNumber, must be a positive integer or an error is returned.
func MaxTurnsFromEnv(v string) (int, error) {
	n := strings.TrimSpace(v)
	if n == "" {
		return 0, nil
	}
	r := textutil.ParseGroupedNumber(n)
	if math.IsNaN(r) || math.IsInf(r, 0) || r != math.Trunc(r) || r <= 0 {
		return 0, fmt.Errorf("GOCODE_CODE_MAX_TURNS must be a positive integer; got \"%s\"", n)
	}
	if r > math.MaxInt32 { // any larger cap is "unbounded" in practice
		r = math.MaxInt32
	}
	return int(r), nil
}
