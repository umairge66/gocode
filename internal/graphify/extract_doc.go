package graphify

import (
	"path"
	"regexp"
	"strings"
)

var (
	headingRe = regexp.MustCompile(`^(#{1,3})\s+(.+?)\s*#*\s*$`)
	linkRe    = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)\s>#]+)`)
	codeRe    = regexp.MustCompile("`([^`\n]+)`")
	slugRe    = regexp.MustCompile(`[^a-z0-9]+`)
	mentionRe = regexp.MustCompile(`^[A-Za-z_][\w.]*[A-Za-z0-9_]$`)
)

// extractDocument reads a text document. Markdown yields a node per heading
// (levels 1-3), "references" for links to other files of the tree and
// "mentions" for code spans that name a definition; other text formats yield
// the file node only.
func extractDocument(rel string, src []byte) *FileExtraction {
	fe := newFileExtraction(rel, "text", "", "document")
	text := string(src)
	fe.Words = len(strings.Fields(text))
	switch strings.ToLower(path.Ext(rel)) {
	case ".md", ".markdown", ".mdx":
		fe.Lang = "markdown"
	default:
		return fe
	}

	dir := path.Dir(rel)
	parents := [4]string{rel} // parents[level] is the open heading of that level
	current := rel
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		lineNo := i + 1
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			title := strings.TrimSpace(m[2])
			slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
			if slug == "" {
				continue
			}
			id := rel + "#" + slug
			if fe.seenID[id] {
				id += "@" + loc(lineNo)
			}
			fe.seenID[id] = true
			parent := rel
			for l := level - 1; l >= 1; l-- {
				if parents[l] != "" {
					parent = parents[l]
					break
				}
			}
			fe.Nodes = append(fe.Nodes, Node{ID: id, Label: title, FileType: "document", SourceFile: rel, SourceLocation: loc(lineNo), Kind: KindSection})
			fe.Edges = append(fe.Edges, Edge{Source: parent, Target: id, Relation: "contains", Confidence: Extracted, SourceFile: rel, SourceLocation: loc(lineNo)})
			parents[level] = id
			for l := level + 1; l < len(parents); l++ {
				parents[l] = ""
			}
			current = id
			continue
		}
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if strings.HasPrefix(target, "/") {
				target = strings.TrimPrefix(target, "/")
			} else {
				target = path.Join(dir, target)
			}
			fe.addRef(Ref{From: current, Name: path.Clean(target), Relation: "references", Line: lineNo})
		}
		for _, m := range codeRe.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSuffix(strings.TrimSpace(m[1]), "()")
			if len(name) >= 4 && mentionRe.MatchString(name) {
				fe.addRef(Ref{From: current, Name: name, Relation: "mentions", Line: lineNo})
			} else if strings.ContainsAny(name, "/.") && !strings.ContainsAny(name, " \t") {
				fe.addRef(Ref{From: current, Name: path.Clean(strings.TrimPrefix(name, "./")), Relation: "references", Line: lineNo})
			}
		}
	}
	return fe
}
