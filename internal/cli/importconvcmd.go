package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/ids"
	"github.com/umairge66/gocode/internal/sessionstore"
)

// `gocode import-conversations <exportPath> --cwd <dir> [--dry-run]`
// (hidden, gated by GOCODE_IMPORT_CONVERSATIONS): import a chat-export archive
// (a conversations JSON file, or the export .zip with conversations.json,
// projects.json or projects/*.json, manifest.json, users.json and files/) as
// local, resumable session transcripts anchored to --cwd. Project documents
// and instructions go under <cwd>/projects/<slug>-<uuid>/, attached files and
// final artifact versions under <cwd>/files/. Everything is read from the
// local file and written locally; nothing is uploaded.
//
// Simplifications: artifact versions are followed in message order rather
// than along each branch of the message tree; images in a message are
// referenced as @"files/…" rather than inlined; no synthetic Write tool_use /
// tool_result pairs are written for saved artifacts and there is no @-mention
// attachment enrichment; the transcript lines use gocode's transcript schema
// (internal/sessionstore) so `--resume` reads them.

const (
	importConvNamespace = "5f3a2c5e-6b8f-4b27-9c0e-2d7f1a9b3c44"
	importMaxExportSize = 1 << 30
	importMaxZipEntries = 100000
	importConvByteCap   = 64 << 20 // per conversation
	importTotalByteCap  = 256 << 20
	importVersionTag    = "gocode-export-import"
	importNoContent     = "(no content)"
	importCapNote       = "_Artifact content from this point on was not imported (size limit)._"
)

var (
	importSafeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	importLtRe       = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	importTrailDotRe = regexp.MustCompile(`[.\s]+$`)
	importSlugRe     = regexp.MustCompile(`[^a-z0-9]+`)
	importAlnumRe    = regexp.MustCompile(`[a-zA-Z0-9]`)
)

// --- export schema (lenient: bad fields fall back to defaults) -------------

type exportFileRef struct {
	FileUUID string `json:"file_uuid"`
	FileName string `json:"file_name"`
}

type exportToolInput struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	Content  string `json:"content"`
	OldStr   string `json:"old_str"`
	NewStr   string `json:"new_str"`
	Title    string `json:"title"`
	Type     string `json:"type"`
	Language string `json:"language"`
	Path     string `json:"path"`
	FileText string `json:"file_text"`
	has      map[string]bool
}

type exportBlock struct {
	Type  string
	Text  *string
	Name  string
	Input *exportToolInput
}

type exportAttachment struct {
	FileName         string
	ExtractedContent string
}

type exportMessage struct {
	UUID        string
	Parent      *string
	Sender      string
	Text        string
	Content     []exportBlock
	CreatedAt   string
	Attachments []exportAttachment
	Files       []exportFileRef
}

type exportConversation struct {
	UUID        string
	Name        string
	CreatedAt   string
	UpdatedAt   string
	ProjectUUID *string
	Messages    []exportMessage
}

type exportDoc struct {
	Filename string
	Content  string
}

type exportProject struct {
	UUID           string
	Name           string
	PromptTemplate string
	Docs           []exportDoc
	Files          []exportFileRef
}

type importManifest struct {
	AccountUUID   string `json:"account_uuid"`
	Conversations []struct {
		UUID         string `json:"uuid"`
		MessageCount int    `json:"message_count"`
	} `json:"conversations"`
	Projects []struct {
		UUID     string `json:"uuid"`
		DocCount int    `json:"doc_count"`
	} `json:"projects"`
}

func jStr(m map[string]any, k string) (string, bool) {
	s, ok := m[k].(string)
	return s, ok
}

func jOptStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func jList(m map[string]any, k string) []map[string]any {
	arr, _ := m[k].([]any)
	var out []map[string]any
	for _, x := range arr {
		if o, ok := x.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func parseFileRefs(m map[string]any) []exportFileRef {
	var out []exportFileRef
	for _, f := range jList(m, "files") {
		id, ok := jStr(f, "file_uuid")
		if !ok {
			continue
		}
		out = append(out, exportFileRef{FileUUID: id, FileName: jOptStr(f, "file_name")})
	}
	return out
}

func parseExportMessage(m map[string]any) (exportMessage, bool) {
	id, ok := jStr(m, "uuid")
	if !ok {
		return exportMessage{}, false
	}
	msg := exportMessage{UUID: id, Sender: "assistant", Text: jOptStr(m, "text"), CreatedAt: jOptStr(m, "created_at"), Files: parseFileRefs(m)}
	if s, ok := jStr(m, "sender"); ok {
		msg.Sender = s
	}
	if p, ok := jStr(m, "parent_message_uuid"); ok {
		msg.Parent = &p
	}
	for _, b := range jList(m, "content") {
		typ, ok := jStr(b, "type")
		if !ok {
			continue
		}
		blk := exportBlock{Type: typ, Name: jOptStr(b, "name")}
		if t, ok := jStr(b, "text"); ok {
			blk.Text = &t
		}
		if in, ok := b["input"].(map[string]any); ok {
			ti := &exportToolInput{has: map[string]bool{}}
			for k, dst := range map[string]*string{"id": &ti.ID, "command": &ti.Command, "content": &ti.Content, "old_str": &ti.OldStr, "new_str": &ti.NewStr, "title": &ti.Title, "type": &ti.Type, "language": &ti.Language, "path": &ti.Path, "file_text": &ti.FileText} {
				if s, ok := jStr(in, k); ok {
					*dst = s
					ti.has[k] = true
				}
			}
			blk.Input = ti
		}
		msg.Content = append(msg.Content, blk)
	}
	for _, a := range jList(m, "attachments") {
		ec, ok := jStr(a, "extracted_content")
		if !ok {
			continue
		}
		msg.Attachments = append(msg.Attachments, exportAttachment{FileName: jOptStr(a, "file_name"), ExtractedContent: ec})
	}
	return msg, true
}

func parseExportConversations(v any) []exportConversation {
	arr, _ := v.([]any)
	var out []exportConversation
	for _, x := range arr {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		id, ok := jStr(m, "uuid")
		if !ok {
			continue
		}
		c := exportConversation{UUID: id, Name: jOptStr(m, "name"), CreatedAt: jOptStr(m, "created_at"), UpdatedAt: jOptStr(m, "updated_at")}
		if p, ok := jStr(m, "project_uuid"); ok {
			c.ProjectUUID = &p
		}
		for _, cm := range jList(m, "chat_messages") {
			if msg, ok := parseExportMessage(cm); ok {
				c.Messages = append(c.Messages, msg)
			}
		}
		out = append(out, c)
	}
	return out
}

func parseExportProjects(v any) []exportProject {
	var arr []any
	switch x := v.(type) {
	case []any:
		arr = x
	case map[string]any:
		arr = []any{x}
	}
	var out []exportProject
	for _, x := range arr {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		id, ok := jStr(m, "uuid")
		if !ok {
			continue
		}
		p := exportProject{UUID: id, Name: jOptStr(m, "name"), PromptTemplate: jOptStr(m, "prompt_template"), Files: parseFileRefs(m)}
		for _, d := range jList(m, "docs") {
			c, ok := jStr(d, "content")
			if !ok {
				continue
			}
			p.Docs = append(p.Docs, exportDoc{Filename: jOptStr(d, "filename"), Content: c})
		}
		out = append(out, p)
	}
	return out
}

// importExport is the parsed archive.
type importExport struct {
	manifest      importManifest
	conversations []exportConversation
	projects      []exportProject
	files         map[string][]byte
}

// projectDirName returns "<slug(name)|project>-<uuid>", or "" when
// that is not a safe directory name.
func projectDirName(p exportProject) string {
	slug := strings.Trim(importSlugRe.ReplaceAllString(strings.ToLower(p.Name), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "project"
	}
	n := slug + "-" + p.UUID
	if importSafeNameRe.MatchString(n) && len(n) <= 200 {
		return n
	}
	return ""
}

// readImportExport reads the export archive or JSON file.
func readImportExport(p string) (*importExport, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.Size() > importMaxExportSize {
		return nil, errors.New("export file exceeds MAX_EXPORT_BYTES")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(p, ".zip") {
		var doc any
		if json.Unmarshal(data, &doc) != nil {
			return nil, errors.New("export file is not valid JSON")
		}
		ex := &importExport{files: map[string][]byte{}}
		if m, ok := doc.(map[string]any); ok {
			ex.conversations = parseExportConversations(m["conversations"])
			ex.projects = parseExportProjects(m["projects"])
			ex.manifest = buildManifest(m["manifest"], ex, "")
		} else {
			ex.manifest = buildManifest(nil, ex, "")
		}
		return ex, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if len(zr.File) > importMaxZipEntries {
		return nil, errors.New("Archive contains too many files")
	}
	entries := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, importMaxExportSize-total+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(b))
		if total > importMaxExportSize {
			return nil, errors.New("Archive total size is too large")
		}
		entries[f.Name] = b
	}
	parse := func(name string) any {
		b, ok := entries[name]
		if !ok {
			return nil
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil
		}
		return v
	}
	ex := &importExport{files: map[string][]byte{}}
	if b, ok := entries["conversations.json"]; ok {
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil, errors.New("export conversations.json is not valid JSON")
		}
		ex.conversations = parseExportConversations(v)
	}
	if v := parse("projects.json"); v != nil {
		ex.projects = parseExportProjects(v)
	} else {
		var names []string
		for name := range entries {
			if strings.HasPrefix(name, "projects/") && strings.HasSuffix(name, ".json") {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			ex.projects = append(ex.projects, parseExportProjects(parse(name))...)
		}
	}
	account := ""
	if users, ok := parse("users.json").([]any); ok && len(users) > 0 {
		if u, ok := users[0].(map[string]any); ok {
			account = jOptStr(u, "uuid")
		}
	}
	ex.manifest = buildManifest(parse("manifest.json"), ex, account)
	for name, b := range entries {
		if !strings.HasPrefix(name, "files/") || strings.HasSuffix(name, "/") {
			continue
		}
		base := path.Base(name)
		if base == "" || !importSafeNameRe.MatchString(base) {
			continue
		}
		ex.files[base] = b
	}
	return ex, nil
}

// buildManifest returns the archive's manifest when it is valid, else
// one synthesized from what was parsed (so nothing mismatches).
func buildManifest(v any, ex *importExport, account string) importManifest {
	var m importManifest
	if v != nil {
		if b, err := json.Marshal(v); err == nil {
			var probe struct {
				Conversations []any `json:"conversations"`
				Projects      []any `json:"projects"`
			}
			if json.Unmarshal(b, &probe) == nil && probe.Conversations != nil && probe.Projects != nil && json.Unmarshal(b, &m) == nil {
				if m.AccountUUID == "" {
					m.AccountUUID = "unknown"
				}
				return m
			}
		}
	}
	m = importManifest{AccountUUID: "unknown"}
	if account != "" {
		m.AccountUUID = account
	}
	for _, c := range ex.conversations {
		m.Conversations = append(m.Conversations, struct {
			UUID         string `json:"uuid"`
			MessageCount int    `json:"message_count"`
		}{c.UUID, len(c.Messages)})
	}
	for _, p := range ex.projects {
		if projectDirName(p) == "" {
			continue
		}
		m.Projects = append(m.Projects, struct {
			UUID     string `json:"uuid"`
			DocCount int    `json:"doc_count"`
		}{p.UUID, len(p.Docs)})
	}
	return m
}

// --- names -----------------------------------------------------------------

func importLt(s string) string { return importLtRe.ReplaceAllString(s, "_") }

// importCt returns a safe file name of at most 128 characters.
func importCt(s string) string {
	n := importLt(s)
	if len(n) > 128 {
		n = n[:128]
	}
	if n == "" {
		return "file"
	}
	return n
}

// importReserved prefixes names that would be read as configuration
// in the anchored directory with "imported-".
func importReserved(name string) string {
	n := importTrailDotRe.ReplaceAllString(filepath.Base(name), "")
	switch {
	case n == "gocode.md", n == "gocode.local.md", n == ".git", n == "project-instructions.md",
		strings.HasPrefix(n, ".gocode"), n == ".windsurfrules", n == ".clinerules", n == ".gemini",
		n == ".cursor", n == ".worktreeinclude", n == ".mcp.json", n == "AGENTS.md":
		return "imported-" + name
	}
	return name
}

// importFileName returns the on-disk name for an attached file.
func importFileName(id, name string) string {
	if name != "" {
		return id + "-" + importCt(name)
	}
	return id
}

// writeNew creates the file (0600) only when it does not
// exist yet; false when it already did.
func writeNew(p string, data []byte) (bool, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return false, werr
	}
	return true, cerr
}

// --- artifacts ---------------------------------------------------------------

type importArtifact struct {
	kind, title, typ, language, content string
	version                             int
	final                               bool
}

var importLangExt = map[string]string{"python": ".py", "py": ".py", "javascript": ".js", "js": ".js", "typescript": ".ts", "ts": ".ts", "jsx": ".jsx", "tsx": ".tsx", "html": ".html", "css": ".css", "json": ".json", "yaml": ".yaml", "yml": ".yml", "toml": ".toml", "xml": ".xml", "markdown": ".md", "md": ".md", "bash": ".sh", "sh": ".sh", "shell": ".sh", "sql": ".sql", "java": ".java", "c": ".c", "cpp": ".cpp", "c++": ".cpp", "csharp": ".cs", "c#": ".cs", "go": ".go", "rust": ".rs", "ruby": ".rb", "php": ".php", "swift": ".swift", "kotlin": ".kt", "r": ".r", "svg": ".svg", "mermaid": ".mmd"}

func importTextExts() map[string]bool {
	m := map[string]bool{".txt": true, ".csv": true, ".tsv": true, ".log": true}
	for _, v := range importLangExt {
		m[v] = true
	}
	return m
}

// artifactExt returns the file extension for an artifact type / language.
func artifactExt(typ, lang string) string {
	switch typ {
	case "text/html":
		return ".html"
	case "application/vnd.ant.react":
		return ".jsx"
	case "text/markdown":
		return ".md"
	case "image/svg+xml":
		return ".svg"
	case "application/vnd.ant.mermaid":
		return ".mmd"
	case "application/vnd.ant.code":
		if e, ok := importLangExt[strings.ToLower(strings.TrimSpace(lang))]; ok {
			return e
		}
	}
	return ".txt"
}

// artifactFence returns the code-fence language ("" = no language,
// skip=true = no fence at all for markdown).
func artifactFence(typ, lang string) (string, bool) {
	switch typ {
	case "text/markdown":
		return "", true
	case "application/vnd.ant.code":
		l := strings.TrimSpace(lang)
		if len(l) > 64 {
			l = l[:64]
		}
		return l, false
	case "text/html":
		return "html", false
	case "application/vnd.ant.react":
		return "jsx", false
	case "image/svg+xml":
		return "svg", false
	case "application/vnd.ant.mermaid":
		return "mermaid", false
	case "text/plain":
		return "text", false
	}
	return "", false
}

func splitExt(name string) (string, string) {
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		return name[:i], name[i:]
	}
	return name, ""
}

// artifactFileName returns the file stem and extension for a saved artifact.
func artifactFileName(a importArtifact) (string, string) {
	title := importTrailDotRe.ReplaceAllString(a.title, "")
	var stem, ext string
	if a.kind == "file" {
		stem, ext = splitExt(title)
		ext = importLt(ext)
		if !importTextExts()[strings.ToLower(ext)] {
			stem += ext
			if ext != "" {
				ext = ".txt"
			}
		}
	} else {
		stem, ext = title, artifactExt(a.typ, a.language)
		if len(stem) > len(ext) && strings.HasSuffix(strings.ToLower(stem), ext) {
			stem = stem[:len(stem)-len(ext)]
		}
	}
	stem = importTrailDotRe.ReplaceAllString(strings.TrimLeft(importCt(stem), "."), "")
	if !importAlnumRe.MatchString(stem) {
		stem = a.kind
	}
	return stem, ext
}

func artifactHeader(a importArtifact) string {
	label := "Artifact"
	if a.kind == "file" {
		label = "File"
	}
	h := fmt.Sprintf("**%s: %s**", label, a.title)
	if a.version > 1 {
		h += fmt.Sprintf(" (v%d)", a.version)
	}
	return h
}

// renderArtifact renders an artifact version as its header plus (fenced) content.
func renderArtifact(a importArtifact) string {
	h := artifactHeader(a)
	var lang string
	noFence := false
	if a.kind == "file" {
		_, e := splitExt(a.title)
		lang = strings.ToLower(strings.TrimPrefix(e, "."))
	} else {
		lang, noFence = artifactFence(a.typ, a.language)
	}
	if noFence {
		return h + "\n\n" + a.content
	}
	body := strings.TrimSuffix(a.content, "\n")
	fence := "```"
	for strings.Contains(body, fence) {
		fence += "`"
	}
	return h + "\n\n" + fence + lang + "\n" + body + "\n" + fence
}

// keptArtifact is a final artifact version to save under files/.
type keptArtifact struct {
	msg, block int
	art        *importArtifact
}

// reconstructArtifacts follows the artifact and file tool calls in message
// order, renders each version, and returns the
// per-block texts, the final versions to save, and the failed str_replace
// count.
func reconstructArtifacts(msgs []exportMessage, budget *int64) (texts [][]*string, kept []keptArtifact, editsFailed int) {
	state := map[string]*importArtifact{}
	var all []*importArtifact
	where := map[*importArtifact][2]int{}
	texts = make([][]*string, len(msgs))
	var spent int64
	capped := false
	for mi, m := range msgs {
		texts[mi] = make([]*string, len(m.Content))
		if m.Sender == "human" {
			continue
		}
		for bi, b := range m.Content {
			in := b.Input
			if b.Type != "tool_use" || in == nil {
				continue
			}
			isArt := b.Name == "artifacts"
			isFile := b.Name == "create_file" || b.Name == "str_replace"
			var key string
			switch {
			case isArt && in.has["id"]:
				key = "artifact:" + in.ID
			case isFile && in.has["path"]:
				key = "file:" + in.Path
			default:
				continue
			}
			if capped {
				continue
			}
			prev := state[key]
			cost := int64(max(4096, len(in.Content), len(in.FileText)))
			if prev != nil {
				cost = max(cost, int64(len(prev.content)))
			}
			spent += cost
			*budget -= cost
			if spent > importConvByteCap || *budget < 0 {
				note := importCapNote
				texts[mi][bi] = &note
				capped = true
				continue
			}
			var content *string
			switch {
			case isArt:
				switch in.Command {
				case "create", "rewrite":
					c := strings.TrimLeft(in.Content, " \t\r\n")
					content = &c
				case "update":
					if in.Content != "" {
						c := in.Content
						content = &c
					} else if prev != nil && in.has["old_str"] {
						old := in.OldStr
						if !strings.Contains(prev.content, old) {
							old = strings.TrimSpace(old)
						}
						if old != "" && strings.Contains(prev.content, old) {
							c := strings.Replace(prev.content, old, in.NewStr, 1)
							content = &c
						}
					}
				}
			case b.Name == "create_file":
				if in.has["file_text"] {
					c := in.FileText
					content = &c
				}
			default: // str_replace: the old text must occur exactly once
				if prev != nil && in.OldStr != "" && strings.Count(prev.content, in.OldStr) == 1 {
					c := strings.Replace(prev.content, in.OldStr, in.NewStr, 1)
					content = &c
				} else if prev != nil {
					editsFailed++
				}
			}
			if content == nil {
				continue
			}
			a := &importArtifact{content: *content, version: 1}
			if prev != nil {
				a.kind, a.title, a.typ, a.language, a.version = prev.kind, prev.title, prev.typ, prev.language, prev.version+1
			} else if isArt {
				a.kind, a.title, a.typ, a.language = "artifact", in.Title, in.Type, in.Language
			} else {
				a.kind = "file"
				parts := strings.FieldsFunc(in.Path, func(r rune) bool { return r == '/' || r == '\\' })
				if len(parts) > 0 {
					a.title = strings.ReplaceAll(parts[len(parts)-1], "`", "")
				}
			}
			if strings.TrimSpace(a.title) == "" {
				a.title = "Untitled"
			}
			if len(a.title) > 200 {
				a.title = a.title[:200]
			}
			state[key] = a
			all = append(all, a)
			where[a] = [2]int{mi, bi}
		}
	}
	for _, a := range state {
		a.final = true
	}
	for _, a := range all {
		pos := where[a]
		var t string
		if a.final {
			t = renderArtifact(*a)
			kept = append(kept, keptArtifact{msg: pos[0], block: pos[1], art: a})
		} else {
			t = artifactHeader(*a) + " — updated; final version below."
		}
		texts[pos[0]][pos[1]] = &t
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].msg != kept[j].msg {
			return kept[i].msg < kept[j].msg
		}
		return kept[i].block < kept[j].block
	})
	return texts, kept, editsFailed
}

