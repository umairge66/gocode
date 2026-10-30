package exec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRoundTripNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "nested", "out.txt")
	tool := WriteTool{}
	res, err := tool.Execute(WriteInput{FilePath: path, Content: "round-trip\n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Created {
		t.Error("Created = false, want true for a new file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "round-trip\n" {
		t.Errorf("content = %q, want %q", got, "round-trip\n")
	}
}

func TestWriteOverwriteRequiresRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default tool (nil tracker) must refuse an overwrite of an existing,
	// not-yet-read file with the expected message.
	tool := WriteTool{}
	if _, err := tool.Execute(WriteInput{FilePath: path, Content: "new"}); err != ErrFileNotRead {
		t.Fatalf("err = %v, want ErrFileNotRead", err)
	}
	if err := ErrFileNotRead.Error(); err != MsgFileNotRead {
		t.Errorf("message = %q, want MsgFileNotRead", err)
	}

	// With a tracker that reports the file as read, the overwrite succeeds.
	toolRead := WriteTool{Tracker: AllowAllReads{}}
	if _, err := toolRead.Execute(WriteInput{FilePath: path, Content: "new"}); err != nil {
		t.Fatalf("overwrite after read: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

type recordedReads map[string]bool

func (r recordedReads) WasRead(p string) bool { return r[p] }

func TestWriteOverwriteWithSelectiveTracker(t *testing.T) {
	dir := t.TempDir()
	read := filepath.Join(dir, "read.txt")
	unread := filepath.Join(dir, "unread.txt")
	for _, p := range []string{read, unread} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := WriteTool{Tracker: recordedReads{read: true}}

	if _, err := tool.Execute(WriteInput{FilePath: read, Content: "y"}); err != nil {
		t.Errorf("read file overwrite: %v", err)
	}
	if _, err := tool.Execute(WriteInput{FilePath: unread, Content: "y"}); err != ErrFileNotRead {
		t.Errorf("unread file: err = %v, want ErrFileNotRead", err)
	}
}

func TestWriteEncodingRefusals(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    error
	}{
		{"nul", "ab\x00cd", ErrContentNUL},
		{"utf16be", "\xFE\xFFab", ErrContentUTF16},
		{"invalid-utf8", "ab\xC3\x28", ErrContentUTF8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".bin")
			if _, err := (WriteTool{}).Execute(WriteInput{FilePath: path, Content: c.content}); err != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Error("file should not have been created on refusal")
			}
		})
	}
}

func TestWriteEmptyPath(t *testing.T) {
	if _, err := (WriteTool{}).Execute(WriteInput{Content: "x"}); err != ErrEmptyPath {
		t.Fatalf("err = %v, want ErrEmptyPath", err)
	}
}

func TestWriteRequireAbs(t *testing.T) {
	tool := WriteTool{RequireAbs: true}
	if _, err := tool.Execute(WriteInput{FilePath: "relative/path.txt", Content: "x"}); err != ErrNonAbsPath {
		t.Fatalf("err = %v, want ErrNonAbsPath", err)
	}
}
