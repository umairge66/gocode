package toolsearch

import (
	"encoding/json"
	"strings"
)

// Description is the ToolSearch tool's description.
const Description = "Fetches full schema definitions for deferred tools so they can be called.\n\n" +
	"Deferred tools appear by name in <system-reminder> messages." +
	" Until fetched, only the name is known — there is no parameter schema, so calling the tool fails with InputValidationError." +
	" When any instruction, system reminder, or other tool's description names a deferred tool, fetch it with query \"select:<name>\" before calling it." +
	" This tool takes a query, matches it against the deferred tool list, and loads the matched tools' complete JSONSchema definitions." +
	" Once a tool's schema is loaded, it is callable exactly like any tool defined at the top of the prompt.\n\n" +
	"Result format: the result names each matched tool, and from then on its full definition is available exactly like the other tools' definitions.\n\n" +
	"Query forms:\n" +
	"- \"select:Read,Edit,Grep\" — fetch these exact tools by name\n" +
	"- \"notebook jupyter\" — keyword search, up to max_results best matches\n" +
	"- \"+slack send\" — require \"slack\" in the name, rank by remaining terms"

// InputSchema is the ToolSearch tool's input schema.
var InputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"query":{"type":"string","description":"Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search."},` +
	`"max_results":{"type":"number","default":5,"description":"Maximum number of results to return (default: 5)"}` +
	`},"required":["query"]}`)

// Input is the decoded ToolSearch input.
type Input struct {
	Query      string   `json:"query"`
	MaxResults *float64 `json:"max_results,omitempty"`
}

// NoMatches is the result text when nothing matched.
const NoMatches = "No matching deferred tools found"

// ResultText is the text of a ToolSearch result that loaded names. A provider
// with native deferred loading replaces it with tool_reference blocks.
func ResultText(names []string) string {
	if len(names) == 0 {
		return NoMatches
	}
	return "Loaded " + strings.Join(names, ", ") +
		". Their full definitions are now available; call them directly."
}