// --- the import ----------------------------------------------------------------

type importCounts struct {
	Conversations   int `json:"conversations"`
	Projects        int `json:"projects"`
	Files           int `json:"files"`
	Docs            int `json:"docs"`
	Artifacts       int `json:"artifacts"`
	FileEditsFailed int `json:"fileEditsFailed"`
	Skipped         int `json:"skipped"`
	messages        int
}

type importProjectRow struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	DirName string `json:"dirName"`
}

type importResult struct {
	SessionIDs           []string            `json:"sessionIds"`
	Titles               []string            `json:"titles"`
	JSONLPaths           []string            `json:"jsonlPaths"`
	CreatedAts           []*int64            `json:"createdAts"`
	UpdatedAts           []*int64            `json:"updatedAts"`
	Cwd                  string              `json:"cwd"`
	Projects             []importProjectRow  `json:"projects"`
	ConversationProjects map[string]*string  `json:"conversationProjects"`
	AttachmentFiles      map[string][]string `json:"attachmentFiles"`
	Counts               importCounts        `json:"counts"`
	manifestDiff         []string
}

// importLine is one transcript line in gocode's schema.
type importLine struct {
	Type         string        `json:"type"`
	UUID         string        `json:"uuid"`
	ParentUUID   *string       `json:"parentUuid"`
	Timestamp    string        `json:"timestamp,omitempty"`
	SessionID    string        `json:"sessionId"`
	Cwd          string        `json:"cwd"`
	Version      string        `json:"version"`
	UserType     string        `json:"userType"`
	IsSidechain  bool          `json:"isSidechain"`
	Message      importLineMsg `json:"message"`
	GocodeSchema int           `json:"gocodeSchema"`
}

