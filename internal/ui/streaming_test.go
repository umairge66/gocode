package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// noThrottle disables the tail-render throttle for the duration of a test so
// every frame reflects the latest delta.
func noThrottle(t *testing.T) {
	t.Helper()
	prev := tailThrottle
	tailThrottle = 0
	t.Cleanup(func() { tailThrottle = prev })
}

// streamFrame renders a streaming (Done:false) assistant message alone, in the
// given style, and returns its plain and styled text.
func streamFrame(style UIStyle, text string, done bool) (plain, styled string) {
	vs := Reduce(New(80), StreamAssistant{Text: text, Done: done})
	lr := NewLipglossRenderer()
	lr.SetStyle(style)
	styled = strings.Join(lr.RenderTranscript(vs), "\n")
	return ansi.Strip(styled), styled
}

// TestStreamingOpenFenceRendersHighlighted is the reported bug: while a code
// block is still being generated it must already render as code — highlighted,
// with no literal ``` fence line — in every style.
func TestStreamingOpenFenceRendersHighlighted(t *testing.T) {
	noThrottle(t)
	src := "Here is the script:\n\n```php\n<?php\necho \"hello\";\n$total = 4"
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		streamCacheReset()
		plain, styled := streamFrame(style, src, false)
		if strings.Contains(plain, "```") {
			t.Errorf("[%s] raw fence marker visible while streaming:\n%s", style, plain)
		}
		if !strings.Contains(plain, `echo "hello";`) || !strings.Contains(plain, "$total = 4") {
			t.Errorf("[%s] streamed code missing:\n%s", style, plain)
		}
		// Highlighted: the keyword carries its own color sequence, i.e. the code
		// went through the syntax highlighter rather than a plain dim style.
		i := strings.Index(styled, "echo")
		if i < 0 || !strings.Contains(styled[max(i-24, 0):i], "\x1b[") {
			t.Errorf("[%s] code is not syntax-highlighted while streaming", style)
		}
	}
}

// TestStreamingFinalEqualsStreamed checks the frame after Done equals the last
// streamed frame for the same text: nothing jumps when the stream finishes.
func TestStreamingFinalEqualsStreamed(t *testing.T) {
	noThrottle(t)
	doc := "# Plan\n\nWe will **refactor** the `parser`.\n\n1. Split the lexer\n2. Add tests\n\n" +
		"```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nDone."
	for _, style := range []UIStyle{StyleModern, StyleCards, StyleClassic} {
		streamCacheReset()
		// Stream it delta by delta, then compare the full-text streamed frame
		// with the finalized one.
		var last string
		for i := 1; i <= len(doc); i += 7 {
			_, last = streamFrame(style, doc[:i], false)
		}
		_, last = streamFrame(style, doc, false)
		_, final := streamFrame(style, doc, true)
		if last != final {
			t.Errorf("[%s] finalized render differs from the last streamed frame\n--- streamed\n%s\n--- final\n%s",
				style, ansi.Strip(last), ansi.Strip(final))
		}
	}
}

// TestStreamingNestedFences checks a 4-backtick fence that wraps ``` lines is
// treated as one block (the inner fences do not close it), and that a fence
// closed by a longer run counts as closed.
func TestStreamingNestedFences(t *testing.T) {
	noThrottle(t)
	streamCacheReset()
	src := "````md\n```php\necho 1;\n```\nstill inside"
	sp := splitMarkdownBlocks(src)
	if sp.open == nil || sp.open.n != 4 || sp.open.ch != '`' {
		t.Fatalf("outer 4-backtick fence should still be open, got %+v", sp.open)
	}
	plain, _ := streamFrame(StyleModern, src, false)
	if !strings.Contains(plain, "```php") || !strings.Contains(plain, "still inside") {
		t.Errorf("inner fence lines must render as code content of the outer fence:\n%s", plain)
	}
	if strings.Contains(plain, "````") {
		t.Errorf("outer fence marker visible:\n%s", plain)
	}

	// Closed by a longer run: the fence is closed and the next paragraph is a
	// block of its own.
	sp = splitMarkdownBlocks("```go\nx := 1\n`````\n\nafter")
	if sp.open != nil || len(sp.done) != 1 || sp.tail != "after" {
		t.Errorf("fence closed by a longer run: got done=%q tail=%q open=%v", sp.done, sp.tail, sp.open)
	}
	// A blank line inside a fence is not a block boundary.
	sp = splitMarkdownBlocks("```go\na\n\nb\n```\n\nnext")
	if len(sp.done) != 1 || !strings.Contains(sp.done[0], "\n\nb") {
		t.Errorf("blank line inside a fence split the block: %q", sp.done)
	}
}

