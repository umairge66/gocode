package frontmatter

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseSubset(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want map[string]any
	}{
		{"plain scalars", "a: hello world\nb: 42\nc: -3\nd: 1.5\ne: true\nf: False\ng: null\nh: ~\ni:\n", map[string]any{
			"a": "hello world", "b": int64(42), "c": int64(-3), "d": 1.5, "e": true, "f": false, "g": nil, "h": nil, "i": nil,
		}},
		{"yes stays a string (YAML 1.2)", "a: yes\nb: 0x1F\nc: 0o17\n", map[string]any{"a": "yes", "b": int64(31), "c": int64(15)}},
		{"double quoted escapes", `a: "tab\there \"q\" \\ \u00e9 \x41 nl\n"` + "\n", map[string]any{"a": "tab\there \"q\" \\ é A nl\n"}},
		{"single quoted", "a: 'it''s # not a comment'\nb: '42'\n", map[string]any{"a": "it's # not a comment", "b": "42"}},
		{"quoted keys", "\"my key\": 1\n'other': two\n", map[string]any{"my key": int64(1), "other": "two"}},
		{"comments", "# leading\na: value # trailing\nb: has#hash\nc: \"q # kept\" # cut\n  # indented comment\n", map[string]any{
			"a": "value", "b": "has#hash", "c": "q # kept",
		}},
		{"inline list", "tools: [Read, \"Bash(git:*)\", 'x, y', 3]\nempty: []\n", map[string]any{
			"tools": []any{"Read", "Bash(git:*)", "x, y", int64(3)}, "empty": []any{},
		}},
		{"block list", "tools:\n  - Read\n  - \"Write\"\n  -   Bash(git add:*)\n", map[string]any{
			"tools": []any{"Read", "Write", "Bash(git add:*)"},
		}},
		{"block list same indent", "tools:\n- a\n- b\nnext: 1\n", map[string]any{"tools": []any{"a", "b"}, "next": int64(1)}},
		{"flow map + multi-line flow", "m: {a: 1, b: [x, y]}\nl: [one,\n  two]\n", map[string]any{
			"m": map[string]any{"a": int64(1), "b": []any{"x", "y"}}, "l": []any{"one", "two"},
		}},
		{"nested map two levels", "metadata:\n  owner: me\n  inner:\n    deep: true\nafter: x\n", map[string]any{
			"metadata": map[string]any{"owner": "me", "inner": map[string]any{"deep": true}}, "after": "x",
		}},
		{"hooks shape", "hooks:\n  PreToolUse:\n    - matcher: \"Bash\"\n      hooks:\n        - type: command\n          command: echo hi\n", map[string]any{
			"hooks": map[string]any{"PreToolUse": []any{map[string]any{
				"matcher": "Bash",
				"hooks":   []any{map[string]any{"type": "command", "command": "echo hi"}},
			}}},
		}},
		{"literal clip", "a: |\n  line1\n    indented\n  line3\n\nb: 1\n", map[string]any{"a": "line1\n  indented\nline3\n", "b": int64(1)}},
		{"literal strip", "a: |-\n  x\n  y\n\n\n", map[string]any{"a": "x\ny"}},
		{"literal keep", "a: |+\n  x\n\n\nb: 2\n", map[string]any{"a": "x\n\n\n", "b": int64(2)}},
		{"folded", "a: >\n  one\n  two\n\n  three\n    more\n  four\n", map[string]any{"a": "one two\nthree\n  more\nfour\n"}},
		{"folded strip with comment", "a: >- # note\n  x\n  y\n", map[string]any{"a": "x y"}},
		{"explicit indent indicator", "a: |2\n    two-extra\n  base\n", map[string]any{"a": "  two-extra\nbase\n"}},
		{"multi-line plain", "description: A long\n  description that\n  wraps\nx: 1\n", map[string]any{"description": "A long description that wraps", "x": int64(1)}},
		{"multi-line double quoted", "a: \"first\n  second\n\n  third\"\n", map[string]any{"a": "first second\nthird"}},
		{"CRLF", "a: one\r\nb:\r\n  - x\r\nc: |\r\n  l1\r\n  l2\r\n", map[string]any{"a": "one", "b": []any{"x"}, "c": "l1\nl2\n"}},
		{"colon in value", "argument-hint: use: like this\nurl: http://x.y/z\n", map[string]any{"argument-hint": "use: like this", "url": "http://x.y/z"}},
		{"star", "allowed-tools: *\n", map[string]any{"allowed-tools": "*"}},
		{"empty", "\n# only comments\n", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.src)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %#v\nwant %#v", got, c.want)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		frag      string
	}{
		{"unterminated double", "a: 1\nb: \"open\n", 2, "unterminated"},
		{"unterminated flow", "a: [x, y\n", 1, "unterminated flow"},
		{"junk after flow", "a: [x] y\n", 1, "after flow"},
		{"junk after quote", "a: \"x\" y\n", 1, "after quoted"},
		{"bad escape", "a: 1\nb: 2\nc: \"\\q\"\n", 3, "escape"},
		{"not a key", "a: 1\njust text\n", 2, "expected"},
		{"bad indentation", "a: 1\n    b: 2\n", 2, "indentation"},
		{"tab indent", "a:\n\t- x\n", 2, "tab"},
		{"seq in map", "a: 1\n- x\n", 2, "sequence"},
		{"bad block header", "a: |x\n  y\n", 1, "block scalar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.src)
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SyntaxError", err)
			}
			if se.Line != c.line || !strings.Contains(se.Error(), c.frag) {
				t.Fatalf("err = %q (line %d), want line %d containing %q", se.Error(), se.Line, c.line, c.frag)
			}
		})
	}
}

