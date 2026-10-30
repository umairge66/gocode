package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/umairge66/gocode/internal/daemon"
)

// -----------------------------------------------------------------------------
// `gocode attach <id|name>`: open a background session in this terminal.
//
// The client sends the attach op, then relays raw bytes both ways with the
// terminal in raw mode. Ctrl+Z, Ctrl+B d (and the kitty-keyboard encodings of
// Ctrl+Z) detach; the session keeps running. Terminal resizes are sent as
// resize ops tagged with this attacher's id.

// terminalSize returns the stdout terminal size.
func terminalSize() (int, int, bool) {
	for _, f := range []*os.File{os.Stdout, os.Stderr, os.Stdin} {
		if w, h, err := term.GetSize(int(f.Fd())); err == nil && w > 0 && h > 0 {
			return w, h, true
		}
	}
	return 0, 0, false
}

type attachOutcome struct {
	kind string // "detached" | "disconnected" | "error"
	msg  string
}

// Detach keys: the kitty-protocol legacy and CSI-u encodings of the detach
// and prefix chords.
var (
	detachKittyLegacy = []byte("\x1b[27;5;122~")
	detachKittyCSIu   = []byte("\x1b[122;5u")
	prefixKittyLegacy = []byte("\x1b[27;5;98~")
	prefixKittyCSIu   = []byte("\x1b[98;5u")
)

// detachScanner finds detach keys in typed input and returns the bytes to
// forward.
type detachScanner struct{ prefixArmed bool }

// scan returns (forward, detached).
func (s *detachScanner) scan(in []byte) ([]byte, bool) {
	var out []byte
	for i := 0; i < len(in); i++ {
		b := in[i]
		if s.prefixArmed {
			s.prefixArmed = false
			if b == 'd' {
				return out, true
			}
			out = append(out, 2, b)
			continue
		}
		if b == 26 || bytes.HasPrefix(in[i:], detachKittyLegacy) || bytes.HasPrefix(in[i:], detachKittyCSIu) {
			return out, true
		}
		if b == 2 {
			s.prefixArmed = true
			continue
		}
		if bytes.HasPrefix(in[i:], prefixKittyLegacy) {
			s.prefixArmed = true
			i += len(prefixKittyLegacy) - 1
			continue
		}
		if bytes.HasPrefix(in[i:], prefixKittyCSIu) {
			s.prefixArmed = true
			i += len(prefixKittyCSIu) - 1
			continue
		}
		out = append(out, b)
	}
	return out, false
}

