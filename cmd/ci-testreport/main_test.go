package main

import (
	"bytes"
	"strings"
	"testing"
)

const sample = `ok  	example.com/a	0.1s
--- FAIL: TestOne (0.00s)
    one_test.go:10: first line
        continued
    --- FAIL: TestOne/sub (0.00s)
        one_test.go:12: sub failed
--- FAIL: TestTwo (5.00s)
    two_test.go:3: 100% broken
FAIL
FAIL	example.com/b	5.1s
--- FAIL: TestPanics (0.00s)
panic: boom [recovered]
	panic: boom

goroutine 1 [running]:
main.f()
FAIL	example.com/c	0.2s
# example.com/d
d/d.go:3:2: undefined: x
FAIL	example.com/d [build failed]
==================
WARNING: DATA RACE
Write at 0x00c by goroutine 7:
==================
--- FAIL: TestRacy (0.01s)
    testing.go:1490: race detected during execution of test
FAIL
FAIL	example.com/e	1.0s
FAIL	example.com/f	0.0s
FAIL
`

func TestParseGroupsFailuresByPackage(t *testing.T) {
	got := parse(strings.NewReader(sample))
	var labels []string
	for _, f := range got {
		labels = append(labels, f.label())
	}
	want := []string{
		"example.com/b TestOne",
		"example.com/b TestTwo",
		"example.com/c TestPanics",
		"example.com/d build failed",
		"example.com/e data race",
		"example.com/e TestRacy",
		"example.com/f",
	}
	if strings.Join(labels, "|") != strings.Join(want, "|") {
		t.Fatalf("labels = %q\nwant %q", labels, want)
	}
	if n := len(got[0].lines); n != 5 {
		t.Errorf("TestOne block has %d lines: %q", n, got[0].lines)
	}
	if !strings.Contains(strings.Join(got[2].lines, "\n"), "goroutine 1 [running]") {
		t.Errorf("panic block lost its trace: %q", got[2].lines)
	}
	if !strings.Contains(strings.Join(got[3].lines, "\n"), "undefined: x") {
		t.Errorf("build block = %q", got[3].lines)
	}
}

func TestReportEscapesAndSummarises(t *testing.T) {
	var out, sum bytes.Buffer
	fails := parse(strings.NewReader(sample))
	report(&out, &sum, "Test failure", 2, "", fails)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a summary plus 2 annotations, got %d:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "::error title=Test failure%3A 7 failing::") {
		t.Errorf("summary annotation = %q", lines[0])
	}
	if !strings.Contains(lines[2], "100%25 broken") || !strings.Contains(lines[1], "first line%0A        continued") {
		t.Errorf("annotations not escaped:\n%s", out.String())
	}
	if !strings.Contains(sum.String(), "example.com/f") || !strings.Contains(sum.String(), "goroutine 1 [running]") {
		t.Errorf("step summary incomplete:\n%s", sum.String())
	}
}
