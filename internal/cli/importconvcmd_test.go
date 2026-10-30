package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/sessionstore"
)

const importExportJSON = `{
 "conversations": [{
  "uuid": "conv-1", "name": "", "created_at": "2025-01-02T03:04:05Z", "updated_at": "2025-01-02T04:00:00Z",
  "project_uuid": "proj-1",
  "chat_messages": [
   {"uuid": "m1", "parent_message_uuid": null, "sender": "human", "text": "make a page", "created_at": "2025-01-02T03:04:05Z",
    "content": [{"type": "text", "text": "make a page"}],
    "attachments": [{"file_name": "notes.txt", "extracted_content": "remember"}], "files": []},
   {"uuid": "m2", "parent_message_uuid": "m1", "sender": "assistant", "text": "", "created_at": "2025-01-02T03:05:00Z",
    "content": [{"type": "text", "text": "Here it is."},
                {"type": "tool_use", "name": "artifacts", "input": {"id": "a1", "command": "create", "title": "Home Page", "type": "text/html", "content": "<p>v1</p>"}},
                {"type": "tool_use", "name": "artifacts", "input": {"id": "a1", "command": "update", "old_str": "v1", "new_str": "v2"}}],
    "attachments": [], "files": []}
  ]}],
 "projects": [{"uuid": "proj-1", "name": "My Proj", "prompt_template": "Be brief.", "docs": [{"filename": "gocode.md", "content": "doc"}], "files": []}]
}`

func TestImportConversationsJSON(t *testing.T) {
	f := newSubcmdFixture(t)
	export := filepath.Join(f.cwd, "export.json")
	subcmdWriteFile(t, export, importExportJSON)
	archive := filepath.Join(f.home, "archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := f.run(t, "import-conversations", export, "--cwd", archive)
	if code != 1 || !strings.Contains(errOut, "import-conversations is not enabled") {
		t.Fatalf("gate: exit %d err %q", code, errOut)
	}
	f.vars["GOCODE_IMPORT_CONVERSATIONS"] = "1"
	code, _, errOut = f.run(t, "import-conversations", export)
	if code != 1 || !strings.Contains(errOut, "--cwd is required") {
		t.Fatalf("cwd: exit %d err %q", code, errOut)
	}

	// Dry run writes nothing.
	code, out, errOut := f.run(t, "import-conversations", export, "--cwd", archive, "--dry-run")
	if code != 0 || !strings.HasPrefix(out, "[dry-run] imported: conversations=1 skipped=0 messages=2 projects=1 docs=1 files=0 artifacts=1") {
		t.Fatalf("dry: exit %d out %q err %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(archive, "projects")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote files")
	}

	code, out, errOut = f.run(t, "import-conversations", export, "--cwd", archive)
	if code != 0 {
		t.Fatalf("import: exit %d out %q err %q", code, out, errOut)
	}
	lines := strings.SplitN(out, "\n", 2)
	var res struct {
		SessionIDs      []string            `json:"sessionIds"`
		Titles          []string            `json:"titles"`
		JSONLPaths      []string            `json:"jsonlPaths"`
		AttachmentFiles map[string][]string `json:"attachmentFiles"`
		Counts          map[string]int      `json:"counts"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &res); err != nil {
		t.Fatalf("summary JSON: %v (%q)", err, lines[1])
	}
	sid := ids.UUIDv5("conv-1", importConvNamespace)
	if len(res.SessionIDs) != 1 || res.SessionIDs[0] != sid || res.Titles[0] != "make a page" || res.Counts["artifacts"] != 1 {
		t.Fatalf("summary = %+v", res)
	}
	wantJSONL := filepath.Join(f.home, ".config", "gocode", "projects", sessionstore.EscapeCwd(archive), sid+".jsonl")
	if res.JSONLPaths[0] != wantJSONL {
		t.Fatalf("jsonl = %s want %s", res.JSONLPaths[0], wantJSONL)
	}
	transcript := subcmdReadFile(t, wantJSONL)
	for _, want := range []string{`"type":"user"`, "remember", `<file name=\"notes.txt\">`, "**Artifact: Home Page** — updated; final version below.", "**Artifact: Home Page** (v2)", "<p>v2</p>", "_Saved to files/Home_Page.html_"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}
	if got := subcmdReadFile(t, filepath.Join(archive, "files", "Home_Page.html")); got != "<p>v2</p>" {
		t.Errorf("saved artifact = %q", got)
	}
	projDir := filepath.Join(archive, "projects", "my-proj-proj-1")
	if subcmdReadFile(t, filepath.Join(projDir, "project-instructions.md")) != "Be brief." || subcmdReadFile(t, filepath.Join(projDir, "imported-gocode.md")) != "doc" {
		t.Error("project files not written (or reserved name not prefixed)")
	}
	// The session transcript reader loads the imported session.
	msgs, err := sessionstore.New(filepath.Join(f.home, ".config", "gocode")).Load(archive, sid)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("Load = %d msgs, %v", len(msgs), err)
	}

	// A second import skips the existing transcript.
	code, out, _ = f.run(t, "import-conversations", export, "--cwd", archive)
	if code != 0 || !strings.Contains(out, "conversations=0 skipped=1") {
		t.Fatalf("re-import: exit %d out %q", code, out)
	}
}

func TestImportConversationsZipAndManifest(t *testing.T) {
	f := newSubcmdFixture(t)
	f.vars["GOCODE_IMPORT_CONVERSATIONS"] = "true"
	archive := filepath.Join(f.home, "archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("conversations.json", `[{"uuid":"c1","name":"Chat","chat_messages":[{"uuid":"x","sender":"human","text":"hi","files":[{"file_uuid":"f1","file_name":"pic.png"}]}]}]`)
	add("manifest.json", `{"account_uuid":"acct","conversations":[{"uuid":"c1","message_count":5}],"projects":[]}`)
	add("files/f1", "PNGDATA")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(f.cwd, "export.zip")
	if err := os.WriteFile(export, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.run(t, "import-conversations", export, "--cwd", archive)
	if code != 1 || !strings.Contains(errOut, "messages: manifest=5 imported=1") || !strings.Contains(errOut, "IMPORT_MANIFEST_MISMATCH") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if got := subcmdReadFile(t, filepath.Join(archive, "files", "f1-pic.png")); got != "PNGDATA" {
		t.Fatalf("attached file = %q", got)
	}
	if !strings.Contains(out, `"attachmentFiles":{"`) || !strings.Contains(out, "files/f1-pic.png") {
		t.Fatalf("attachmentFiles missing: %q", out)
	}

	subcmdWriteFile(t, filepath.Join(f.cwd, "bad.json"), "{nope")
	code, _, errOut = f.run(t, "import-conversations", filepath.Join(f.cwd, "bad.json"), "--cwd", archive)
	if code != 1 || !strings.Contains(errOut, "export file is not valid JSON") || !strings.Contains(errOut, "(IMPORT_ERROR)") {
		t.Fatalf("bad json exit %d err %q", code, errOut)
	}
}
