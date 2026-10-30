package statusline

// Footer layout and dimmed-color rendering.
//
// The custom status line is laid out by the REPL footer, not by the runner. The
// footer renders the command's mapped stdout (see RenderLine) as a stack of
// terminal text lines, each dimmed and truncated at the end. The whole stack
// sits in a box whose horizontal padding comes
// from the `padding` setting (gap 2 between children), inside the REPL footer's
// own two-column gutter (zero in compact mode). A multi-line text becomes a
// column of lines, each carrying the previous lines' SGR/OSC-8 state forward.

// PaddingValue returns the horizontal padding gutter for the status-line box,
// defaulting to zero when unset. A negative value is treated as zero.
func (c *Config) PaddingValue() int {
	if c == nil || c.Padding == nil || *c.Padding < 0 {
		return 0
	}
	return *c.Padding
}
