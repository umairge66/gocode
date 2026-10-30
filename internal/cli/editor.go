package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/ui"
)

// External editor support for the commands that hand a file to the user's
// editor (/memory, /hooks): the editor is $VISUAL, then $EDITOR, then the
// first of a few common editors found on PATH. A terminal editor runs in the
// foreground with the UI suspended; a graphical one is started on its own and
// the session continues at once.

// editorChoice is the resolved external editor.
type editorChoice struct {
	argv []string // program and its leading arguments
	// source names where it came from: "$VISUAL", "$EDITOR" or "default".
	source string
	value  string // the raw variable value (source $VISUAL / $EDITOR)
}

// errNoEditor reports that no editor is configured or installed.
var errNoEditor = errors.New("no editor found")

// defaultEditors are tried, in order, when neither $VISUAL nor $EDITOR is set.
func defaultEditors() []string {
	if runtime.GOOS == "windows" {
		return []string{"code", "notepad"}
	}
	return []string{"code", "vi", "nano"}
}

// guiEditors start their own window; they are not run in the foreground.
var guiEditors = map[string]bool{
	"code": true, "code-insiders": true, "cursor": true, "windsurf": true,
	"subl": true, "zed": true, "gedit": true, "kate": true, "mate": true,
	"notepad": true, "notepad++": true, "gvim": true, "mvim": true,
}

// resolveEditor picks the editor from the environment, or a default found on
// PATH. lookPath is exec.LookPath outside tests.
func resolveEditor(getenv func(string) string, lookPath func(string) (string, error)) (editorChoice, error) {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return editorChoice{argv: strings.Fields(v), source: "$" + name, value: v}, nil
		}
	}
	for _, e := range defaultEditors() {
		if _, err := lookPath(e); err == nil {
			return editorChoice{argv: []string{e}, source: "default"}, nil
		}
	}
	return editorChoice{}, errNoEditor
}

// isGUIEditor reports whether the editor program opens its own window.
func isGUIEditor(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(argv[0]), filepath.Ext(argv[0])))
	return guiEditors[base]
}

// editorHint is the line shown after a file was opened, naming the editor
// variable in use (or how to choose one).
func editorHint(c editorChoice) string {
	if c.source == "$VISUAL" || c.source == "$EDITOR" {
		return "> Using " + c.source + "=\"" + c.value + "\". To change editor, set $EDITOR or $VISUAL environment variable."
	}
	return "> To use a different editor, set the $EDITOR or $VISUAL environment variable."
}

// launchEditor opens path in the resolved editor. A terminal editor runs with
// the UI suspended and this call waits for it to exit; a graphical editor is
// started detached. It returns the editor used.
func (d *interactiveDialogs) launchEditor(path string) (editorChoice, error) {
	c, err := resolveEditor(envGetenv(d.env), exec.LookPath)
	if err != nil {
		return c, err
	}
	argv := append(append([]string(nil), c.argv...), path)
	if isGUIEditor(c.argv) {
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			return c, err
		}
		go func() { _ = cmd.Wait() }()
		return c, nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	done := make(chan error, 1)
	d.sendMsg(ui.ExecProcessMsg{Cmd: cmd, Done: func(err error) { done <- err }})
	return c, <-done
}

// openInEditor opens path through the injected seam (tests) or the real
// editor.
func (d *interactiveDialogs) openInEditor(path string) (editorChoice, error) {
	if d.openEditor != nil {
		return d.openEditor(path)
	}
	return d.launchEditor(path)
}
