package web

// This file is a small, stdlib-only HTML -> markdown-ish text converter.
// golang.org/x/net/html is not a module dependency, so this is a forgiving
// hand-rolled tokenizer that handles the subset of HTML that matters for
// readable page text: headings, paragraphs, links, lists, emphasis, inline code
// and preformatted blocks. style/script/noscript/iframe elements, navigation
// and other page chrome are dropped.

import (
	"html"
	"regexp"
	"strings"
)

// skipElements are elements whose entire subtree is dropped from the output.
// script/style/noscript/iframe never carry readable text; the rest is page
// chrome or non-text content.
var skipElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "iframe": true,
	"nav": true, "svg": true, "template": true, "head": true,
	"object": true, "embed": true, "canvas": true, "select": true,
	"button": true, "form": true, "dialog": true,
}

// rawTextElements have content that is not markup; their body is scanned up to
// the matching end tag without tokenizing.
var rawTextElements = map[string]bool{
	"script": true, "style": true, "textarea": true, "title": true,
	"xmp": true, "noscript": true, "iframe": true,
}

// voidElements never have an end tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// blockElements force a paragraph break before and after.
var blockElements = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"header": true, "footer": true, "aside": true, "blockquote": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true,
	"figure": true, "figcaption": true, "address": true, "details": true,
	"summary": true, "dl": true, "fieldset": true, "body": true, "html": true,
}

type listState struct {
	ordered bool
	n       int
}

type linkState struct {
	href  string
	start int // byte offset in out where the link text began
}

// outBuf is a byte buffer that, unlike strings.Builder, can be truncated so a
// link's already-written label can be rewritten in place in O(len(label)).
type outBuf struct{ b []byte }

func (o *outBuf) addString(s string) { o.b = append(o.b, s...) }
func (o *outBuf) addByte(c byte)     { o.b = append(o.b, c) }
func (o *outBuf) Len() int           { return len(o.b) }
func (o *outBuf) String() string     { return string(o.b) }
func (o *outBuf) truncate(n int)     { o.b = o.b[:n] }
func (o *outBuf) hasSuffix(s string) bool {
	return len(o.b) >= len(s) && string(o.b[len(o.b)-len(s):]) == s
}
func (o *outBuf) last() (byte, bool) {
	if len(o.b) == 0 {
		return 0, false
	}
	return o.b[len(o.b)-1], true
}

type converter struct {
	out       outBuf
	skipDepth int
	skipName  []string
	preDepth  int
	lists     []listState
	links     []linkState
	// pendingSpace records collapsed whitespace not yet written.
	pendingSpace bool
	title        string
}

// HTMLToMarkdown converts an HTML document to readable markdown-ish text.
// Headings become "#" lines, links "[text](href)", list items "- " / "1. ",
// <pre> blocks fenced code, <code> inline backticks. script/style/nav and
// similar chrome are stripped. Character references are decoded.
func HTMLToMarkdown(src string) string {
	c := &converter{}
	c.run(src)
	text := c.out.String()
	if c.title != "" && !strings.Contains(text, c.title) {
		text = "# " + c.title + "\n\n" + text
	}
	return tidy(text)
}

var (
	multiBlank   = regexp.MustCompile(`\n{3,}`)
	trailingWS   = regexp.MustCompile(`[ \t]+\n`)
	wsRun        = regexp.MustCompile(`[ \t\r\n\f]+`)
	tagNameRunes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-:"
)

func tidy(s string) string {
	s = trailingWS.ReplaceAllString(s, "\n")
	s = multiBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func (c *converter) run(src string) {
	i := 0
	n := len(src)
	for i < n {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			c.text(src[i:])
			return
		}
		if lt > 0 {
			c.text(src[i : i+lt])
		}
		i += lt
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return
			}
			i += 4 + end + 3
			continue
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return
			}
			i += end + 1
			continue
		}
		closing := false
		j := 1
		if j < len(rest) && rest[j] == '/' {
			closing = true
			j++
		}
		k := j
		for k < len(rest) && strings.IndexByte(tagNameRunes, rest[k]) >= 0 {
			k++
		}
		if k == j {
			// Not a tag ("<" followed by something else): literal text.
			c.text("<")
			i++
			continue
		}
		name := strings.ToLower(rest[j:k])
		end := findTagEnd(rest, k)
		if end < 0 {
			return
		}
		attrSrc := rest[k:end]
		selfClose := strings.HasSuffix(strings.TrimSpace(attrSrc), "/")
		i += end + 1
		if closing {
			c.endTag(name)
			continue
		}
		attrs := parseAttrs(attrSrc)
		if rawTextElements[name] && !selfClose {
			closeIdx := indexFold(src[i:], "</"+name)
			body := ""
			if closeIdx < 0 {
				body = src[i:]
				i = n
			} else {
				body = src[i : i+closeIdx]
				gt := strings.IndexByte(src[i+closeIdx:], '>')
				if gt < 0 {
					i = n
				} else {
					i += closeIdx + gt + 1
				}
			}
			switch name {
			case "title":
				if c.title == "" {
					c.title = strings.TrimSpace(wsRun.ReplaceAllString(html.UnescapeString(body), " "))
				}
			case "textarea", "xmp":
				if c.skipDepth == 0 {
					c.text(body)
				}
			}
			continue
		}
		c.startTag(name, attrs, selfClose)
	}
}

