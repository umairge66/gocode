package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/ui"
)

// askWorkspaceTrust shows the trust dialog before an interactive session is
// built — before any SessionStart hook, MCP server, status line, permission,
// mode or sandbox setting of the folder takes effect — when the folder carries
// something that needs it and is not trusted yet. "Yes" remembers the folder
// (the home directory and above: this session only); "No", esc or quitting
// leaves it untrusted, so its risky settings stay ignored and buildSession
// shows the one-line notice. A background session never prompts.
func askWorkspaceTrust(env *Env, newProgram func(*ui.Model, *Env) teaProgram) {
	if envGetenv(env)("GOCODE_CODE_SESSION_KIND") == "bg" {
		return
	}
	rep := assessWorkspaceTrust(env)
	if !rep.needsPrompt() {
		return
	}
	m := ui.NewModel(80)
	if env != nil && !env.StdoutTTY {
		m.SetRenderer(ui.PlainRenderer{})
	} else {
		m.SetFullScreen(true)
		m.SetNoColor(noColorEnv(env))
	}
	prog := newProgram(m, env)
	answer := make(chan string, 1)
	choose := func(v string) {
		select {
		case answer <- v:
		default:
		}
		go prog.Quit()
	}
	m.OnDialogSubmit = func(values []string) { choose(firstValue(values)) }
	m.OnDialogCancel = func() { choose(trustChoiceNo) }
	dlg := &ui.Dialog{
		Kind:  ui.DialogPermission,
		Title: "Accessing workspace: " + rep.cwd,
		Body:  rep.dialogBody(),
		Options: []ui.DialogOption{
			{Label: "Yes, trust this folder", Value: trustChoiceYes},
			{Label: "No, ignore its settings", Value: trustChoiceNo},
		},
		// The safe answer is highlighted.
		Selected: 1,
	}
	go prog.Send(ui.OpenDialogMsg{Dialog: dlg})
	_, _ = prog.Run()
	var v string
	select {
	case v = <-answer:
	default:
	}
	if v == trustChoiceYes {
		acceptWorkspaceTrust(env, rep)
	}
}

// acceptWorkspaceTrust applies a "Yes": remembered in the user's global
// config, or for this session only for the home directory and above.
func acceptWorkspaceTrust(env *Env, rep trustReport) {
	if rep.sessionOnly {
		env.trustSession = true
		return
	}
	if _, err := setFolderTrust(env, rep.cwd, true); err != nil {
		fmt.Fprintf(env.Stderr, "gocode: could not remember the trust decision (%v); trusting this folder for this session only.\n", err)
		env.trustSession = true
	}
}

// runTrustCmd is `gocode trust [path]` / `gocode trust --revoke [path]`: the
// non-interactive way to trust a folder (for -p, mcp serve and background
// sessions, which never prompt) or to take trust back.
func runTrustCmd(env *Env, t []string) int {
	o, err := scanOpts(argsAfter(t, "trust"), map[string]string{"--revoke": "revoke"}, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	if len(o.unknown) > 0 {
		fmt.Fprintf(env.Stderr, "error: unknown option '%s'\n", o.unknown[0])
		return 1
	}
	if len(o.pos) > 1 {
		fmt.Fprintf(env.Stderr, "error: too many arguments for 'trust'. Expected 1 argument but got %d.\n", len(o.pos))
		return 1
	}
	cwd := sessionCwd(env)
	dir := cwd
	if len(o.pos) == 1 {
		dir = o.pos[0]
		if strings.HasPrefix(dir, "~"+string(filepath.Separator)) || dir == "~" {
			dir = filepath.Join(trustHome(env), strings.TrimPrefix(dir, "~"))
		}
		if !filepath.IsAbs(dir) && cwd != "" {
			dir = filepath.Join(cwd, dir)
		}
	}
	if dir == "" {
		fmt.Fprintln(env.Stderr, "gocode trust: no folder given and the current directory is unavailable.")
		return 1
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(env.Stderr, "gocode trust: %s is not a directory.\n", dir)
		return 1
	}
	revoke := o.has("revoke")
	key, err := setFolderTrust(env, dir, !revoke)
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode trust: %v\n", err)
		return 1
	}
	cfg := gocodeGlobalConfigPath(env)
	home := canonicalDir(trustHome(env))
	if revoke {
		fmt.Fprintf(env.Stdout, "Revoked trust for %s (in %s).\n", key, cfg)
		if folderTrustedInStore(env, dir) {
			fmt.Fprintf(env.Stdout, "Note: %s is still trusted through an enclosing folder; revoke that folder to stop applying its settings.\n", dir)
		}
		return 0
	}
	fmt.Fprintf(env.Stdout, "Trusted %s (saved in %s). Its project settings apply from the next session.\n", key, cfg)
	if isHomeOrAncestor(key, home) {
		fmt.Fprintln(env.Stdout, "This is your home directory or above it: the trust covers this folder itself, not the folders inside it.")
	}
	// Show what the folder applies now that it is trusted.
	view := *env
	view.Cwd = func() (string, error) { return dir, nil }
	view.trustCheck = func(string) bool { return false }
	if rep := assessWorkspaceTrust(&view); len(rep.items) > 0 {
		fmt.Fprintln(env.Stdout, "It applies:")
		for _, it := range rep.items {
			fmt.Fprintln(env.Stdout, "  • "+it)
		}
	}
	return 0
}