type importLineMsg struct {
	Role    string            `json:"role"`
	Content []importLineBlock `json:"content"`
	ID      string            `json:"id,omitempty"`
}

type importLineBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseTimeMillis(s string) *int64 {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			ms := t.UnixMilli()
			return &ms
		}
	}
	return nil
}

// exportMessageText returns the first text block of an exported message, else
// its text field.
func exportMessageText(m exportMessage) string {
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != nil {
			return *b.Text
		}
	}
	return m.Text
}

// runImport performs the import (or a dry run).
func runImport(dataDir, exportPath, cwd string, dryRun bool) (*importResult, error) {
	ex, err := readImportExport(exportPath)
	if err != nil {
		return nil, err
	}
	store := sessionstore.New(dataDir)
	transcriptDir := store.ProjectDir(cwd)
	res := &importResult{Cwd: cwd, ConversationProjects: map[string]*string{}, AttachmentFiles: map[string][]string{}, Projects: []importProjectRow{}, SessionIDs: []string{}, Titles: []string{}, JSONLPaths: []string{}, CreatedAts: []*int64{}, UpdatedAts: []*int64{}}
	type namedProject struct {
		p   exportProject
		dir string
	}
	var projects []namedProject
	for _, p := range ex.projects {
		if d := projectDirName(p); d != "" {
			projects = append(projects, namedProject{p, d})
			res.Projects = append(res.Projects, importProjectRow{UUID: p.UUID, Name: p.Name, DirName: d})
		}
	}
	if !dryRun {
		for _, d := range []string{transcriptDir, filepath.Join(cwd, "projects"), filepath.Join(cwd, "files")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return nil, err
			}
		}
	}
	fileNames := map[string]string{}
	for _, c := range ex.conversations {
		for _, m := range c.Messages {
			for _, f := range m.Files {
				fileNames[f.FileUUID] = f.FileName
			}
		}
	}
	projectFiles := map[string]bool{}
	for _, np := range projects {
		dir := filepath.Join(cwd, "projects", np.dir)
		if !dryRun {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			if t := strings.TrimSpace(np.p.PromptTemplate); t != "" {
				if _, err := writeNew(filepath.Join(dir, "project-instructions.md"), []byte(t)); err != nil {
					return nil, err
				}
			}
		}
		for _, d := range np.p.Docs {
			name := ""
			if d.Filename != "" {
				name = importCt(d.Filename)
			}
			if !importAlnumRe.MatchString(name) {
				name = "untitled.md"
			}
			res.Counts.Docs++
			if !dryRun {
				if _, err := writeNew(filepath.Join(dir, importReserved(name)), []byte(d.Content)); err != nil {
					return nil, err
				}
			}
		}
		for _, f := range np.p.Files {
			data, ok := ex.files[f.FileUUID]
			if !ok {
				continue
			}
			projectFiles[f.FileUUID] = true
			res.Counts.Files++
			name := ""
			if f.FileName != "" {
				name = importCt(f.FileName)
			}
			if !importAlnumRe.MatchString(name) {
				name = f.FileUUID
			}
			if !dryRun {
				if _, err := writeNew(filepath.Join(dir, importReserved(name)), data); err != nil {
					return nil, err
				}
			}
		}
		res.Counts.Projects++
	}
	fileIDs := make([]string, 0, len(ex.files))
	for id := range ex.files {
		fileIDs = append(fileIDs, id)
	}
	sort.Strings(fileIDs)
	for _, id := range fileIDs {
		if projectFiles[id] {
			continue
		}
		if !dryRun {
			if _, err := writeNew(filepath.Join(cwd, "files", importFileName(id, fileNames[id])), ex.files[id]); err != nil {
				return nil, err
			}
		}
		res.Counts.Files++
	}

	budget := int64(importTotalByteCap)
	artifactNames := map[string]int{}
	for _, c := range ex.conversations {
		sessionID := ids.UUIDv5(c.UUID, importConvNamespace)
		jsonl := filepath.Join(transcriptDir, sessionID+".jsonl")
		already := false
		if !dryRun {
			_, statErr := os.Stat(jsonl)
			already = statErr == nil
		}
		var texts [][]*string
		var kept []keptArtifact
		if !already {
			var failed int
			texts, kept, failed = reconstructArtifacts(c.Messages, &budget)
			res.Counts.FileEditsFailed += failed
		} else {
			texts = make([][]*string, len(c.Messages))
			for i, m := range c.Messages {
				texts[i] = make([]*string, len(m.Content))
			}
		}
		var saved []string
		for _, k := range kept {
			stem, ext := artifactFileName(*k.art)
			key := strings.ToLower(stem + ext)
			rel := ""
			for try := 0; try < 1000; try++ {
				artifactNames[key]++
				n := artifactNames[key]
				name := stem + ext
				if n > 1 {
					name = fmt.Sprintf("%s-%d%s", stem, n, ext)
				}
				name = importReserved(name)
				if dryRun {
					rel = "files/" + name
					break
				}
				ok, err := writeNew(filepath.Join(cwd, "files", name), []byte(k.art.content))
				if err != nil {
					return nil, err
				}
				if ok {
					rel = "files/" + name
					break
				}
			}
			if rel == "" {
				continue
			}
			saved = append(saved, rel)
			t := *texts[k.msg][k.block] + "\n\n_Saved to " + rel + "_"
			texts[k.msg][k.block] = &t
		}
		res.Counts.Artifacts += len(saved)

		attached := map[string]bool{}
		var lines []importLine
		for mi, m := range c.Messages {
			line := importLine{UUID: m.UUID, ParentUUID: m.Parent, Timestamp: m.CreatedAt, SessionID: sessionID, Cwd: cwd, Version: importVersionTag, UserType: "external", GocodeSchema: sessionstore.SchemaVersion}
			if m.Sender == "human" {
				var refs []string
				seen := map[string]bool{}
				for _, f := range m.Files {
					if seen[f.FileUUID] {
						continue
					}
					seen[f.FileUUID] = true
					if _, ok := ex.files[f.FileUUID]; !ok {
						continue
					}
					rel := "files/" + importFileName(f.FileUUID, fileNames[f.FileUUID])
					if !projectFiles[f.FileUUID] {
						attached[rel] = true
					}
					refs = append(refs, `@"`+rel+`"`)
				}
				text := exportMessageText(m)
				for _, a := range m.Attachments {
					text += fmt.Sprintf("\n\n<file name=%q>\n%s\n</file>", a.FileName, a.ExtractedContent)
				}
				if len(refs) > 0 {
					text = strings.Join(refs, "\n") + "\n" + text
				}
				line.Type = "user"
				line.Message = importLineMsg{Role: "user", Content: []importLineBlock{{Type: "text", Text: text}}}
			} else {
				var blocks []importLineBlock
				for bi, b := range m.Content {
					if b.Type == "text" && b.Text != nil && strings.TrimSpace(*b.Text) != "" {
						blocks = append(blocks, importLineBlock{Type: "text", Text: *b.Text})
					} else if texts[mi][bi] != nil {
						blocks = append(blocks, importLineBlock{Type: "text", Text: *texts[mi][bi]})
					}
				}
				if len(blocks) == 0 {
					t := exportMessageText(m)
					if strings.TrimSpace(t) == "" {
						t = importNoContent
					}
					blocks = []importLineBlock{{Type: "text", Text: t}}
				}
				line.Type = "assistant"
				line.Message = importLineMsg{Role: "assistant", Content: blocks, ID: m.UUID}
			}
			lines = append(lines, line)
		}
		title := strings.TrimSpace(c.Name)
		if title == "" || title == "New conversation" {
			title = ""
			for _, m := range c.Messages {
				if m.Sender == "human" {
					title = strings.TrimSpace(m.Text)
					break
				}
			}
			if r := []rune(title); len(r) > 60 {
				title = string(r[:60])
			}
			if title == "" {
				title = "Untitled"
			}
		}
		res.SessionIDs = append(res.SessionIDs, sessionID)
		res.JSONLPaths = append(res.JSONLPaths, jsonl)
		res.Titles = append(res.Titles, title)
		res.CreatedAts = append(res.CreatedAts, parseTimeMillis(c.CreatedAt))
		res.UpdatedAts = append(res.UpdatedAts, parseTimeMillis(c.UpdatedAt))
		res.ConversationProjects[sessionID] = c.ProjectUUID
		var files []string
		for rel := range attached {
			files = append(files, rel)
		}
		files = append(files, saved...)
		sort.Strings(files)
		if len(files) > 0 {
			res.AttachmentFiles[sessionID] = files
		}
		res.Counts.messages += len(c.Messages)
		if !dryRun {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf) // one line per entry, no HTML escaping (JSON.stringify)
			enc.SetEscapeHTML(false)
			for _, l := range lines {
				if err := enc.Encode(l); err != nil {
					return nil, err
				}
			}
			ok, err := writeNew(jsonl, buf.Bytes())
			if err != nil {
				return nil, err
			}
			if !ok {
				res.Counts.Skipped++
				continue
			}
		}
		res.Counts.Conversations++
	}
	res.manifestDiff = manifestDiff(ex.manifest, res.Counts)
	return res, nil
}