// findTagEnd returns the index of the '>' closing a tag, honouring quoted
// attribute values.
func findTagEnd(s string, from int) int {
	var quote byte
	for p := from; p < len(s); p++ {
		ch := s[p]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			quote = ch
		case '>':
			return p
		}
	}
	return -1
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

// parseAttrs extracts name=value pairs from a tag's attribute source.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	p := 0
	for p < len(s) {
		for p < len(s) && (isSpace(s[p]) || s[p] == '/') {
			p++
		}
		start := p
		for p < len(s) && !isSpace(s[p]) && s[p] != '=' && s[p] != '/' {
			p++
		}
		if start == p {
			p++
			continue
		}
		name := strings.ToLower(s[start:p])
		for p < len(s) && isSpace(s[p]) {
			p++
		}
		val := ""
		if p < len(s) && s[p] == '=' {
			p++
			for p < len(s) && isSpace(s[p]) {
				p++
			}
			if p < len(s) && (s[p] == '"' || s[p] == '\'') {
				q := s[p]
				p++
				vs := p
				for p < len(s) && s[p] != q {
					p++
				}
				val = s[vs:p]
				if p < len(s) {
					p++
				}
			} else {
				vs := p
				for p < len(s) && !isSpace(s[p]) {
					p++
				}
				val = s[vs:p]
			}
		}
		if _, dup := attrs[name]; !dup {
			attrs[name] = html.UnescapeString(val)
		}
	}
	return attrs
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// text emits character data, collapsing whitespace outside <pre>.
func (c *converter) text(raw string) {
	if c.skipDepth > 0 || raw == "" {
		return
	}
	s := html.UnescapeString(raw)
	if c.preDepth > 0 {
		c.out.addString(s)
		return
	}
	if strings.TrimSpace(s) == "" {
		if s != "" {
			c.pendingSpace = true
		}
		return
	}
	lead := isSpace(s[0])
	trail := isSpace(s[len(s)-1])
	s = wsRun.ReplaceAllString(strings.TrimSpace(s), " ")
	if (lead || c.pendingSpace) && !c.atLineStart() {
		c.out.addByte(' ')
	}
	c.out.addString(s)
	c.pendingSpace = trail
}

func (c *converter) atLineStart() bool {
	last, ok := c.out.last()
	if !ok {
		return true
	}
	return last == '\n' || last == ' ' || last == '(' || last == '['
}

// block ensures the output ends with a blank line (paragraph break).
func (c *converter) block() {
	c.pendingSpace = false
	if c.out.Len() == 0 {
		return
	}
	switch {
	case c.out.hasSuffix("\n\n"):
	case c.out.hasSuffix("\n"):
		c.out.addByte('\n')
	default:
		c.out.addString("\n\n")
	}
}

// newline ensures the output ends with a single line break.
func (c *converter) newline() {
	c.pendingSpace = false
	if c.out.Len() > 0 && !c.out.hasSuffix("\n") {
		c.out.addByte('\n')
	}
}

func (c *converter) inline(s string) {
	if c.skipDepth > 0 {
		return
	}
	if c.pendingSpace && !c.atLineStart() {
		c.out.addByte(' ')
	}
	c.pendingSpace = false
	c.out.addString(s)
}

