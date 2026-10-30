package cli

import (
	"encoding/json"
	"testing"
)

// Read/Grep/Glob results show a one-line summary in the
// transcript; the model still gets the full result.
func TestDisplayToolResultSummaries(t *testing.T) {
	for _, tc := range []struct {
		name, input, result, want string
	}{
		{"Read", `{"file_path":"a.go"}`, "     1\tpackage a\n     2\t\n     3\tfunc A() {}\n", "Read 3 lines"},
		{"Read", `{"file_path":"a.go"}`, "     1\tpackage a\n", "Read 1 line"},
		{"Read", `{"file_path":"x.png"}`, "[image]", "[image]"},
		{"Glob", `{"pattern":"**/*.go"}`, "a.go\nb.go\n(Results are truncated to 2 of 9 files; consider a more specific pattern.)", "Found 2 files"},
		{"Glob", `{"pattern":"*.zz"}`, "No files found", "Found 0 files"},
		{"Grep", `{"pattern":"x"}`, "a.go\nb.go\nc.go", "Found 3 files"},
		{"Grep", `{"pattern":"x"}`, "No matches found", "Found 0 files"},
		{"Grep", `{"pattern":"x","output_mode":"content"}`, "a.go:1:x\na.go:4:x", "Found 2 lines"},
		{"Grep", `{"pattern":"x","output_mode":"count"}`, "a.go:3\nb.go:1", "Found 4 matches across 2 files"},
		{"Read", `{}`, "<tool_use_error>File does not exist.</tool_use_error>", "<tool_use_error>File does not exist.</tool_use_error>"},
		{"Bash", `{"command":"ls"}`, "<tool_use_error>The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed.</tool_use_error>", "User rejected tool use"},
		{"Bash", `{"command":"ls"}`, "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed.", "User rejected tool use"},
		{"Edit", `{}`, "<tool_use_error>The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\nkeep the old name</tool_use_error>", "User rejected tool use: keep the old name"},
	} {
		if got := displayToolResult(tc.name, json.RawMessage(tc.input), tc.result); got != tc.want {
			t.Errorf("%s %s: got %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}
