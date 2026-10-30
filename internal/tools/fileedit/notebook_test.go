package fileedit

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadNotebookImageOutputsAndCap(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfakebody"))
	big := strings.Repeat("x", maxNotebookOutputChars+1)
	nbJSON := map[string]any{
		"cells": []any{
			map[string]any{"cell_type": "code", "source": "plot()", "outputs": []any{
				map[string]any{"output_type": "display_data", "data": map[string]any{
					"text/plain": "<Figure>", "image/png": png[:8] + "\n" + png[8:]}},
			}},
			map[string]any{"cell_type": "code", "source": "spam()", "outputs": []any{
				map[string]any{"output_type": "stream", "text": big},
			}},
			map[string]any{"cell_type": "code", "source": "bad()", "outputs": []any{
				map[string]any{"output_type": "display_data", "data": map[string]any{
					"image/png": base64.StdEncoding.EncodeToString([]byte("not an image"))}},
			}},
		},
		"metadata": map[string]any{},
	}
	raw, _ := json.Marshal(nbJSON)
	p := filepath.Join(t.TempDir(), "n.ipynb")
	writeFile(t, p, string(raw))
	res, err := ReadTool{Tracker: NewFileStateTracker()}.Execute(ctx, ReadInput{FilePath: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || res.Images[0].Base64 != png || res.Images[0].MediaType != "image/png" {
		t.Fatalf("images: %+v", res.Images)
	}
	if !strings.Contains(res.Text, "<Figure>") {
		t.Fatalf("text output missing: %q", res.Text)
	}
	want := "Outputs are too large to include. Use Bash with: cat <notebook_path> | jq '.cells[1].outputs'"
	if runtime.GOOS == "windows" {
		want = "Outputs are too large to include. Use PowerShell"
	}
	if !strings.Contains(res.Text, want) || strings.Contains(res.Text, big) {
		t.Fatalf("cap not applied: %q", res.Text[:200])
	}
}
