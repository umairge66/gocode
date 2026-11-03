// Package headless holds the machine-readable output of print mode
// (`gocode -p`): the output and input format choices, the newline-delimited
// JSON event records (system/init, assistant, user, stream_event, the
// compact_boundary / api_retry / task_* system records, result), the final
// result record, the stream-json input control protocol (control.go), and
// the JSON Schema validation behind --json-schema structured output. The
// protocol is described in docs/headless.md.
//
// Nothing here performs I/O beyond writing records to the writer it is given;
// in particular nothing is ever sent anywhere.
package headless

import (
	"fmt"
	"strings"
)

// Output formats for `--output-format`.
const (
	FormatText       = "text"
	FormatJSON       = "json"
	FormatStreamJSON = "stream-json"
)

// OutputFormats are the accepted `--output-format` values, in help order.
var OutputFormats = []string{FormatText, FormatJSON, FormatStreamJSON}

// InputFormats are the accepted `--input-format` values, in help order.
var InputFormats = []string{FormatText, FormatStreamJSON}

// Option specs as shown in help and in choice errors.
const (
	OutputFormatSpec = "--output-format <format>"
	InputFormatSpec  = "--input-format <format>"
)

// InvalidChoice renders the error for an option value outside its choices:
//
//	error: option '--output-format <format>' argument 'xml' is invalid. Allowed choices are text, json, stream-json.
func InvalidChoice(spec, value string, choices []string) string {
	return fmt.Sprintf("error: option '%s' argument '%s' is invalid. Allowed choices are %s.", spec, value, strings.Join(choices, ", "))
}

// ValidOutputFormat reports whether f is an accepted `--output-format` value.
func ValidOutputFormat(f string) bool { return contains(OutputFormats, f) }

// ValidInputFormat reports whether f is an accepted `--input-format` value.
func ValidInputFormat(f string) bool { return contains(InputFormats, f) }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Startup validation messages for the print-mode output flags.
const (
	ErrStreamJSONNeedsVerbose       = "Error: When using --print, --output-format=stream-json requires --verbose"
	ErrInputStreamNeedsOutputStream = "Error: --input-format=stream-json requires output-format=stream-json."
	ErrPartialNeedsStreamJSON       = "Error: --include-partial-messages requires --print and --output-format=stream-json."
	ErrJSONSchemaNotObject          = "Error: --json-schema must be a JSON object"
	ErrNoResult                     = "Error: No messages returned from query"
	ErrNoSessionPersistencePrint    = "Error: --no-session-persistence can only be used with --print mode."
	ErrInvalidSessionID             = "Error: Invalid session ID. Must be a valid UUID."
	ErrSessionIDNeedsFork           = "Error: --session-id can only be used with --continue or --resume if --fork-session is also specified."
)

// ErrJSONSchemaInvalid renders the error for a --json-schema value that does
// not parse as JSON.
func ErrJSONSchemaInvalid(err error) string {
	return "Error: --json-schema is not valid JSON: " + err.Error()
}

// ErrSessionIDInUse renders the error for a --session-id whose transcript
// already exists.
func ErrSessionIDInUse(id string) string {
	return fmt.Sprintf("Error: Session ID %s is already in use.", id)
}
