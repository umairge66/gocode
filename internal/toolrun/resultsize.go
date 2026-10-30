package toolrun

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/lsp"
	"github.com/umairge66/gocode/internal/protocol"
)

// Oversized tool results.
//
// Every tool has an inline size limit (in characters). A text result longer
// than its tool's limit is not sent to the model in full: the complete text is
// saved to <ToolResultsDir>/<tool_use_id>.txt and the model receives a
// <persisted-output> block with the size, the saved path and a preview of the
// first ~2KB (cut at a line break when one falls in the second half of the
// preview). The model can Read or Grep the saved file for the rest.
//
// When the result cannot be saved (no directory, or the write failed) the
// result is cut to fit the limit instead, inside a <truncated-output> block
// that says the rest was dropped.
//
// Limits: Bash 30000, Grep 20000, Read unlimited (it bounds itself by lines,
// bytes and tokens), every other tool 50000 — the global ceiling no per-tool
// limit may exceed. Results that carry images are never rewritten.

const (
	// MaxResultSizeChars is the ceiling on any tool's inline result size.
	MaxResultSizeChars = 50000
	// BashMaxResultSizeChars is the Bash tool's inline limit.
	BashMaxResultSizeChars = 30000
	// GrepMaxResultSizeChars is the Grep tool's inline limit.
	GrepMaxResultSizeChars = 20000
	// ResultPreviewChars is the size of the preview kept inline for a
	// persisted result.
	ResultPreviewChars = 2000

	persistedOutputOpen  = "<persisted-output>"
	persistedOutputClose = "</persisted-output>"
	truncatedOutputOpen  = "<truncated-output>"
	truncatedOutputClose = "</truncated-output>"

	// truncatedSizePlaceholder sizes the truncated-output frame before the
	// kept length is known (the widest "N bytes" rendering).
	truncatedSizePlaceholder = "1023 bytes"
)

// toolResultLimits are the per-tool inline limits that differ from
// MaxResultSizeChars. A value of 0 means unlimited.
var toolResultLimits = map[protocol.Tool]int{
	protocol.ToolBash: BashMaxResultSizeChars,
	protocol.ToolGrep: GrepMaxResultSizeChars,
	protocol.ToolRead: 0,
	protocol.ToolLSP:  lsp.MaxResultSizeChars,
}

// WithToolResultsDir sets the directory oversized tool results are saved to.
// The directory is created on first use. Reads (Read/Grep/Glob) inside it are
// allowed like reads inside the working directory, so the model can open a
// saved result.
func WithToolResultsDir(dir string) Option {
	return func(d *Dispatcher) {
		d.ToolResultsDir = dir
		if dir != "" && d.Decider != nil {
			d.Decider.ReadableDirs = append(d.Decider.ReadableDirs, dir)
		}
	}
}

// resultLimit is the inline size limit for a tool's results (0 = unlimited).
func resultLimit(tool string) int {
	if n, ok := toolResultLimits[protocol.Tool(tool)]; ok {
		if n <= 0 {
			return 0
		}
		return min(n, MaxResultSizeChars)
	}
	return MaxResultSizeChars
}

// capResult applies the inline size limit of call's tool to res.
func (d *Dispatcher) capResult(call agent.ToolUse, res agent.ToolResult) agent.ToolResult {
	limit := resultLimit(call.Name)
	if call.Name == string(protocol.ToolBash) && len(res.Images) == 0 {
		res.Content, _ = capShellText(d.ToolResultsDir, call.ID, res.Content, limit)
		return res
	}
	if limit == 0 || len(res.Images) > 0 || utf8.RuneCountInString(res.Content) <= limit {
		return res
	}
	if path, err := persistToolResult(d.ToolResultsDir, call.ID, res.Content); err == nil {
		res.Content = persistedOutputMessage(path, res.Content)
		return res
	}
	res.Content = truncatedOutputMessage(res.Content, limit)
	return res
}

// persistToolResult writes content to <dir>/<escaped id>.txt, creating the
// directory. A file already there under that name (the same result saved
// before) is reused when it is a plain regular file.
func persistToolResult(dir, toolUseID, content string) (string, error) {
	if dir == "" {
		return "", errors.New("tool result was not saved")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, toolResultFileName(toolUseID))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		info, lerr := os.Lstat(path)
		if lerr != nil {
			return "", errors.New("tool result path could not be checked; not persisted")
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", errors.New("tool result path is a link; not persisted")
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("tool result path is not a regular file; not persisted")
		}
		return path, nil
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// toolResultFileName escapes a tool_use id into a file name: [A-Za-z0-9_-]
// and non-leading dots are kept, every other byte becomes %XX.
func toolResultFileName(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		keep := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' && b.Len() > 0
		if keep {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	if b.Len() == 0 {
		b.WriteByte('%')
	}
	return b.String() + ".txt"
}

// resultPreview returns the first ~n characters of s, cut at the last line
// break when it falls past the middle of the window, and whether s continues.
func resultPreview(s string, n int) (string, bool) {
	rs := []rune(s)
	if len(rs) <= n {
		return s, false
	}
	head := string(rs[:n])
	if i := strings.LastIndexByte(head, '\n'); i >= 0 && 2*utf8.RuneCountInString(head[:i]) > n {
		return head[:i], true
	}
	return head, true
}

// persistedOutputMessage is the inline replacement for a saved result.
func persistedOutputMessage(path, content string) string {
	preview, more := resultPreview(content, ResultPreviewChars)
	var b strings.Builder
	b.WriteString(persistedOutputOpen + "\n")
	fmt.Fprintf(&b, "Output too large (%s). Full output saved to: %s\n\n", formatSize(utf8.RuneCountInString(content)), path)
	fmt.Fprintf(&b, "Preview (first %s):\n", formatSize(ResultPreviewChars))
	b.WriteString(preview)
	if more {
		b.WriteString("\n...\n")
	} else {
		b.WriteString("\n")
	}
	b.WriteString(persistedOutputClose)
	return b.String()
}

// truncatedOutputMessage cuts content so the whole message fits in limit
// characters, for a result that could not be saved.
func truncatedOutputMessage(content string, limit int) string {
	total := formatSize(utf8.RuneCountInString(content))
	room := max(0, limit-utf8.RuneCountInString(truncatedOutputFrame(total, truncatedSizePlaceholder, "")))
	kept, _ := resultPreview(content, room)
	return truncatedOutputFrame(total, formatSize(utf8.RuneCountInString(kept)), kept)
}

func truncatedOutputFrame(total, shown, body string) string {
	return truncatedOutputOpen + "\nOutput too large (" + total + "). It could not be saved, so only the first " + shown +
		" are shown; the rest was dropped. If the tool can page or filter its results, call it again for the part you need.\n\n" +
		body + "\n" + truncatedOutputClose
}

// formatSize renders a size the way result notices show it: "512 bytes",
// "1.5KB", "2KB", "3.2MB".
func formatSize(n int) string {
	kb := float64(n) / 1024
	if kb < 1 {
		return fmt.Sprintf("%d bytes", n)
	}
	if kb < 1024 {
		return trimPointZero(kb) + "KB"
	}
	mb := kb / 1024
	if mb < 1024 {
		return trimPointZero(mb) + "MB"
	}
	return trimPointZero(mb/1024) + "GB"
}

func trimPointZero(v float64) string {
	s := fmt.Sprintf("%.1f", math.Round(v*10)/10)
	return strings.TrimSuffix(s, ".0")
}
