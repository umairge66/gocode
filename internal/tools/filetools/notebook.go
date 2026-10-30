package filetools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Edit-mode and cell-type enum values from the input schema.
const (
	EditModeReplace = "replace"
	EditModeInsert  = "insert"
	EditModeDelete  = "delete"

	CellTypeCode     = "code"
	CellTypeMarkdown = "markdown"
)

// NotebookEditInput is the input schema for the NotebookEdit tool.
type NotebookEditInput struct {
	// NotebookPath: "The absolute path to the Jupyter notebook file to edit
	// (must be absolute, not relative)".
	NotebookPath string `json:"notebook_path"`
	// CellID: "The ID of the cell to edit. When inserting a new cell, the new
	// cell will be inserted after the cell with this ID, or at the beginning if
	// not specified." Required for replace and delete.
	CellID string `json:"cell_id,omitempty"`
	// NewSource: "The new source for the cell".
	NewSource string `json:"new_source"`
	// CellType is "code" or "markdown". Required when edit_mode=insert.
	CellType string `json:"cell_type,omitempty"`
	// EditMode is "replace" (default), "insert" or "delete".
	EditMode string `json:"edit_mode,omitempty"`
}

// NotebookEditResult is a subset of the tool's output schema. The fields
// omitted here (original_file/updated_file) are rendering aids and not needed
// to express the transform.
type NotebookEditResult struct {
	NewSource string `json:"new_source"`
	OldSource string `json:"old_source,omitempty"`
	CellID    string `json:"cell_id,omitempty"`
	CellType  string `json:"cell_type"`
	Language  string `json:"language"`
	EditMode  string `json:"edit_mode"`
	Error     string `json:"error,omitempty"`
}

// notebookCell models a single .ipynb cell. It preserves unknown fields so
// edits round-trip losslessly, and emits the execution_count/outputs fields
// that the .ipynb schema requires for code cells (and omits them for markdown).
type notebookCell struct {
	CellType       string
	ID             string
	Source         interface{}
	Metadata       map[string]interface{}
	ExecutionCount *int
	hasExecCount   bool
	Outputs        []interface{}
	hasOutputs     bool
	// extra preserves any unknown fields so round-tripping is lossless.
	extra map[string]json.RawMessage
}

// UnmarshalJSON records which optional fields were present so they can be
// reproduced on write.
func (c *notebookCell) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.extra = map[string]json.RawMessage{}
	for k, v := range raw {
		switch k {
		case "cell_type":
			_ = json.Unmarshal(v, &c.CellType)
		case "id":
			_ = json.Unmarshal(v, &c.ID)
		case "source":
			_ = json.Unmarshal(v, &c.Source)
		case "metadata":
			_ = json.Unmarshal(v, &c.Metadata)
		case "execution_count":
			c.hasExecCount = true
			_ = json.Unmarshal(v, &c.ExecutionCount)
		case "outputs":
			c.hasOutputs = true
			_ = json.Unmarshal(v, &c.Outputs)
		default:
			c.extra[k] = v
		}
	}
	return nil
}

// MarshalJSON emits the canonical field set for the cell.
func (c notebookCell) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{
		"cell_type": c.CellType,
		"source":    c.Source,
	}
	if c.Metadata != nil {
		out["metadata"] = c.Metadata
	} else {
		out["metadata"] = map[string]interface{}{}
	}
	if c.ID != "" {
		out["id"] = c.ID
	}
	if c.CellType == CellTypeCode || c.hasExecCount {
		out["execution_count"] = c.ExecutionCount // nil -> null
	}
	if c.CellType == CellTypeCode || c.hasOutputs {
		if c.Outputs == nil {
			out["outputs"] = []interface{}{}
		} else {
			out["outputs"] = c.Outputs
		}
	}
	for k, v := range c.extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// notebook models the top-level .ipynb document.
type notebook struct {
	Cells         []notebookCell         `json:"cells"`
	Metadata      map[string]interface{} `json:"metadata"`
	NBFormat      int                    `json:"nbformat"`
	NBFormatMinor int                    `json:"nbformat_minor"`
	extra         map[string]json.RawMessage
}

