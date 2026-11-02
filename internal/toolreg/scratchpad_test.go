package toolreg

import (
	"os"
	"path/filepath"
	"testing"

	pathguard "github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
)

func TestScratchpadReadAndWriteNeedNoPrompt(t *testing.T) {
	d, f, root := fileDecider(t)
	pad := filepath.Join(root, "tmp", "gocode-1000", "proj", "sid", "scratchpad")
	if err := os.MkdirAll(pad, 0o700); err != nil {
		t.Fatal(err)
	}
	f.ScratchpadDir = pad
	p := filepath.Join(pad, "notes.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := Call{Tool: protocol.ToolRead, Rule: "Read", RuleContent: p, Path: p, Op: pathguard.OpRead}
	if got := d.Decide(rc, ModeDefault, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("read: %+v", got)
	}
	if got := d.Decide(writeCall(filepath.Join(pad, "out.json")), ModeDefault, nil, nil); got.Behavior != BehaviorAllow {
		t.Errorf("write: %+v", got)
	}

	// A file next to the scratchpad is outside the working directories: asked.
	other := filepath.Join(root, "tmp", "other.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	oc := Call{Tool: protocol.ToolRead, Rule: "Read", RuleContent: other, Path: other, Op: pathguard.OpRead}
	if got := d.Decide(oc, ModeDefault, nil, nil); got.Behavior != BehaviorAsk {
		t.Errorf("read outside: %+v", got)
	}

	// A link in the scratchpad that leads out of it is not a free read.
	link := filepath.Join(pad, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	lc := Call{Tool: protocol.ToolRead, Rule: "Read", RuleContent: link, Path: link, Op: pathguard.OpRead}
	if got := d.Decide(lc, ModeDefault, nil, nil); got.Behavior != BehaviorAsk {
		t.Errorf("read through link: %+v", got)
	}

	// Without a scratchpad the same read asks.
	f.ScratchpadDir = ""
	if got := d.Decide(rc, ModeDefault, nil, nil); got.Behavior != BehaviorAsk {
		t.Errorf("no scratchpad: %+v", got)
	}
}
