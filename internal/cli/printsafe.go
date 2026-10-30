package cli

import (
	"io"
	"os"

	"github.com/umairge66/gocode/internal/ui"
)

// terminalSafeWriter strips terminal control sequences (OSC 52 clipboard
// writes, title sets, cursor moves, terminal queries, C1 controls, bidi
// overrides …) from text written to a terminal. The `-p` answer and its
// stderr notices carry model output, tool output and server error bodies; when
// they go to a TTY that text must not be able to drive the terminal. Piped
// output (a file, jq, another program) is left byte-exact.
type terminalSafeWriter struct{ w io.Writer }

func (t terminalSafeWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(t.w, ui.SanitizeText(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// terminalSafeEnv returns env with Stdout/Stderr wrapped in a
// terminalSafeWriter when they are terminals.
func terminalSafeEnv(env *Env) *Env {
	if env == nil {
		return env
	}
	e := *env
	if env.StdoutTTY && env.Stdout != nil {
		e.Stdout = terminalSafeWriter{env.Stdout}
	}
	if f, ok := env.Stderr.(*os.File); ok && isTerminal(f) {
		e.Stderr = terminalSafeWriter{env.Stderr}
	}
	return &e
}
