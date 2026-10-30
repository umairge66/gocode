// Command ci-testreport turns the text output of `go test` into GitHub Actions
// annotations: one per failing test (or panic, data race, build failure),
// each carrying that failure's full output block, plus one summary
// annotation naming every failure. GitHub shows only the first few error
// annotations of a step, so the complete set is also written as Markdown to
// $GITHUB_STEP_SUMMARY when that variable is set.
//
// Usage:
//
//	ci-testreport [-title "Test failure"] [-max 30] test-output.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Limits keep each annotation and the step summary within GitHub's sizes.
const (
	maxBlockLines   = 80
	maxMessageBytes = 6000
	maxSummaryBytes = 900 << 10
	maxListedNames  = 100
	tailLines       = 40
)

// failure is one failing unit and the output lines that belong to it.
type failure struct {
	pkg   string
	name  string
	lines []string
}

func (f failure) label() string {
	if f.pkg == "" {
		return f.name
	}
	if f.name == "" {
		return f.pkg
	}
	return f.pkg + " " + f.name
}

type blockKind int

const (
	kindTest   blockKind = iota // --- FAIL: block; ends at the first unindented line
	kindGreedy                  // panic or build failure; runs to the package result line
	kindRace                    // WARNING: DATA RACE; runs to the closing separator
)

// parse splits go test output into failures. Blocks printed before a
// package's "FAIL\t<pkg>" line are attributed to that package.
func parse(r io.Reader) []failure {
	var (
		out     []failure
		pending []failure
		cur     *failure
		kind    blockKind
	)
	finish := func() {
		if cur != nil {
			pending = append(pending, *cur)
			cur = nil
		}
	}
	start := func(name string, k blockKind, line string) {
		finish()
		cur = &failure{name: name, lines: []string{line}}
		kind = k
	}
	flush := func(pkg string) {
		for i := range pending {
			if pending[i].pkg == "" {
				pending[i].pkg = pkg
			}
		}
		out = append(out, pending...)
		pending = nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "FAIL\t"):
			finish()
			fields := strings.Fields(line)
			pkg := ""
			if len(fields) > 1 {
				pkg = fields[1]
			}
			if len(pending) == 0 {
				// A package that failed without a failing test (TestMain exit,
				// setup or build failure): the result line names the cause.
				name := ""
				if i := strings.Index(line, "["); i >= 0 {
					name = line[i:]
				}
				pending = append(pending, failure{name: name, lines: []string{line}})
			}
			flush(pkg)
		case strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "?   \t"):
			finish()
			if len(pending) > 0 {
				flush(strings.Fields(line)[1])
			}
		case strings.HasPrefix(line, "--- FAIL: "):
			name := strings.TrimPrefix(line, "--- FAIL: ")
			if i := strings.LastIndex(name, " ("); i > 0 {
				name = name[:i]
			}
			start(name, kindTest, line)
		case line == "WARNING: DATA RACE":
			start("data race", kindRace, line)
		case strings.HasPrefix(line, "# ") && (cur == nil || kind != kindGreedy):
			start("build failed", kindGreedy, line)
		case strings.HasPrefix(line, "panic: ") && (cur == nil || kind == kindTest):
			if cur == nil {
				start("panic", kindGreedy, line)
			} else {
				cur.lines = append(cur.lines, line)
				kind = kindGreedy
			}
		case cur == nil:
		case kind == kindRace:
			cur.lines = append(cur.lines, line)
			if strings.HasPrefix(line, "==================") {
				finish()
			}
		case kind == kindGreedy:
			cur.lines = append(cur.lines, line)
		default: // kindTest
			if line != "" && line[0] != ' ' && line[0] != '\t' {
				finish()
				continue
			}
			cur.lines = append(cur.lines, line)
		}
	}
	finish()
	flush("")
	return out
}

// escapeData escapes an annotation message.
func escapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

// escapeProperty escapes an annotation property such as the title.
func escapeProperty(s string) string {
	s = escapeData(s)
	s = strings.ReplaceAll(s, ":", "%3A")
	return strings.ReplaceAll(s, ",", "%2C")
}

// body joins a failure's lines, keeping the message within the size limits.
func body(lines []string) string {
	cut := false
	if len(lines) > maxBlockLines {
		lines, cut = lines[:maxBlockLines], true
	}
	s := strings.Join(lines, "\n")
	if len(s) > maxMessageBytes {
		s, cut = s[:maxMessageBytes], true
	}
	if cut {
		s += "\n… (truncated; see the job log or summary)"
	}
	return s
}

func annotate(w io.Writer, title, msg string) {
	fmt.Fprintf(w, "::error title=%s::%s\n", escapeProperty(title), escapeData(msg))
}

func lastLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"cannot read test output: " + err.Error()}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func report(w io.Writer, summary io.Writer, title string, limit int, path string, fails []failure) {
	if len(fails) == 0 {
		annotate(w, title, "No failing test found in the output; last lines:\n"+strings.Join(lastLines(path, tailLines), "\n"))
		return
	}
	names := make([]string, 0, len(fails))
	for i, f := range fails {
		if i == maxListedNames {
			names = append(names, fmt.Sprintf("… and %d more", len(fails)-i))
			break
		}
		names = append(names, f.label())
	}
	annotate(w, fmt.Sprintf("%s: %d failing", title, len(fails)), strings.Join(names, "\n"))
	for i, f := range fails {
		if i == limit {
			break
		}
		annotate(w, title+": "+f.label(), body(f.lines))
	}

	if summary == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%d)\n\n", title, len(fails))
	for _, f := range fails {
		block := fmt.Sprintf("<details><summary><code>%s</code></summary>\n\n```text\n%s\n```\n\n</details>\n\n",
			f.label(), strings.ReplaceAll(strings.Join(f.lines, "\n"), "```", "ʼʼʼ"))
		if b.Len()+len(block) > maxSummaryBytes {
			b.WriteString("_Output truncated; see the job log._\n")
			break
		}
		b.WriteString(block)
	}
	_, _ = io.WriteString(summary, b.String())
}

func main() {
	title := flag.String("title", "Test failure", "annotation title prefix")
	limit := flag.Int("max", 30, "maximum number of per-failure annotations")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ci-testreport [-title T] [-max N] test-output.txt")
		os.Exit(2)
	}
	path := flag.Arg(0)
	var fails []failure
	if f, err := os.Open(path); err == nil {
		fails = parse(f)
		_ = f.Close()
	}
	var summary io.Writer
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
			defer f.Close()
			summary = f
		}
	}
	report(os.Stdout, summary, *title, *limit, path, fails)
}
