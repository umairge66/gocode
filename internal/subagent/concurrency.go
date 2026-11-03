package subagent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Spawn admission: besides the nesting cap (spawndepth.go) and the spend
// cap (budget.go), a spawn is refused when the session already runs
// MaxConcurrent subagents.

// DefaultMaxConcurrentSubagents caps the subagents running at once when
// GOCODE_MAX_CONCURRENT_SUBAGENTS does not set another limit.
const DefaultMaxConcurrentSubagents = 20

// MaxConcurrentEnv names the variable that overrides
// DefaultMaxConcurrentSubagents.
const MaxConcurrentEnv = "GOCODE_MAX_CONCURRENT_SUBAGENTS"

// ErrConcurrentLimit is returned (wrapped) when a spawn would exceed the
// concurrent-subagent cap.
var ErrConcurrentLimit = errors.New("Concurrent subagent limit reached")

// MaxConcurrentFromEnv returns the cap GOCODE_MAX_CONCURRENT_SUBAGENTS sets:
// a whole number of at least 1 written in digits only. It returns 0 (use
// DefaultMaxConcurrentSubagents) when the variable is unset or malformed.
func MaxConcurrentFromEnv(getenv func(string) string) int {
	if getenv == nil {
		return 0
	}
	v := strings.TrimSpace(getenv(MaxConcurrentEnv))
	if v == "" || len(v) > 9 {
		return 0
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// maxConcurrent returns the configured concurrent-subagent cap.
func (s *Spawner) maxConcurrent() int {
	if s.MaxConcurrent > 0 {
		return s.MaxConcurrent
	}
	return DefaultMaxConcurrentSubagents
}

// TryTakeConcurrencySlot takes a slot unless max are already held (max <= 0
// means no cap). ok is false, and nothing is taken, when the cap is reached;
// otherwise release frees the slot exactly once.
func (r *TaskRegistry) TryTakeConcurrencySlot(max int) (release func(), ok bool) {
	r.mu.Lock()
	if max > 0 && r.running >= max {
		r.mu.Unlock()
		return nil, false
	}
	r.running++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.running > 0 {
				r.running--
			}
			r.mu.Unlock()
		})
	}, true
}

// ConcurrentLimitError is the model-facing refusal of a spawn past the cap
// of max running agents.
func ConcurrentLimitError(max int) error {
	return fmt.Errorf("%w (%d agents are already running). Wait for a running agent to finish, or complete this task directly using your tools. To allow more, raise %s.", ErrConcurrentLimit, max, MaxConcurrentEnv)
}
