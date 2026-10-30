package toolreg

import (
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
)

func TestRegistryRegisterLookup(t *testing.T) {
	r := NewRegistry()
	d := ToolDescriptor{Name: protocol.ToolBash, RulePrefix: "Bash"}
	r.Register(d)

	got, ok := r.Lookup(protocol.ToolBash)
	if !ok {
		t.Fatal("Bash not found after Register")
	}
	if got.Name != protocol.ToolBash || got.Rule() != "Bash" {
		t.Fatalf("unexpected descriptor: %+v", got)
	}

	if _, ok := r.Lookup(protocol.ToolGrep); ok {
		t.Fatal("Grep should not be registered")
	}
}

func TestRegistryReRegisterKeepsPosition(t *testing.T) {
	r := NewRegistry()
	r.Register(ToolDescriptor{Name: protocol.ToolBash})
	r.Register(ToolDescriptor{Name: protocol.ToolGrep})
	// Re-register Bash with new metadata; it must stay first.
	r.Register(ToolDescriptor{Name: protocol.ToolBash, ReadOnly: true})

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("want 2 descriptors, got %d", len(list))
	}
	if list[0].Name != protocol.ToolBash || !list[0].ReadOnly {
		t.Fatalf("Bash should remain first and be updated: %+v", list[0])
	}
	if list[1].Name != protocol.ToolGrep {
		t.Fatalf("Grep should remain second: %+v", list[1])
	}
}

func TestRegistryRuleFallbackToName(t *testing.T) {
	d := ToolDescriptor{Name: protocol.ToolGlob}
	if d.Rule() != "Glob" {
		t.Fatalf("Rule() should fall back to Name, got %q", d.Rule())
	}
}

func TestDefaultRegistryHasAllBuiltins(t *testing.T) {
	r := DefaultRegistry()
	for _, tool := range protocol.AllTools() {
		if _, ok := r.Lookup(tool); !ok {
			t.Errorf("DefaultRegistry missing built-in tool %q", tool)
		}
	}
	if len(r.List()) != len(protocol.AllTools()) {
		t.Fatalf("DefaultRegistry size %d != AllTools %d", len(r.List()), len(protocol.AllTools()))
	}
}

func TestDefaultRegistryMetadata(t *testing.T) {
	r := DefaultRegistry()

	// Glob/Grep/Diff are read-only and concurrency-safe.
	for _, tool := range []protocol.Tool{protocol.ToolGlob, protocol.ToolGrep, protocol.ToolDiff} {
		d, _ := r.Lookup(tool)
		if !d.ReadOnly || !d.ConcurrencySafe {
			t.Errorf("%q should be read-only + concurrency-safe: %+v", tool, d)
		}
		if d.EditTool {
			t.Errorf("%q should not be an edit tool", tool)
		}
	}

	// Write/NotebookEdit are mutating edit tools.
	for _, tool := range []protocol.Tool{protocol.ToolWrite, protocol.ToolNotebookEdit} {
		d, _ := r.Lookup(tool)
		if d.ReadOnly {
			t.Errorf("%q should not be read-only", tool)
		}
		if !d.EditTool {
			t.Errorf("%q should be an edit tool", tool)
		}
	}

	// Bash is not read-only and not an edit tool (bashPrefixTool).
	if d, _ := r.Lookup(protocol.ToolBash); d.ReadOnly || d.EditTool {
		t.Errorf("Bash metadata wrong: %+v", d)
	}
}

// TestDefaultRegistryModelCallableMetadata checks the two newly integrated
// model-callable tools. TodoWrite and AskUserQuestion are listed with the
// expected metadata: both are keyed under their own rule, both mutate /
// interact (not read-only) and neither is concurrency-safe.
func TestDefaultRegistryModelCallableMetadata(t *testing.T) {
	r := DefaultRegistry()

	for _, tool := range []protocol.Tool{protocol.ToolTodoWrite, protocol.ToolAskUserQuestion} {
		d, ok := r.Lookup(tool)
		if !ok {
			t.Fatalf("DefaultRegistry missing %q", tool)
		}
		if d.ReadOnly {
			t.Errorf("%q should not be read-only: %+v", tool, d)
		}
		if d.ConcurrencySafe {
			t.Errorf("%q should not be concurrency-safe: %+v", tool, d)
		}
		if d.EditTool {
			t.Errorf("%q should not be an edit tool: %+v", tool, d)
		}
		if d.Rule() != string(tool) {
			t.Errorf("%q rule = %q, want %q", tool, d.Rule(), tool)
		}
	}
}

