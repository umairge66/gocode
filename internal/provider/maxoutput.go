package provider

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MaxOutputEnv lets the user raise (or lower) how many output tokens one reply
// may use, up to the model's limit.
const MaxOutputEnv = "GOCODE_MAX_OUTPUT_TOKENS"

// getenv reads the environment (a seam for tests).
var getenv = os.Getenv

// MaxOutput is the resolved per-reply output cap.
type MaxOutput struct {
	// Tokens is the cap to send (0 = leave it to the provider).
	Tokens int
	// FromEnv reports that Tokens came from GOCODE_MAX_OUTPUT_TOKENS.
	FromEnv bool
	// Note explains an ignored or capped value ("" when the value was used as
	// given or the variable is unset).
	Note string
}

// ResolveMaxOutput applies GOCODE_MAX_OUTPUT_TOKENS (raw, "" when unset) to the
// model's capabilities: unset → the model's default; not
// a positive number → the default; above the model's limit → the limit.
func ResolveMaxOutput(caps Capabilities, raw string) MaxOutput {
	def := caps.MaxOutputTokens
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return MaxOutput{Tokens: def}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return MaxOutput{Tokens: def, Note: fmt.Sprintf("%s: invalid value %q (using default: %d)", MaxOutputEnv, raw, def)}
	}
	if caps.MaxOutputLimit > 0 && n > caps.MaxOutputLimit {
		return MaxOutput{Tokens: caps.MaxOutputLimit, FromEnv: true, Note: fmt.Sprintf("%s: capped from %d to %d", MaxOutputEnv, n, caps.MaxOutputLimit)}
	}
	return MaxOutput{Tokens: n, FromEnv: true}
}
