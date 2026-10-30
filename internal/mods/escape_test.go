package mods

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSymlinkedModuleOutsideThePluginIsRefused: a plugin (installed from git,
// which keeps symbolic links) whose hooks/mod.ts is a link to a file outside
// the plugin must not have that file read by the bundler — imports were
// already held to the plugin's real folder, the entry module was not.
func TestSymlinkedModuleOutsideThePluginIsRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET-TOKEN-1234 = not code"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeMod(t, `export function register(on) {}`)
	mod := filepath.Join(dir, "hooks", "mod.ts")
	if err := os.Remove(mod); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, mod); err != nil {
		t.Skip("no symlinks:", err)
	}
	if p, err := ModulePath(dir); err == nil {
		t.Errorf("ModulePath accepted a module linking outside the plugin: %s", p)
	}
	_, err := Compile(dir, mod)
	if err == nil {
		t.Fatal("Compile read a module linking outside the plugin")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("the outside file's text leaked into the error: %v", err)
	}

	// A linked hooks/ folder is refused the same way.
	dir2 := writeMod(t, `export function register(on) {}`)
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "mod.ts"), []byte(`export function register(on) {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "hooks.json"), []byte(`{"modules": ["./mod.ts"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir2, "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outDir, filepath.Join(dir2, "hooks")); err != nil {
		t.Fatal(err)
	}
	if p, err := ModulePath(dir2); err == nil {
		t.Errorf("ModulePath accepted a module in a linked-out hooks folder: %s", p)
	}
}

// TestMemoryBombIsInterrupted: goja has no heap limit, and the time watchdog
// alone let a tight allocating loop grow the process by gigabytes inside its
// 10 s budget. A job that grows the heap past the limit is interrupted and
// passed over like one that outran its time.
func TestMemoryBombIsInterrupted(t *testing.T) {
	oldLimit := jobMemoryLimit.Swap(64 << 20)
	t.Cleanup(func() { jobMemoryLimit.Store(oldLimit) })

	dir := writeMod(t, `
const keep = []
export function register(on) {
  on('tool.call', ($, e, next) => {
    if (e.command === 'bomb') {
      // Bounded so a failing guard costs ~600 MB, not the machine.
      for (let i = 0; i < 600; i++) keep.push(new Array(1 << 17).fill(i))
      return { result: 'bomb finished' }
    }
    return next(e)
  })
}`)
	en := newEngine(t, Options{HookBudget: 60 * time.Second})
	load(t, en, "bomb", dir, TierUser)
	var ran atomic.Int32
	res, err := en.Dispatch(context.Background(), "tool.call", map[string]any{"tool": "Bash", "tool_use_id": "t", "command": "bomb"}, coreTool(&ran))
	if err != nil {
		t.Fatal(err)
	}
	if res["result"] == "bomb finished" || ran.Load() != 1 {
		t.Fatalf("the allocating hook was not interrupted and passed over: %v", res)
	}
}