func TestParseNotTable(t *testing.T) {
	if _, err := Parse("- a\n- b\n"); !errors.Is(err, ErrNotTable) {
		t.Fatalf("err = %v, want ErrNotTable", err)
	}
}

func TestParseDocument(t *testing.T) {
	d := ParseDocument("\ufeff---\r\ndescription: hi\r\n---\r\nbody --- here\n")
	if !d.HasFrontmatter || d.Data["description"] != "hi" || d.Body != "body --- here\n" || d.Err != nil {
		t.Fatalf("doc = %#v", d)
	}
	// A "---" inside a value does not close the block.
	d = ParseDocument("---\ndescription: a---b\n---\nB")
	if d.Data["description"] != "a---b" || d.Body != "B" {
		t.Fatalf("doc = %#v", d)
	}
	// No frontmatter.
	d = ParseDocument("# Title\ntext")
	if d.HasFrontmatter || d.Body != "# Title\ntext" || len(d.Data) != 0 {
		t.Fatalf("doc = %#v", d)
	}
}

func TestParseDocumentLenientRetry(t *testing.T) {
	// Strict parse fails (junk after the flow list); the lenient retry's
	// quoting makes the value a literal string.
	d := ParseDocument("---\nargument-hint: [file] [options]\ndescription: ok\n---\n")
	if d.Err != nil || d.Data["argument-hint"] != "[file] [options]" || d.Data["description"] != "ok" {
		t.Fatalf("doc = %#v", d)
	}
	// Tabs in indentation are repaired by the retry.
	d = ParseDocument("---\ntools:\n\t- Read\n---\n")
	if d.Err != nil || !reflect.DeepEqual(d.Data["tools"], []any{"Read"}) {
		t.Fatalf("doc = %#v", d)
	}
}

func TestParseDocumentErrorKeepsBody(t *testing.T) {
	d := ParseDocument("---\ndescription: fine\nbroken: \"unterminated\n---\nThe body\n")
	// The unterminated string (not repairable by the lenient retry) is a
	// parse error; the body is still returned and Data is empty.
	if d.Err == nil {
		t.Fatal("want a parse error")
	}
	var se *SyntaxError
	if !errors.As(d.Err, &se) || se.Line != 3 {
		t.Fatalf("err = %v, want file line 3", d.Err)
	}
	if len(d.Data) != 0 || d.Body != "The body\n" {
		t.Fatalf("doc = %#v", d)
	}
}

func TestHelpers(t *testing.T) {
	if String(1.0) != "1" || String(int64(7)) != "7" || String(true) != "true" || String([]any{"a", "b"}) != "a,b" || String(nil) != "" {
		t.Fatal("String coercions wrong")
	}
	for in, want := range map[any]bool{"yes": true, "ON": true, " 1 ": true, "off": false, "No": false, int64(0): false, true: true} {
		if v, ok := Bool(in); !ok || v != want {
			t.Errorf("Bool(%v) = %v,%v", in, v, ok)
		}
	}
	if _, ok := Bool("maybe"); ok {
		t.Error("Bool(maybe) should be unrecognised")
	}
	if l, ok := ToolList("Bash(git add:*), Read Write"); !ok || !reflect.DeepEqual(l, []string{"Bash(git add:*)", "Read", "Write"}) {
		t.Errorf("ToolList = %#v", l)
	}
	if l, _ := ToolList([]any{"Read", "*"}); !reflect.DeepEqual(l, []string{"*"}) {
		t.Errorf("ToolList star = %#v", l)
	}
	if _, ok := ToolList(nil); ok {
		t.Error("nil should be absent")
	}
}
