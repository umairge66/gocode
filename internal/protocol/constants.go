// Shared protocol constants: size and count limits, timeouts, exit codes,
// identifiers and messages. The *values* are a stable contract.
package protocol

// --- Byte-size limits ------------------------------------------------------

// --- Count limits ----------------------------------------------------------

// --- Timeouts and intervals (milliseconds) ---------------------------------

const (
	// MaxInt32 is the maximum signed 32-bit integer. It is used throughout as
	// an "effectively unbounded" timeout clamp (Math.min/max, setTimeout, and
	// an upper bound for integer validation).
	MaxInt32 = 2147483647
)

// --- Process exit codes (BSD sysexits.h) -----------------------------------

// --- Filenames -------------------------------------------------------------

// --- Mode / kind strings ---------------------------------------------------

// Tool names used by the deferred-tool-loading mechanism. They reuse the Tool
// type declared in kinds.go.
const (
	// ToolSearch is the meta-tool that loads deferred tool schemas on demand.
	ToolSearch Tool = "ToolSearch"
)