func (c *converter) startTag(name string, attrs map[string]string, selfClose bool) {
	if c.skipDepth > 0 {
		if !voidElements[name] && !selfClose {
			c.skipName = append(c.skipName, name)
			c.skipDepth++
		}
		return
	}
	if skipElements[name] || attrs["aria-hidden"] == "true" || hasHidden(attrs) {
		if !voidElements[name] && !selfClose {
			c.skipName = append(c.skipName, name)
			c.skipDepth++
		}
		return
	}
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.block()
		c.out.addString(strings.Repeat("#", int(name[1]-'0')) + " ")
	case "br":
		if c.preDepth > 0 {
			c.out.addByte('\n')
		} else {
			c.newline()
		}
	case "hr":
		c.block()
		c.out.addString("---\n\n")
	case "pre":
		c.block()
		c.out.addString("```\n")
		c.preDepth++
	case "code", "kbd", "samp", "tt":
		if c.preDepth == 0 {
			c.inline("`")
		}
	case "strong", "b":
		c.inline("**")
	case "em", "i":
		c.inline("*")
	case "a":
		c.inline("")
		c.links = append(c.links, linkState{href: strings.TrimSpace(attrs["href"]), start: c.out.Len()})
	case "img":
		if alt := strings.TrimSpace(attrs["alt"]); alt != "" {
			c.inline("![" + alt + "](" + attrs["src"] + ")")
		}
	case "ul", "ol":
		c.newline()
		if len(c.lists) == 0 {
			c.block()
		}
		c.lists = append(c.lists, listState{ordered: name == "ol"})
	case "li":
		c.newline()
		depth := len(c.lists)
		if depth == 0 {
			c.out.addString("- ")
			return
		}
		c.out.addString(strings.Repeat("  ", depth-1))
		top := &c.lists[depth-1]
		if top.ordered {
			top.n++
			c.out.addString(itoa(top.n) + ". ")
		} else {
			c.out.addString("- ")
		}
	case "tr", "dt", "dd":
		c.newline()
	case "td", "th":
		c.inline(" | ")
	case "blockquote":
		c.block()
		c.out.addString("> ")
	default:
		if blockElements[name] {
			c.block()
		}
	}
}

func hasHidden(attrs map[string]string) bool {
	_, ok := attrs["hidden"]
	return ok
}

func (c *converter) endTag(name string) {
	if c.skipDepth > 0 {
		// Pop to the matching open skip element (tolerates misnesting).
		for idx := len(c.skipName) - 1; idx >= 0; idx-- {
			if c.skipName[idx] == name {
				c.skipDepth -= len(c.skipName) - idx
				c.skipName = c.skipName[:idx]
				break
			}
		}
		return
	}
	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		c.block()
	case "pre":
		if c.preDepth > 0 {
			c.preDepth--
			if !c.out.hasSuffix("\n") {
				c.out.addByte('\n')
			}
			c.out.addString("```")
			c.block()
		}
	case "code", "kbd", "samp", "tt":
		if c.preDepth == 0 {
			c.pendingSpace = false
			c.out.addString("`")
		}
	case "strong", "b":
		c.pendingSpace = false
		c.out.addString("**")
	case "em", "i":
		c.pendingSpace = false
		c.out.addString("*")
	case "a":
		if len(c.links) == 0 {
			return
		}
		l := c.links[len(c.links)-1]
		c.links = c.links[:len(c.links)-1]
		if l.start > c.out.Len() {
			return
		}
		label := strings.TrimSpace(string(c.out.b[l.start:]))
		href := l.href
		if href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") || label == "" {
			return
		}
		if href == label {
			// Bare link: "<https://x>" reads cleaner than "[x](x)".
			c.out.truncate(l.start)
			c.out.addString("<" + href + ">")
			return
		}
		c.out.truncate(l.start)
		c.out.addString("[" + label + "](" + href + ")")
		c.pendingSpace = false
	case "ul", "ol":
		if len(c.lists) > 0 {
			c.lists = c.lists[:len(c.lists)-1]
		}
		if len(c.lists) == 0 {
			c.block()
		} else {
			c.newline()
		}
	case "li", "tr", "dt", "dd":
		c.newline()
	default:
		if blockElements[name] {
			c.block()
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// StripTags removes all markup from s and collapses whitespace; used for search
// snippets that carry inline highlighting tags.
func StripTags(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '<':
			in = true
		case s[i] == '>' && in:
			in = false
		case !in:
			b.WriteByte(s[i])
		}
	}
	return strings.TrimSpace(wsRun.ReplaceAllString(html.UnescapeString(b.String()), " "))
}
