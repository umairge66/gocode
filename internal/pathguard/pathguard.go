// Package pathguard holds the self-contained, syntactic path-safety core of
// gocode: the file-path permission / sandbox access-control logic.
// Its primary responsibility is deciding whether a requested file path
// may be read / written / created, starting with purely *syntactic* gates
// (UNC, tilde, shell-expansion, `..` traversal, brace-expansion, glob) before
// any filesystem resolution.
//
// SCOPE. The pure string/path utilities and the syntactic decision gates live
// here. The allowOnly / allowAllExcept / allBeneathItsDirectory rule engine
// sits behind the RuleEngine interface, implemented by RuleSetEngine
// (ruleset.go).
package pathguard

import (
	"strings"
)

// Operation is the kind of filesystem access being requested for a path.
type Operation string

const (
	// OpRead requests read access.
	OpRead Operation = "read"
	// OpWrite requests write access to an existing target.
	OpWrite Operation = "write"
	// OpCreate requests creation of a new target.
	OpCreate Operation = "create"
)

// isMutating reports whether op modifies the filesystem (write or create).
func (op Operation) isMutating() bool { return op == OpWrite || op == OpCreate }

// Mutating reports whether op writes (OpWrite or OpCreate).
func (op Operation) Mutating() bool { return op.isMutating() }

// DecisionReason explains why a path check resolved the way it did: the
// `decisionReason` object attached to every refusal.
type DecisionReason struct {
	// Type is the reason category, e.g. "other", "safetyCheck", "rule".
	Type string
	// Reason is the human-readable explanation surfaced to the user.
	Reason string
	// ClassifierApprovable indicates whether an automated classifier may
	// approve this path; false forces manual approval.
	ClassifierApprovable bool
	// CircuitBreaker names an internal circuit-breaker bucket, when set.
	CircuitBreaker string
}

// Decision is the outcome of a path check: whether access is allowed, the path
// as resolved so far, and (on refusal) the reason.
type Decision struct {
	Allowed      bool
	ResolvedPath string
	Reason       *DecisionReason
}

// Reason category constants (DecisionReason.Type values).
const (
	ReasonTypeOther       = "other"
	ReasonTypeSafetyCheck = "safetyCheck"
	ReasonTypeRule        = "rule"
)

// -----------------------------------------------------------------------------
// low-level helpers
// -----------------------------------------------------------------------------

// isSep reports whether c is a path separator ("/" or "\").
func isSep(c byte) bool { return c == '/' || c == '\\' }

// toSlashPath converts OS backslashes to forward slashes, matching the POSIX
// slash convention the fsfacade walker expects.
func toSlashPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }
