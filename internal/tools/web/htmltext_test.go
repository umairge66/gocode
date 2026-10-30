package web

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown(t *testing.T) {
	src := `<!doctype html><html><head><title>Doc Title</title>
<style>body{color:red}</style><script>alert("x<b>")</script></head>
<body><nav><a href="/home">Home</a> | <a href="/about">About</a></nav>
<h1>Main   Heading</h1>
<p>Hello <b>bold</b> and <em>em</em> &amp; a <a href="https://example.com/x">link   text</a>.</p>
<!-- a comment -->
<h2>Sub</h2>
<ul><li>one</li><li>two <code>x()</code></li></ul>
<ol><li>first</li><li>second</li></ol>
<pre><code>func main() {
	fmt.Println("hi")
}</code></pre>
<noscript>enable js</noscript>
<p>Line<br>break</p>
</body></html>`
	got := HTMLToMarkdown(src)
	for _, want := range []string{
		"# Main Heading",
		"## Sub",
		"Hello **bold** and *em* & a [link text](https://example.com/x).",
		"- one",
		"- two `x()`",
		"1. first",
		"2. second",
		"```\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```",
		"Line\nbreak",
		"# Doc Title",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	for _, bad := range []string{"alert", "color:red", "Home", "About", "enable js", "a comment", "<"} {
		if strings.Contains(got, bad) {
			t.Errorf("output should not contain %q:\n%s", bad, got)
		}
	}
}

func TestHTMLToMarkdownNestedListsAndAttrs(t *testing.T) {
	got := HTMLToMarkdown(`<ul><li>a<ul><li>b</li></ul></li></ul><p><a href='https://e.com/q?a=1&amp;b=2' title="x > y">q</a></p><img src="i.png" alt="pic">`)
	if !strings.Contains(got, "- a\n  - b") {
		t.Errorf("nested list wrong:\n%s", got)
	}
	if !strings.Contains(got, "[q](https://e.com/q?a=1&b=2)") {
		t.Errorf("attr parse wrong:\n%s", got)
	}
	if !strings.Contains(got, "![pic](i.png)") {
		t.Errorf("img wrong:\n%s", got)
	}
}

func TestStripTags(t *testing.T) {
	if got := StripTags("The <strong>Go</strong>  &amp; you"); got != "The Go & you" {
		t.Fatalf("got %q", got)
	}
}
