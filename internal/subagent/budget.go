package subagent

import (
	"errors"
	"fmt"
	"strings"
)

// Spend cap: Spawner.Budget is the session's cap check. A child loop stops on
// it after the response that crossed the cap, like the main loop, and no
// spawn starts while it reports the cap reached.

// ErrSpendCap is returned (wrapped) when a spawn is refused because the
// session's spend cap is reached.
var ErrSpendCap = errors.New("Spend cap reached")

// SpendCapError is the model-facing refusal of a spawn once the session's
// spend cap is reached; cause is the budget check's error.
func SpendCapError(cause error) error {
	detail := strings.TrimPrefix(strings.TrimSpace(cause.Error()), ErrSpendCap.Error())
	detail = strings.TrimPrefix(detail, ": ")
	return fmt.Errorf("%w, so no new agent can be started (%s). Complete this task directly using your tools.", ErrSpendCap, detail)
}