// manifestDiff lists where the imported counts differ from the manifest.
func manifestDiff(m importManifest, c importCounts) []string {
	var out []string
	if len(m.Conversations) != c.Conversations+c.Skipped {
		out = append(out, fmt.Sprintf("conversations: manifest=%d imported=%d", len(m.Conversations), c.Conversations+c.Skipped))
	}
	msgs := 0
	for _, x := range m.Conversations {
		msgs += x.MessageCount
	}
	if msgs != c.messages {
		out = append(out, fmt.Sprintf("messages: manifest=%d imported=%d", msgs, c.messages))
	}
	if len(m.Projects) != c.Projects {
		out = append(out, fmt.Sprintf("projects: manifest=%d imported=%d", len(m.Projects), c.Projects))
	}
	docs := 0
	for _, x := range m.Projects {
		docs += x.DocCount
	}
	if docs != c.Docs {
		out = append(out, fmt.Sprintf("docs: manifest=%d imported=%d", docs, c.Docs))
	}
	return out
}

// runImportConversationsCmd runs `gocode import-conversations`.
func runImportConversationsCmd(env *Env, t []string) int {
	o, err := scanOpts(argsAfter(t, "import-conversations"), map[string]string{"--dry-run": "dryRun"}, map[string]string{"--cwd": "cwd"})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.pos) == 0 {
		fmt.Fprintln(env.Stderr, "error: missing required argument 'exportPath'")
		return 1
	}
	if !envutil.EnvTruthy(envGetenv(env)("GOCODE_IMPORT_CONVERSATIONS")) {
		fmt.Fprintln(env.Stderr, "import-conversations is not enabled")
		return 1
	}
	cwdOpt, _ := o.value("cwd")
	if cwdOpt == "" {
		fmt.Fprintln(env.Stderr, "--cwd is required")
		return 1
	}
	dataDir := sessionConfigHome(env, resolveSessionPaths(env))
	fail := func(err error) int {
		msg := doctorClean(err.Error())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		tooLarge := strings.HasPrefix(msg, "export file exceeds MAX_EXPORT_BYTES") || strings.HasPrefix(msg, "Archive total size is too large") || strings.HasPrefix(msg, "Archive contains too many files")
		fmt.Fprintf(env.Stderr, "import-conversations failed: %s\n", msg)
		if tooLarge {
			fmt.Fprintln(env.Stderr, "Import failed: the export exceeds the import size limit (IMPORT_TOO_LARGE)")
		} else {
			fmt.Fprintln(env.Stderr, "Import failed (IMPORT_ERROR)")
		}
		return 1
	}
	if dataDir == "" {
		return fail(errors.New("could not resolve the gocode data directory"))
	}
	cwd := cwdOpt
	if !filepath.IsAbs(cwd) {
		if base := sessionCwd(env); base != "" {
			cwd = filepath.Join(base, cwd)
		}
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fail(err)
	}
	exportPath := o.pos[0]
	if !filepath.IsAbs(exportPath) {
		if base := sessionCwd(env); base != "" {
			exportPath = filepath.Join(base, exportPath)
		}
	}
	dry := o.has("dryRun")
	res, err := runImport(dataDir, exportPath, real, dry)
	if err != nil {
		return fail(err)
	}
	prefix := ""
	if dry {
		prefix = "[dry-run] "
	}
	c := res.Counts
	fmt.Fprintf(env.Stdout, "%simported: conversations=%d skipped=%d messages=%d projects=%d docs=%d files=%d artifacts=%d\n", prefix, c.Conversations, c.Skipped, c.messages, c.Projects, c.Docs, c.Files, c.Artifacts)
	writeJSONLine(env.Stdout, res)
	if len(res.manifestDiff) > 0 {
		fmt.Fprintf(env.Stderr, "manifest mismatch:\n  %s\nImport completed with mismatches (IMPORT_MANIFEST_MISMATCH)\n", strings.Join(res.manifestDiff, "\n  "))
		return 1
	}
	return 0
}
