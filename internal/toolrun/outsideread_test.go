package toolrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/toolreg"
)

// Reads outside the working directories ask again: inside → allow,
// outside → ask with a session Read(<dir>/**) suggestion, an allow rule or an
// additional directory → allow, a deny rule → deny.

func outsideFixture(t *testing.T) (proj, outside, file string) {
	t.Helper()
	proj, outside = autoFixture(t)
	file = filepath.Join(outside, "o.txt")
	if err := os.WriteFile(file, []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "in.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return proj, outside, file
}

func recordPrompter(kind PermissionResponseKind, persisted *[]string) *fakePrompter {
	return &fakePrompter{respond: func(req PermissionRequest) (PermissionResponse, error) {
		resp := PermissionResponse{Kind: kind}
		if kind == PermissionAllowedAlways && len(req.Suggested) > 0 {
			if !req.SessionOnly && persisted != nil {
				*persisted = append(*persisted, req.Suggested[0])
			}
			c := strings.TrimSuffix(strings.TrimPrefix(req.Suggested[0], "Read("), ")")
			resp.Rule = &toolreg.Rule{Behavior: toolreg.RuleAllow, ToolName: "Read", Content: c, Source: "session"}
		}
		return resp, nil
	}}
}

func TestOutsideReadInsideAllowedWithoutPrompt(t *testing.T) {
	proj, _, _ := outsideFixture(t)
	p := recordPrompter(PermissionDenied, nil)
	d := New(toolreg.ModeDefault, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithPermissionPrompter(p))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": filepath.Join(proj, "in.txt")}); res.IsError {
		t.Fatalf("inside read: %+v", res)
	}
	if res := dispatch(t, d, string(protocol.ToolGrep), map[string]any{"pattern": "needle"}); res.IsError {
		t.Fatalf("inside grep (no path): %+v", res)
	}
	if len(p.requests) != 0 {
		t.Fatalf("inside reads prompted: %+v", p.requests)
	}
}

func TestOutsideReadAsks(t *testing.T) {
	proj, outside, file := outsideFixture(t)
	for _, mode := range []toolreg.Mode{toolreg.ModeDefault, toolreg.ModeAcceptEdits, toolreg.ModePlan} {
		p := recordPrompter(PermissionDenied, nil)
		d := New(mode, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithPermissionPrompter(p))
		if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); !res.IsError {
			t.Fatalf("%s: denied outside read ran: %+v", mode, res)
		}
		if len(p.requests) != 1 {
			t.Fatalf("%s: prompts = %d", mode, len(p.requests))
		}
		req := p.requests[0]
		wantRule := "Read(//" + strings.TrimPrefix(outside, "/") + "/**)"
		if len(req.Suggested) != 1 || req.Suggested[0] != wantRule || !req.SessionOnly ||
			req.AlwaysAllowLabel != "Yes, allow reading from "+outside+"/ during this session" ||
			req.Notice != "gocode requested permissions to read from "+file+", but you haven't granted it yet." {
			t.Fatalf("%s: request = %+v", mode, req)
		}
	}
	// dontAsk denies; bypass and auto allow.
	d := New(toolreg.ModeDontAsk, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); !res.IsError {
		t.Fatalf("dontAsk: %+v", res)
	}
	for _, m := range []toolreg.Mode{toolreg.ModeBypassPermissions, toolreg.ModeAuto} {
		d := New(m, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
		if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); res.IsError {
			t.Fatalf("%s: %+v", m, res)
		}
	}
	// Headless: denied with the read message and how to allow it.
	d = New(toolreg.ModeDefault, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file})
	if !res.IsError || !strings.Contains(res.Content, "read from "+file) || !strings.Contains(res.Content, "--add-dir") {
		t.Fatalf("headless: %+v", res)
	}
}

func TestOutsideReadDontAskAgainIsSessionRule(t *testing.T) {
	proj, outside, file := outsideFixture(t)
	var persisted []string
	p := recordPrompter(PermissionAllowedAlways, &persisted)
	d := New(toolreg.ModeDefault, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithPermissionPrompter(p))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); res.IsError {
		t.Fatalf("approved read: %+v", res)
	}
	// The same directory no longer asks — for Read, Grep and Glob.
	if res := dispatch(t, d, string(protocol.ToolGrep), map[string]any{"pattern": "needle", "path": outside}); res.IsError {
		t.Fatalf("grep after don't-ask-again: %+v", res)
	}
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); res.IsError {
		t.Fatalf("re-read: %+v", res)
	}
	if len(p.requests) != 1 {
		t.Fatalf("prompts = %d, want 1", len(p.requests))
	}
	if len(persisted) != 0 {
		t.Fatalf("session-only rule persisted: %v", persisted)
	}
}

func TestOutsideReadAllowRuleAndAdditionalDir(t *testing.T) {
	proj, outside, file := outsideFixture(t)
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleAllow, ToolName: "Read", Content: "//" + strings.TrimPrefix(outside, "/") + "/**"}}
	d := New(toolreg.ModeDefault, rules, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); res.IsError {
		t.Fatalf("allow rule: %+v", res)
	}
	// An additional working directory (--add-dir / additionalDirectories).
	f := sessionFilesFor(proj, t.TempDir(), false)
	f.AdditionalWorkingDirs = []string{outside}
	d = New(toolreg.ModeDefault, nil, WithSessionFiles(f))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); res.IsError {
		t.Fatalf("additional dir: %+v", res)
	}
}

func TestOutsideReadDenyRuleAndBlockSetting(t *testing.T) {
	proj, outside, file := outsideFixture(t)
	p := recordPrompter(PermissionAllowedOnce, nil)
	rules := toolreg.RuleSet{{Behavior: toolreg.RuleDeny, ToolName: "Read", Content: "//" + strings.TrimPrefix(outside, "/") + "/**"}}
	d := New(toolreg.ModeDefault, rules, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), false)), WithPermissionPrompter(p))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); !res.IsError {
		t.Fatalf("deny rule: %+v", res)
	}
	d = New(toolreg.ModeAuto, nil, WithSessionFiles(sessionFilesFor(proj, t.TempDir(), true)), WithPermissionPrompter(p))
	if res := dispatch(t, d, string(protocol.ToolRead), map[string]any{"file_path": file}); !res.IsError || !strings.Contains(res.Content, "blockReadsOutsideWorkingDirectories") && !strings.Contains(res.Content, pathguard.ReasonReadsOutsideBlocked) {
		t.Fatalf("block setting: %+v", res)
	}
	if len(p.requests) != 0 {
		t.Fatal("denied reads prompted")
	}
}