// attachClient runs one attach session.
func attachClient(env *Env, configDir, short string) attachOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := daemon.DialControl(ctx, configDir)
	cancel()
	if err != nil {
		return attachOutcome{"error", redactPipeNonce(err.Error())}
	}
	defer conn.Close()
	cols, rows, ok := terminalSize()
	if !ok {
		cols, rows = 120, 30
	}
	attachID := newUUID()
	req := daemon.Request{Proto: daemon.ProtoVersion, Op: daemon.OpAttach, Short: short,
		Auth: daemon.ReadControlKey(configDir), Cols: cols, Rows: rows, AttachID: attachID,
		Caps: json.RawMessage(`{"imark":false}`)}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := daemon.WriteFrame(conn, req); err != nil {
		return attachOutcome{"error", err.Error()}
	}
	br := bufio.NewReader(conn)
	line, err := daemon.ReadFrame(br)
	if err != nil {
		if isTimeoutErr(err) {
			return attachOutcome{"error", serviceName + " did not respond — it may be stalled"}
		}
		return attachOutcome{"error", "control socket closed"}
	}
	_ = conn.SetDeadline(time.Time{})
	var ack struct {
		OK    bool   `json:"ok"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &ack) != nil {
		return attachOutcome{"error", "bad ack: non-object ack line"}
	}
	if !ack.OK {
		return attachOutcome{"error", ack.Code + ": " + ack.Error}
	}

	stdinFile, _ := env.Stdin.(*os.File)
	var restore func()
	if env.StdinTTY && stdinFile != nil {
		if st, err := term.MakeRaw(int(stdinFile.Fd())); err == nil {
			restore = func() { _ = term.Restore(int(stdinFile.Fd()), st) }
		}
	}
	if env.StdoutTTY {
		fmt.Fprint(env.Stdout, "\x1b[?1049h\x1b[H\x1b[2J\r\n  \x1b[2mAttaching…\x1b[0m\r\n")
	}

	var once sync.Once
	result := make(chan attachOutcome, 3)
	finish := func(o attachOutcome) { once.Do(func() { result <- o }) }

	// daemon -> terminal
	go func() {
		_, err := io.Copy(env.Stdout, br)
		_ = err
		finish(attachOutcome{kind: "disconnected"})
	}()
	// terminal -> daemon
	if env.Stdin != nil {
		go func() {
			sc := &detachScanner{}
			buf := make([]byte, 4096)
			for {
				n, err := env.Stdin.Read(buf)
				if n > 0 {
					fwd, detached := sc.scan(buf[:n])
					if len(fwd) > 0 {
						if _, werr := conn.Write(fwd); werr != nil {
							finish(attachOutcome{kind: "disconnected"})
							return
						}
					}
					if detached {
						finish(attachOutcome{kind: "detached"})
						return
					}
				}
				if err != nil {
					finish(attachOutcome{kind: "detached"})
					return
				}
			}
		}()
	}
	stopResize := watchResize(func() {
		c, r, ok := terminalSize()
		if !ok {
			return
		}
		daemon.NewControlClient(configDir).Request(context.Background(), daemon.Request{
			Proto: daemon.ProtoVersion, Op: daemon.OpResize, Short: short, Cols: c, Rows: r, AttachID: attachID})
	})
	out := <-result
	stopResize()
	_ = conn.Close()
	if restore != nil {
		restore()
	}
	if env.StdoutTTY {
		fmt.Fprint(env.Stdout, "\x1b[0m\x1b[?25h\x1b[?1049l")
	}
	return out
}

// bgAttach is the `attach` verb.
func bgAttach(env *Env, configDir, arg string, rest []string) int {
	r := resolveJob(env, configDir, arg, rest, "gocode attach <id|name>",
		"Open the background session in this terminal. ← returns to agent view, Ctrl+Z drops back to your shell. The session keeps running either way. "+bgNameHelp, true)
	if r.code != 0 {
		return max(r.code, 0)
	}
	short := r.short
	if ok, reason := ensureDaemon(env, configDir); !ok {
		fmt.Fprintf(env.Stderr, "Couldn't attach — %s is unavailable (%s)\n", serviceName, reason)
		return 1
	}
	if !daemonHasJob(configDir, short) {
		st, _ := daemon.ReadJobState(daemon.JobDir(configDir, short))
		switch {
		case st != nil && st.State == "failed" && !st.IsExecOnly():
			detail := st.Detail
			if detail == "" {
				detail = "it crashed repeatedly"
			}
			fmt.Fprintf(env.Stderr, "Session %s can't start — %s\n", short, detail)
			return 1
		case r.byName:
			fmt.Fprintf(env.Stderr, "The %s has no process for session %s.\nA name opens only a running session. Use the id: `gocode attach %s`\n", serviceName, short, short)
			return 1
		}
		fmt.Fprintf(env.Stderr, "Waking session %s…\n", short)
		ok, alive, errText := wakeJob(env, configDir, short, false)
		if !ok && !alive {
			fmt.Fprintf(env.Stderr, "Couldn't wake %s — %s\n", short, errText)
			return 1
		}
	}
	for reconnects := 0; ; reconnects++ {
		out := attachClient(env, configDir, short)
		switch out.kind {
		case "detached":
			if out.msg != "" {
				fmt.Fprintln(env.Stderr, out.msg)
			}
			return 0
		case "disconnected":
			if reconnects < 3 && daemonHasJob(configDir, short) {
				fmt.Fprintf(env.Stderr, "Reconnecting to %s…\n", short)
				continue
			}
			st, _ := daemon.ReadJobState(daemon.JobDir(configDir, short))
			extra := ""
			if st != nil && st.State == "failed" && st.Detail != "" {
				extra = " (" + st.Detail + ")"
			}
			fmt.Fprintf(env.Stderr, "Session %s has exited%s.\n", short, extra)
			return 0
		default:
			msg := out.msg
			if strings.Contains(msg, "ENOJOB") {
				msg = strings.TrimPrefix(msg, "ENOJOB: ")
			}
			fmt.Fprintf(env.Stderr, "Couldn't attach to %s — %s\n", short, msg)
			return 1
		}
	}
}
