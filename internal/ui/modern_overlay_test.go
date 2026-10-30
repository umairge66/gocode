package ui

import (
	"strings"
	"testing"
)

// Behind a modal, block-glyph pixel art fades evenly: both halves of a
// half-block cell are darkened by colour, without the faint attribute.
func TestDimFrameKeepsPixelArtIntact(t *testing.T) {
	line := "\x1b[38;2;124;245;166;48;2;11;61;29m▀\x1b[0m\x1b[38;2;200;200;200mA\x1b[0m"
	out := dimFrame([]string{line}, 2, 1)[0]
	block := out[:strings.Index(out, "▀")]
	if strings.Contains(block, "[2m") || strings.Contains(block, ";2m") || strings.Contains(block, "38;2;124;245;166") {
		t.Fatalf("block cell still faint or undimmed: %q", block)
	}
	if !strings.Contains(block, "38;2;") || !strings.Contains(block, "48;2;") {
		t.Fatalf("block cell lost a colour: %q", block)
	}
}