// TestDefaultRegistryDiffModsArePermissionOnly documents that Diff and Mods keep
// permission descriptors (so AllTools()/the permission layer stay complete) even
// though they are NOT model-callable tools; the callable-tool exclusion is
// enforced in internal/toolrun (no dispatcher adapter), covered by its tests.
func TestDefaultRegistryDiffModsArePermissionOnly(t *testing.T) {
	r := DefaultRegistry()
	for _, tool := range []protocol.Tool{protocol.ToolDiff, protocol.ToolMods} {
		if _, ok := r.Lookup(tool); !ok {
			t.Errorf("DefaultRegistry should still carry a permission descriptor for %q", tool)
		}
	}
}

// TestWorkflowDescriptorMirrorsAgent checks the Workflow tool's permission
// posture matches the Agent tool: read-only + concurrency-safe spawn front,
// AutoAllow with DeferInAutoPlan, keyed under its own "Workflow" rule. Mirroring
// Agent is the wiring directive; a deny/ask rule and the child-gating still apply.
func TestWorkflowDescriptorMirrorsAgent(t *testing.T) {
	r := DefaultRegistry()
	ag, ok := r.Lookup(protocol.ToolAgent)
	if !ok {
		t.Fatal("missing Agent descriptor")
	}
	wf, ok := r.Lookup(protocol.ToolWorkflow)
	if !ok {
		t.Fatal("missing Workflow descriptor")
	}
	if wf.ReadOnly != ag.ReadOnly || wf.ConcurrencySafe != ag.ConcurrencySafe ||
		wf.AutoAllow != ag.AutoAllow || wf.DeferInAutoPlan != ag.DeferInAutoPlan {
		t.Fatalf("Workflow posture %+v does not mirror Agent %+v", wf, ag)
	}
	if wf.Rule() != "Workflow" {
		t.Fatalf("Workflow rule = %q, want Workflow", wf.Rule())
	}
}

func TestRegistryNamesSorted(t *testing.T) {
	r := NewRegistry()
	r.Register(ToolDescriptor{Name: protocol.ToolWrite})
	r.Register(ToolDescriptor{Name: protocol.ToolBash})
	names := r.Names()
	if len(names) != 2 || names[0] != protocol.ToolBash || names[1] != protocol.ToolWrite {
		t.Fatalf("Names not sorted: %v", names)
	}
}

// TestDefaultRegistryFileAndWebTools checks the five fileedit/web tools carry
// the expected metadata: Read/WebFetch/WebSearch are read-only and
// concurrency-safe; Edit/MultiEdit are mutating, non-concurrent edit tools.
func TestDefaultRegistryFileAndWebTools(t *testing.T) {
	r := DefaultRegistry()
	cases := []struct {
		tool                         protocol.Tool
		readOnly, concurrent, editTl bool
	}{
		{protocol.ToolRead, true, true, false},
		{protocol.ToolEdit, false, false, true},
		{protocol.ToolMultiEdit, false, false, true},
		{protocol.ToolWebFetch, true, true, false},
		{protocol.ToolWebSearch, true, true, false},
	}
	for _, c := range cases {
		d, ok := r.Lookup(c.tool)
		if !ok {
			t.Fatalf("DefaultRegistry missing %q", c.tool)
		}
		if d.ReadOnly != c.readOnly || d.ConcurrencySafe != c.concurrent || d.EditTool != c.editTl {
			t.Errorf("%q metadata = %+v, want readOnly=%v concurrent=%v edit=%v", c.tool, d, c.readOnly, c.concurrent, c.editTl)
		}
		if d.Rule() != string(c.tool) {
			t.Errorf("%q rule = %q", c.tool, d.Rule())
		}
	}
}