// TestStreamingPartialConstructs covers the other half-written constructs.
func TestStreamingPartialConstructs(t *testing.T) {
	noThrottle(t)
	cases := []struct {
		name, src string
		want      []string
		reject    []string
	}{
		{"inline backtick", "Run the `go vet", []string{"go vet"}, []string{"`"}},
		{"bold", "This is **important", []string{"important"}, []string{"**"}},
		{"heading", "## Partial head", []string{"Partial head"}, []string{"##"}},
		{"list", "Steps:\n\n- first\n- sec", []string{"first", "sec"}, nil},
		{"setext guard", "A paragraph\n-", []string{"A paragraph"}, nil},
		{"partial fence opener", "Look:\n``", []string{"Look:"}, []string{"``"}},
		{"table without separator", "| name | size |\n| foo", []string{"| name | size |", "| foo"}, nil},
	}
	for _, c := range cases {
		streamCacheReset()
		plain, _ := streamFrame(StyleModern, c.src, false)
		for _, w := range c.want {
			if !strings.Contains(plain, w) {
				t.Errorf("%s: missing %q:\n%s", c.name, w, plain)
			}
		}
		for _, r := range c.reject {
			if strings.Contains(plain, r) {
				t.Errorf("%s: unexpected %q:\n%s", c.name, r, plain)
			}
		}
	}
	// The setext guard keeps the paragraph a paragraph (not an H2 heading).
	streamCacheReset()
	md, _ := prepareTail("A paragraph\n-", nil)
	if md != "A paragraph" {
		t.Errorf("lone '-' under a paragraph should be held back, got %q", md)
	}
	// A table renders as a table once its separator row arrives.
	streamCacheReset()
	plain, _ := streamFrame(StyleModern, "| name | size |\n|---|---|\n| foo | 1", false)
	if strings.Contains(plain, "|---") || !strings.Contains(plain, "name") || !strings.Contains(plain, "foo") {
		t.Errorf("table with separator should render as a table:\n%s", plain)
	}
}

// TestStreamingOnlyTailRerenders checks the cost model: completed blocks are
// rendered once; a delta that only grows the tail renders no complete block
// again, and the tail render is throttled.
func TestStreamingOnlyTailRerenders(t *testing.T) {
	mdRenderCacheReset()
	streamCacheReset()
	prev := tailThrottle
	tailThrottle = time.Hour
	defer func() { tailThrottle = prev }()

	base := "# A\n\npara one\n\n```go\nx := 1\n```\n\ntail"
	_ = renderStreamingMarkdown(base, 60, true)
	blocksRendered := mdRenderComputeCount
	if blocksRendered != 3 {
		t.Fatalf("three complete blocks should render once each, got %d", blocksRendered)
	}
	first := strings.Join(renderStreamingMarkdown(base, 60, true), "\n")
	grown := strings.Join(renderStreamingMarkdown(base+" grows", 60, true), "\n")
	if mdRenderComputeCount != blocksRendered {
		t.Errorf("growing the tail re-rendered a complete block: %d -> %d", blocksRendered, mdRenderComputeCount)
	}
	if grown != first {
		t.Errorf("a delta inside the throttle window should reuse the previous tail render")
	}
	tailThrottle = 0
	grown = strings.Join(renderStreamingMarkdown(base+" grows", 60, true), "\n")
	if !strings.Contains(ansi.Strip(grown), "tail grows") {
		t.Errorf("tail not re-rendered once the throttle allows:\n%s", ansi.Strip(grown))
	}
}

// TestStreamingLongOpenFenceChunks checks a long code block that is still
// open renders contiguously (chunked: no gaps, no fence markers, every line),
// and that a delta re-renders only the growing chunk.
func TestStreamingLongOpenFenceChunks(t *testing.T) {
	noThrottle(t)
	mdRenderCacheReset()
	streamCacheReset()
	var b strings.Builder
	b.WriteString("```go\n")
	for i := 0; i < 60; i++ {
		b.WriteString("x" + strings.Repeat("y", i%7) + " := 1\n")
	}
	lines := renderStreamingMarkdown(b.String(), 70, true)
	plain := ansi.Strip(strings.Join(lines, "\n"))
	if strings.Contains(plain, "```") {
		t.Fatalf("fence marker visible:\n%s", plain)
	}
	code := 0
	for _, ln := range strings.Split(plain, "\n") {
		if strings.Contains(ln, ":= 1") {
			code++
		} else if strings.TrimSpace(ln) == "" {
			t.Fatalf("blank line inside the chunked code block:\n%s", plain)
		}
	}
	if code != 60 {
		t.Fatalf("got %d code lines, want 60:\n%s", code, plain)
	}
	chunks := mdRenderComputeCount
	b.WriteString("z := 2\n")
	_ = renderStreamingMarkdown(b.String(), 70, true)
	if mdRenderComputeCount != chunks {
		t.Errorf("a delta re-rendered completed chunks: %d -> %d", chunks, mdRenderComputeCount)
	}
}