// NotebookEditTool implements the NotebookEdit tool: it replaces, inserts or
// deletes a single cell in a Jupyter notebook (.ipynb) file.
type NotebookEditTool struct{}

// Validation error codes reported by input validation.
var (
	// ErrNotNotebook: non-.ipynb path (errorCode 2).
	ErrNotNotebook = errors.New("file must be a Jupyter notebook (.ipynb file); for editing other file types, use the FileEdit tool")
	// ErrCellTypeRequired: insert without cell_type (errorCode 5).
	ErrCellTypeRequired = errors.New("cell type is required when using edit_mode=insert")
	// ErrNotJSON: notebook body not valid JSON (errorCode 6).
	ErrNotJSON = errors.New("notebook is not valid JSON")
	// ErrCellIDRequired: replace/delete without cell_id (errorCode 7).
	ErrCellIDRequired = errors.New("cell ID must be specified when not inserting a new cell")
	// ErrCellNotFound: cell_id absent from notebook (errorCode 8).
	ErrCellNotFound = errors.New("cell with the given ID not found in notebook")
)

// Execute applies the cell edit to the notebook on disk and returns the result
// record. Permission / read-state machinery (read-file state, write
// permissions, file-history ops) is out of scope here; this implements the pure
// .ipynb transform.
func (NotebookEditTool) Execute(in NotebookEditInput) (NotebookEditResult, error) {
	mode := in.EditMode
	if mode == "" {
		mode = EditModeReplace
	}

	// --- validate input ---------------------------------------------------
	if strings.ToLower(filepath.Ext(in.NotebookPath)) != ".ipynb" {
		return NotebookEditResult{}, ErrNotNotebook
	}
	if mode == EditModeInsert && in.CellType == "" {
		return NotebookEditResult{}, ErrCellTypeRequired
	}
	if in.CellID == "" && mode != EditModeInsert {
		return NotebookEditResult{}, ErrCellIDRequired
	}

	data, err := os.ReadFile(in.NotebookPath)
	if err != nil {
		if os.IsNotExist(err) {
			return NotebookEditResult{}, errors.New("notebook file does not exist")
		}
		return NotebookEditResult{}, err
	}

	nb, err := parseNotebook(data)
	if err != nil {
		return NotebookEditResult{}, ErrNotJSON
	}

	// --- locate target cell -----------------------------------------------
	idx := 0
	if in.CellID == "" {
		idx = 0
	} else {
		idx = -1
		for i, c := range nb.Cells {
			if c.ID == in.CellID {
				idx = i
				break
			}
		}
		if idx == -1 {
			// Fall back to interpreting cell_id as a numeric index. The
			// append position (n == len) is permitted so that a replace past
			// the final cell converts to an append below.
			if n, ok := parseCellIndex(in.CellID); ok && n >= 0 && n <= len(nb.Cells) {
				idx = n
			} else {
				return NotebookEditResult{}, ErrCellNotFound
			}
		}
		if mode == EditModeInsert {
			// insert *after* the referenced cell.
			idx++
		}
	}

	// A replace that lands past the final cell becomes an append (insert).
	if mode == EditModeReplace && idx == len(nb.Cells) {
		mode = EditModeInsert
		if in.CellType == "" {
			in.CellType = CellTypeCode
		}
	}

	language := "python"
	if nb.Metadata != nil {
		if li, ok := nb.Metadata["language_info"].(map[string]interface{}); ok {
			if name, ok := li["name"].(string); ok && name != "" {
				language = name
			}
		}
	}

	// New-cell IDs are only assigned for nbformat >= 4.5.
	supportsIDs := nb.NBFormat > 4 || (nb.NBFormat == 4 && nb.NBFormatMinor >= 5)

	result := NotebookEditResult{
		NewSource: in.NewSource,
		CellType:  in.CellType,
		Language:  language,
		EditMode:  mode,
		CellID:    in.CellID,
	}

	switch mode {
	case EditModeDelete:
		if idx < 0 || idx >= len(nb.Cells) {
			return NotebookEditResult{}, ErrCellNotFound
		}
		target := nb.Cells[idx]
		result.OldSource = sourceToString(target.Source)
		result.CellType = target.CellType
		nb.Cells = append(nb.Cells[:idx], nb.Cells[idx+1:]...)

	case EditModeInsert:
		var newID string
		if supportsIDs {
			newID = newCellID()
		}
		result.CellID = newID
		cell := notebookCell{
			ID:       newID,
			Source:   in.NewSource,
			Metadata: map[string]interface{}{},
		}
		if in.CellType == CellTypeMarkdown {
			cell.CellType = CellTypeMarkdown
		} else {
			cell.CellType = CellTypeCode
			// execution_count: null, outputs: [] (emitted by MarshalJSON).
			cell.ExecutionCount = nil
			cell.hasExecCount = true
			cell.Outputs = []interface{}{}
			cell.hasOutputs = true
		}
		if idx < 0 {
			idx = 0
		}
		if idx > len(nb.Cells) {
			idx = len(nb.Cells)
		}
		nb.Cells = append(nb.Cells[:idx], append([]notebookCell{cell}, nb.Cells[idx:]...)...)

	default: // replace
		if idx < 0 || idx >= len(nb.Cells) {
			return NotebookEditResult{}, ErrCellNotFound
		}
		target := &nb.Cells[idx]
		result.OldSource = sourceToString(target.Source)
		target.Source = in.NewSource
		if target.CellType == CellTypeCode {
			target.ExecutionCount = nil
			target.Outputs = []interface{}{}
		}
		if in.CellType != "" && in.CellType != target.CellType {
			target.CellType = in.CellType
		}
		result.CellID = target.ID
		result.CellType = target.CellType
	}

	// Serialize with one-space indentation (the standard .ipynb layout).
	out, err := marshalNotebook(nb)
	if err != nil {
		return NotebookEditResult{}, err
	}
	if err := os.WriteFile(in.NotebookPath, out, 0o644); err != nil {
		return NotebookEditResult{}, err
	}
	return result, nil
}

