package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ToolNameMultiEdit is the MultiEdit tool name.
const ToolNameMultiEdit = "MultiEdit"

// MultiEditPrompt is the model-facing usage text, describing the shared
// applier's semantics.
const MultiEditPrompt = "Makes multiple exact string replacements in one file in a single, atomic operation. Prefer it over Edit when you need several edits to the same file.\n\nUsage:" +
	"\n- You must use your `Read` tool at least once in the conversation before editing. This tool will error if you attempt an edit without reading the file." +
	"\n- `edits` is an array of {old_string, new_string, replace_all} objects with the same rules as the Edit tool's parameters." +
	"\n- Edits are applied in order, each one to the result of the previous edit." +
	"\n- The operation is atomic: if any edit fails, none are applied and the error names the failing edit's index." +
	"\n- An edit's old_string must not be a substring of an earlier edit's new_string." +
	"\n- To create a new file, give a first edit with an empty old_string and the file contents as new_string."

var multiEditInputSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"file_path":{"type":"string","description":"The absolute path to the file to modify"},` +
	`"edits":{"type":"array","minItems":1,"description":"Array of edit operations to perform sequentially on the file","items":{"type":"object","properties":{` +
	`"old_string":{"type":"string","description":"The text to replace"},` +
	`"new_string":{"type":"string","description":"The text to replace it with (must be different from old_string)"},` +
	`"replace_all":{"type":"boolean","default":false,"description":"Replace all occurrences of old_string (default false)"}` +
	`},"required":["old_string","new_string"],"additionalProperties":false}}` +
	`},"required":["file_path","edits"],"additionalProperties":false}`)

// EditOperation is one entry of MultiEditInput.Edits.
type EditOperation struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// MultiEditInput is the MultiEdit input.
type MultiEditInput struct {
	FilePath string          `json:"file_path"`
	Edits    []EditOperation `json:"edits"`
}

// MultiEditResult reports what MultiEdit did.
type MultiEditResult struct {
	FilePath     string
	Edits        []EditOperation // as actually applied (normalized strings)
	OriginalFile string
	UpdatedFile  string
	Created      bool
	Text         string
}

// ErrNoEdits is returned for an empty edits array.
var ErrNoEdits = errors.New("edits must contain at least one edit")

// MultiEditTool applies several edits to one file atomically.
type MultiEditTool struct {
	// Tracker gates edits like EditTool.Tracker.
	Tracker *FileStateTracker
}

// Name returns the tool name.
func (MultiEditTool) Name() string { return ToolNameMultiEdit }

// Prompt returns the model-facing usage text.
func (MultiEditTool) Prompt() string { return MultiEditPrompt }

// InputSchema returns the JSON input schema.
func (MultiEditTool) InputSchema() json.RawMessage { return multiEditInputSchema }

// Execute applies in.Edits in order; nothing is written unless all succeed.
func (t MultiEditTool) Execute(ctx context.Context, in MultiEditInput) (MultiEditResult, error) {
	if err := ctx.Err(); err != nil {
		return MultiEditResult{}, err
	}
	if len(in.Edits) == 0 {
		return MultiEditResult{}, ErrNoEdits
	}
	ops := make([]editOp, len(in.Edits))
	for i, e := range in.Edits {
		ops[i] = editOp{e.OldString, e.NewString, e.ReplaceAll}
	}
	res, err := runEdits(t.Tracker, in.FilePath, ops, true)
	if err != nil {
		return MultiEditResult{}, err
	}
	applied := make([]EditOperation, len(res.applied))
	for i, a := range res.applied {
		applied[i] = EditOperation{a.OldString, a.NewString, a.ReplaceAll}
	}
	return MultiEditResult{
		FilePath:     in.FilePath,
		Edits:        applied,
		OriginalFile: res.original,
		UpdatedFile:  res.updated,
		Created:      res.created,
		Text:         fmt.Sprintf("Applied %d edits to %s.%s", len(applied), in.FilePath, msgStateCurrent),
	}, nil
}
