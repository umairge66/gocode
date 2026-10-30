package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func editHook(t *testing.T, f *subcmdFixture, edit string) (int, string, string) {
	t.Helper()
	f.stdin = edit
	return f.run(t, "edit-hook", "--json")
}

func userHooks(t *testing.T, f *subcmdFixture) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(subcmdReadFile(t, f.userSettingsPath())), &obj); err != nil {
		t.Fatal(err)
	}
	h, _ := obj["hooks"].(map[string]any)
	return h
}

func TestEditHookAddReplaceRemove(t *testing.T) {
	f := newSubcmdFixture(t)
	subcmdWriteFile(t, f.userSettingsPath(), `{"theme":"dark"}`)
	add := `{"op":"add","source":"userSettings","event":"PreToolUse","matcher":"Bash","hook":{"type":"command","command":"echo hi","junk":1}}`
	code, out, errOut := editHook(t, f, add)
	if code != 0 || strings.TrimSpace(out) != `{"ok":true}` {
		t.Fatalf("add exit %d out %q err %q", code, out, errOut)
	}
	h := userHooks(t, f)
	groups := h["PreToolUse"].([]any)
	g := groups[0].(map[string]any)
	if g["matcher"] != "Bash" || g["hooks"].([]any)[0].(map[string]any)["command"] != "echo hi" {
		t.Fatalf("hooks = %v", h)
	}
	if _, has := g["hooks"].([]any)[0].(map[string]any)["junk"]; has {
		t.Fatal("unknown key was written")
	}

	// Duplicate.
	code, out, errOut = editHook(t, f, add)
	if code != 1 || !strings.Contains(out, `"code":"duplicate"`) || !strings.Contains(errOut, "an identical hook is already configured") {
		t.Fatalf("dup exit %d out %q err %q", code, out, errOut)
	}

	// Replace in place.
	rep := `{"op":"replace","source":"userSettings","event":"PreToolUse","matcher":"Bash","hook":{"type":"command","command":"echo bye"},"target":{"event":"PreToolUse","matcher":"Bash","hook":{"type":"command","command":"echo hi"}}}`
	if code, out, errOut = editHook(t, f, rep); code != 0 {
		t.Fatalf("replace exit %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(subcmdReadFile(t, f.userSettingsPath()), "echo bye") {
		t.Fatal("replace not applied")
	}

	// Target missing.
	code, out, _ = editHook(t, f, rep)
	if code != 1 || !strings.Contains(out, `"code":"target_missing"`) {
		t.Fatalf("missing exit %d out %q", code, out)
	}

	// Move to another event, then remove it.
	mv := `{"op":"replace","source":"userSettings","event":"Stop","hook":{"type":"command","command":"echo bye"},"target":{"event":"PreToolUse","matcher":"Bash","hook":{"type":"command","command":"echo bye"}}}`
	if code, out, errOut = editHook(t, f, mv); code != 0 {
		t.Fatalf("move exit %d out %q err %q", code, out, errOut)
	}
	h = userHooks(t, f)
	if _, still := h["PreToolUse"]; still {
		t.Fatalf("emptied event kept: %v", h)
	}
	rm := `{"op":"remove","source":"userSettings","event":"Stop","target":{"event":"Stop","hook":{"type":"command","command":"echo bye"}}}`
	if code, out, errOut = editHook(t, f, rm); code != 0 {
		t.Fatalf("remove exit %d out %q err %q", code, out, errOut)
	}
	got := subcmdReadFile(t, f.userSettingsPath())
	if strings.Contains(got, "hooks") || !strings.Contains(got, `"theme"`) {
		t.Fatalf("after remove: %s", got)
	}
}

func TestEditHookRefusals(t *testing.T) {
	f := newSubcmdFixture(t)
	code, _, errOut := f.run(t, "edit-hook")
	if code != 1 || !strings.Contains(errOut, "required option '--json'") {
		t.Fatalf("no --json: %d %q", code, errOut)
	}
	f.stdinTTY = true
	code, _, errOut = f.run(t, "edit-hook", "--json")
	if code != 1 || !strings.Contains(errOut, "not by hand") {
		t.Fatalf("tty: %d %q", code, errOut)
	}
	f.stdinTTY = false
	code, _, errOut = editHook(t, f, "not json")
	if code != 1 || !strings.Contains(errOut, "stdin is not a JSON edit") {
		t.Fatalf("bad json: %d %q", code, errOut)
	}
	for edit, want := range map[string]string{
		`{"op":"zap"}`:                                                  "op must be add, replace, or remove",
		`{"op":"add","source":"policySettings"}`:                        "source must be",
		`{"op":"add","source":"userSettings","event":"Nope"}`:           "not a known hook event",
		`{"op":"add","source":"userSettings","event":"Stop","hook":{}}`: "is not a valid hook (type)",
		`{"op":"add","source":"userSettings","event":"Stop","matcher":"constructor","hook":{"type":"command","command":"x"}}`: "matcher is not allowed",
		`{"op":"remove","source":"userSettings","event":"Stop"}`:                                                              "target must be an object",
		`{"op":"add","source":"userSettings","event":"Stop","hook":{"type":"http","url":"nope"}}`:                             "(url)",
	} {
		code, out, errOut := editHook(t, f, edit)
		if code != 1 || !strings.Contains(out, `"code":"bad_request"`) || !strings.Contains(errOut, want) {
			t.Errorf("%s: exit %d out %q err %q", edit, code, out, errOut)
		}
	}
	f.vars["GOCODE_CODE_CHILD_SESSION"] = "1"
	code, _, errOut = editHook(t, f, `{}`)
	if code != 1 || !strings.Contains(errOut, "started inside a gocode session") {
		t.Fatalf("child session: %d %q", code, errOut)
	}
}

func TestEditHookHeaderTwins(t *testing.T) {
	settings := map[string]any{}
	base := map[string]any{"type": "http", "url": "https://h.example/x", "headers": map[string]any{"Authorization": "a"}}
	req := hookEditRequest{Op: "add", Source: "userSettings", Event: "Stop", Hook: base}
	if out, _ := applyHookEdit(settings, req); out != "applied" {
		t.Fatalf("first add = %s", out)
	}
	twin := map[string]any{"type": "http", "url": "https://h.example/x", "headers": map[string]any{"Authorization": "b"}}
	if out, _ := applyHookEdit(settings, hookEditRequest{Op: "add", Source: "userSettings", Event: "Stop", Hook: twin}); out != "twin-by-headers" {
		t.Fatalf("twin add = %s", out)
	}
	// A blank header value in a replacement keeps the stored secret.
	repl := map[string]any{"type": "http", "url": "https://h.example/y", "headers": map[string]any{"Authorization": ""}}
	out, _ := applyHookEdit(settings, hookEditRequest{Op: "replace", Source: "userSettings", Event: "Stop", Hook: repl, Target: &hookEditTarget{Event: "Stop", Hook: base}})
	if out != "applied" {
		t.Fatalf("replace = %s", out)
	}
	h := settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if h["url"] != "https://h.example/y" || h["headers"].(map[string]any)["Authorization"] != "a" {
		t.Fatalf("merged = %v", h)
	}
}
