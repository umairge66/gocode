package toolrun

import (
	"encoding/json"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/fileedit"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// WithCompactTools makes TodoWrite, Grep and Read advertise compact
// descriptions with shorter parameter texts (the input schemas keep their
// structure) and applies the larger deferred set
// (toolsearch.IsDeferredCompact). A subagent's dispatcher takes the parent's
// value.
func WithCompactTools(on bool) Option {
	return func(d *Dispatcher) { d.compact = on }
}

// CompactTools reports whether the dispatcher advertises the compact tool
// texts.
func (d *Dispatcher) CompactTools() bool { return d.compact }

// compactText is the description and input schema advertised for a tool in
// place of the full ones when compact tool texts are on.
type compactText struct {
	description string
	inputSchema json.RawMessage
}

// compactTexts holds the compact variants by tool name.
var compactTexts = map[protocol.Tool]compactText{
	protocol.ToolTodoWrite: {interact.TodoWriteCompactPrompt, toolInputSchemas[protocol.ToolTodoWrite]},
	protocol.ToolGrep:      {filetools.GrepCompactDescription, grepCompactInputSchema},
	protocol.ToolRead:      {fileedit.ReadCompactPrompt, fileedit.ReadCompactInputSchema()},
}

// grepCompactInputSchema is the Grep input schema with shorter parameter
// descriptions; properties, types, enums and required fields match the full
// schema.
var grepCompactInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"pattern":{"type":"string","description":"Regular expression to search for"},` +
	`"path":{"type":"string","description":"File or directory to search (default: working directory)"},` +
	`"glob":{"type":"string","description":"Glob filter for files, e.g. \"*.{ts,tsx}\" (rg --glob)"},` +
	`"output_mode":{"type":"string","enum":["content","files_with_matches","count"],"description":"Default \"files_with_matches\""},` +
	`"-B":{"type":"number","description":"Lines before each match (content mode)"},` +
	`"-A":{"type":"number","description":"Lines after each match (content mode)"},` +
	`"-C":{"type":"number","description":"Alias for context."},` +
	`"context":{"type":"number","description":"Lines around each match (content mode)"},` +
	`"-n":{"type":"boolean","description":"Line numbers (content mode; default true)"},` +
	`"-i":{"type":"boolean","description":"Case insensitive"},` +
	`"-o":{"type":"boolean","description":"Only the matched parts, one per line (content mode)"},` +
	`"type":{"type":"string","description":"File type (rg --type), e.g. js, py, go"},` +
	`"head_limit":{"type":"number","description":"Max lines/entries (default 250; 0 = unlimited, use sparingly)"},` +
	`"offset":{"type":"number","description":"Skip the first N lines/entries before head_limit (default 0)"},` +
	`"multiline":{"type":"boolean","description":"Patterns span lines; . matches newlines (rg -U)"}` +
	`},"required":["pattern"]}`)
