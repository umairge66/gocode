package cli

import (
	"fmt"
	"math"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/textutil"
)

// --max-turns <turns>: the maximum number of agentic turns in print mode.

// parseMaxTurnsFlag validates a --max-turns value.
func parseMaxTurnsFlag(v string) (int, error) {
	n := strings.TrimSpace(v)
	r := textutil.ParseGroupedNumber(n)
	if n == "" || math.IsNaN(r) || math.IsInf(r, 0) || r != math.Trunc(r) || r <= 0 {
		return 0, fmt.Errorf("--max-turns must be a positive integer; got \"%s\"", n)
	}
	if r > math.MaxInt32 {
		r = math.MaxInt32
	}
	return int(r), nil
}

// resolveMaxTurns returns the --max-turns limit: the flag when given, else the
// environment variable; 0 means no limit.
func resolveMaxTurns(env *Env, d *DispatchDefaults) (int, error) {
	if d != nil && d.MaxTurnsSet {
		return parseMaxTurnsFlag(d.MaxTurns)
	}
	return envutil.MaxTurnsFromEnv(envGetenv(env)("GOCODE_CODE_MAX_TURNS"))
}
