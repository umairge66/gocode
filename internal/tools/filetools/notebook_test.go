package filetools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sampleNotebook() string {
	return `{
 "cells": [
  {"cell_type": "code", "id": "aaa", "source": "print(1)", "metadata": {}, "execution_count": 5, "outputs": [{"x": 1}]},
  {"cell_type": "markdown", "id": "bbb", "source": ["# Title"], "metadata": {}}
 ],
 "metadata": {"language_info": {"name": "python"}},
 "nbformat": 4,
 "nbformat_minor": 5
}`
}

func writeNb(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "nb.ipynb")
	writeFile(t, p, sampleNotebook())
	return p
}

func readNb(t *testing.T, path string) *notebook {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := parseNotebook(data)
	if err != nil {
		t.Fatal(err)
	}
	return nb
}

func TestNotebookReplace(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	res, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		CellID:       "aaa",
		NewSource:    "print(42)",
		EditMode:     "replace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OldSource != "print(1)" {
		t.Fatalf("expected old source captured, got %q", res.OldSource)
	}
	nb := readNb(t, p)
	if sourceToString(nb.Cells[0].Source) != "print(42)" {
		t.Fatalf("source not replaced: %v", nb.Cells[0].Source)
	}
	// Code cell outputs and execution_count must be reset.
	if nb.Cells[0].ExecutionCount != nil {
		t.Fatalf("execution_count should be reset to null, got %v", *nb.Cells[0].ExecutionCount)
	}
	if len(nb.Cells[0].Outputs) != 0 {
		t.Fatalf("outputs should be cleared, got %v", nb.Cells[0].Outputs)
	}
	if res.Language != "python" {
		t.Fatalf("expected language python, got %q", res.Language)
	}
}

func TestNotebookReplaceChangesCellType(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	_, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		CellID:       "aaa",
		NewSource:    "# now markdown",
		CellType:     "markdown",
		EditMode:     "replace",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := readNb(t, p)
	if nb.Cells[0].CellType != "markdown" {
		t.Fatalf("cell type should change to markdown, got %q", nb.Cells[0].CellType)
	}
}

func TestNotebookInsertAfter(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	res, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		CellID:       "aaa",
		NewSource:    "print('new')",
		CellType:     "code",
		EditMode:     "insert",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := readNb(t, p)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells after insert, got %d", len(nb.Cells))
	}
	// Inserted after aaa => index 1.
	if sourceToString(nb.Cells[1].Source) != "print('new')" {
		t.Fatalf("inserted cell in wrong place: %v", nb.Cells[1].Source)
	}
	// New cell gets an 8-char id (nbformat 4.5).
	if len(res.CellID) != 8 {
		t.Fatalf("expected 8-char generated id, got %q", res.CellID)
	}
}

func TestNotebookInsertAtBeginning(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	_, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		NewSource:    "# header",
		CellType:     "markdown",
		EditMode:     "insert",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := readNb(t, p)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(nb.Cells))
	}
	if nb.Cells[0].CellType != "markdown" || sourceToString(nb.Cells[0].Source) != "# header" {
		t.Fatalf("expected new markdown cell at start, got %+v", nb.Cells[0])
	}
}

func TestNotebookDelete(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	_, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		CellID:       "bbb",
		NewSource:    "",
		EditMode:     "delete",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := readNb(t, p)
	if len(nb.Cells) != 1 {
		t.Fatalf("expected 1 cell after delete, got %d", len(nb.Cells))
	}
	if nb.Cells[0].ID != "aaa" {
		t.Fatalf("wrong cell deleted, remaining %q", nb.Cells[0].ID)
	}
}

func TestNotebookMarkdownInsertHasNoOutputs(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	_, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		NewSource:    "text",
		CellType:     "markdown",
		EditMode:     "insert",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	var raw struct {
		Cells []map[string]json.RawMessage `json:"cells"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	first := raw.Cells[0]
	if _, ok := first["outputs"]; ok {
		t.Fatal("markdown cell must not have outputs field")
	}
	if _, ok := first["execution_count"]; ok {
		t.Fatal("markdown cell must not have execution_count field")
	}
}

func TestNotebookValidationErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)

	// Non-.ipynb path.
	bad := filepath.Join(dir, "x.txt")
	writeFile(t, bad, "{}")
	if _, err := (NotebookEditTool{}).Execute(NotebookEditInput{NotebookPath: bad, NewSource: "x", CellID: "a"}); err != ErrNotNotebook {
		t.Fatalf("expected ErrNotNotebook, got %v", err)
	}

	// Insert without cell_type.
	if _, err := (NotebookEditTool{}).Execute(NotebookEditInput{NotebookPath: p, NewSource: "x", EditMode: "insert"}); err != ErrCellTypeRequired {
		t.Fatalf("expected ErrCellTypeRequired, got %v", err)
	}

	// Replace without cell_id.
	if _, err := (NotebookEditTool{}).Execute(NotebookEditInput{NotebookPath: p, NewSource: "x", EditMode: "replace"}); err != ErrCellIDRequired {
		t.Fatalf("expected ErrCellIDRequired, got %v", err)
	}

	// Unknown cell_id.
	if _, err := (NotebookEditTool{}).Execute(NotebookEditInput{NotebookPath: p, NewSource: "x", CellID: "zzz", EditMode: "replace"}); err != ErrCellNotFound {
		t.Fatalf("expected ErrCellNotFound, got %v", err)
	}
}

func TestNotebookReplacePastEndAppends(t *testing.T) {
	dir := t.TempDir()
	p := writeNb(t, dir)
	// cell_id "cell-2" resolves to numeric index 2 == len(cells), which is
	// converted from replace to an append (insert).
	_, err := NotebookEditTool{}.Execute(NotebookEditInput{
		NotebookPath: p,
		CellID:       "cell-2",
		NewSource:    "appended",
		EditMode:     "replace",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := readNb(t, p)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells after append, got %d", len(nb.Cells))
	}
	if sourceToString(nb.Cells[2].Source) != "appended" {
		t.Fatalf("expected appended cell at end, got %v", nb.Cells[2].Source)
	}
}
