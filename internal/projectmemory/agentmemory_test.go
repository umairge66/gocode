package projectmemory

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentMemoryDir: one directory per scope, named after the sanitized
// agent type, with a trailing separator.
func TestAgentMemoryDir(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		scope, want string
	}{
		{AgentMemoryUser, filepath.Join("/cfg", "agent-memory", "my-agent-v2") + sep},
		{AgentMemoryProject, filepath.Join("/w", ".gocode", "agent-memory", "my-agent-v2") + sep},
		{AgentMemoryLocal, filepath.Join("/w", ".gocode", "agent-memory-local", "my-agent-v2") + sep},
		{"team", ""},
	}
	for _, c := range cases {
		got := AgentMemoryDir(AgentMemoryConfig{AgentType: "my agent.v2", Scope: c.scope, ConfigDir: "/cfg", Cwd: "/w"})
		if got != c.want {
			t.Errorf("AgentMemoryDir(%s) = %q, want %q", c.scope, got, c.want)
		}
	}
	if got := SanitizeAgentName(""); got != "unknown" {
		t.Errorf("SanitizeAgentName(\"\") = %q", got)
	}
}

// TestAgentMemoryPromptWithIndex: the section carries the guidelines, the
// scope line, and the MEMORY.md body; the directory is created.
func TestAgentMemoryPromptWithIndex(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/cfg/agent-memory/reviewer/MEMORY.md": "- [Style](style.md) — prefers tabs\n",
	})
	var made []string
	out := AgentMemoryPrompt(AgentMemoryConfig{
		AgentType: "reviewer", Scope: AgentMemoryUser, ConfigDir: "/cfg", Cwd: "/w", FS: fs,
		MkdirAll: func(d string) error { made = append(made, d); return nil },
	})
	for _, want := range []string{
		"# Persistent Agent Memory",
		"You have a persistent, file-based memory system at `" + filepath.Join("/cfg", "agent-memory", "reviewer") + string(filepath.Separator) + "`.",
		"## Types of memory",
		"## How to save memories",
		"- Since this memory is user-scope, keep learnings general since they apply across all projects",
		"## MEMORY.md\n\n- [Style](style.md) — prefers tabs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len(made) != 1 || made[0] != filepath.Join("/cfg", "agent-memory", "reviewer") {
		t.Errorf("MkdirAll calls = %v", made)
	}
}

// TestAgentMemoryPromptEmpty: no MEMORY.md yet yields the "currently empty"
// note and the full (writable) guidelines.
func TestAgentMemoryPromptEmpty(t *testing.T) {
	fs := newFS("/w", nil).MkdirAll("/w")
	out := AgentMemoryPrompt(AgentMemoryConfig{AgentType: "a", Scope: AgentMemoryProject, Cwd: "/w", FS: fs})
	if !strings.Contains(out, "Your MEMORY.md is currently empty.") {
		t.Errorf("missing empty note:\n%s", out)
	}
	if !strings.Contains(out, "project-scope and shared with your team") {
		t.Error("missing project scope guideline")
	}
}

// TestAgentMemoryProjectLinkedOut: a project memory directory that is a link
// leading out of the working copy is not read and is read-only.
func TestAgentMemoryProjectLinkedOut(t *testing.T) {
	fs := newFS("/w", map[string]string{
		"/elsewhere/MEMORY.md": "secret index",
	})
	fs.MkdirAll("/w/.gocode/agent-memory")
	fs.Symlink("/elsewhere", "/w/.gocode/agent-memory/a")
	called := false
	out := AgentMemoryPrompt(AgentMemoryConfig{
		AgentType: "a", Scope: AgentMemoryProject, Cwd: "/w", FS: fs,
		MkdirAll: func(string) error { called = true; return nil },
	})
	if strings.Contains(out, "secret index") {
		t.Error("MEMORY.md behind an outward link must not be read")
	}
	if !strings.Contains(out, "This memory is read-only this session") || !strings.Contains(out, "was not loaded") {
		t.Errorf("expected the read-only wording:\n%s", out)
	}
	if strings.Contains(out, "## How to save memories") {
		t.Error("a read-only memory must not describe saving")
	}
	if called {
		t.Error("the directory must not be created when MEMORY.md may not be read")
	}
}

// TestTruncateEntrypoint: more than 200 lines are cut with a warning naming
// the first dropped line.
func TestTruncateEntrypoint(t *testing.T) {
	var lines []string
	for i := 0; i < 205; i++ {
		lines = append(lines, "- entry")
	}
	lines[200] = "- first dropped"
	got := truncateEntrypoint(strings.Join(lines, "\n"))
	if strings.Count(got, "- entry") != 200 {
		t.Errorf("kept %d entries, want 200", strings.Count(got, "- entry"))
	}
	want := "> WARNING: MEMORY.md is 205 lines (limit: 200). Only part of it was loaded: 5 of 205 lines were cut off, starting at line 201 (\"- first dropped\")."
	if !strings.Contains(got, want) {
		t.Errorf("warning missing; tail = %q", got[len(got)-300:])
	}
	if short := truncateEntrypoint("  one line \n"); short != "one line" {
		t.Errorf("short index = %q", short)
	}
}