// sourceToString joins an .ipynb "source" value, which is either a string or an
// array of strings.
func sourceToString(src interface{}) string {
	switch v := src.(type) {
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, s := range v {
			if str, ok := s.(string); ok {
				b.WriteString(str)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// newCellID returns an 8-character hex id.
func newCellID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// parseCellIndex interprets a cell_id of the form "cell-<n>" or a bare integer
// as a numeric index (the fallback when no cell has that ID).
func parseCellIndex(id string) (int, bool) {
	s := strings.TrimPrefix(id, "cell-")
	n := 0
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// parseNotebook decodes an .ipynb document, preserving unknown top-level and
// per-cell fields so writes are lossless.
func parseNotebook(data []byte) (*notebook, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if _, ok := raw["cells"]; !ok {
		return nil, errors.New("not a notebook: missing cells")
	}
	nb := &notebook{extra: map[string]json.RawMessage{}}
	for k, v := range raw {
		switch k {
		case "cells":
			if err := json.Unmarshal(v, &nb.Cells); err != nil {
				return nil, err
			}
		case "metadata":
			_ = json.Unmarshal(v, &nb.Metadata)
		case "nbformat":
			_ = json.Unmarshal(v, &nb.NBFormat)
		case "nbformat_minor":
			_ = json.Unmarshal(v, &nb.NBFormatMinor)
		default:
			nb.extra[k] = v
		}
	}
	return nb, nil
}

// marshalNotebook serializes the notebook with one-space indentation.
func marshalNotebook(nb *notebook) ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range nb.extra {
		out[k] = v
	}
	cells, err := json.Marshal(nb.Cells)
	if err != nil {
		return nil, err
	}
	out["cells"] = cells
	if nb.Metadata != nil {
		md, err := json.Marshal(nb.Metadata)
		if err != nil {
			return nil, err
		}
		out["metadata"] = md
	}
	out["nbformat"] = json.RawMessage(fmt.Sprintf("%d", nb.NBFormat))
	out["nbformat_minor"] = json.RawMessage(fmt.Sprintf("%d", nb.NBFormatMinor))
	return json.MarshalIndent(out, "", " ")
}
