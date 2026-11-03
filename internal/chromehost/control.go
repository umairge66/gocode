package chromehost

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/daemon"
)

// -----------------------------------------------------------------------------
// Chrome-host CONTROL SOCKET (milestone 2).
//
// The native-messaging host (gocode launched by Chrome, see internal/cli's
// runChromeHost) speaks the native-messaging stdio protocol to the MV3 extension
// on stdin/stdout. That host process is SEPARATE from the live gocode session
// where the model runs, so the session cannot reach the extension directly. This
// control socket bridges the gap with LOCAL IPC only:
//
//	session (client)  --unix socket-->  host (server)  --stdio-->  extension
//
// A session dials the per-user control socket, sends one {method, params}
// request, the host relays it to the extension via Host.Send and returns the
// extension's reply over the socket. There is NO network, NO account/OAuth, NO
// cloud bridge and NO telemetry: every byte stays on the local machine, and the
// socket is same-uid-only (the daemon peer-cred authorizer is reused).
//
// The socket lives in the SAME per-user runtime-dir layout the daemon control
// socket uses (/tmp/cc-daemon-<uid>/<instance>/), under a DISTINCT basename
// (chrome-host.sock) so it never collides with control.sock. stdout stays the
// native-messaging protocol channel — the socket is a separate fd and all
// diagnostics go to stderr.
// -----------------------------------------------------------------------------

// ChromeHostSocketName is the control socket's basename inside the per-user
// runtime directory. It is deliberately distinct from daemon.ControlSocketName
// ("control.sock") so the chrome-host socket and the daemon control socket never
// collide in the same runtime dir.
const ChromeHostSocketName = "chrome-host.sock"

// ErrChromeNotConnected is returned by the session-side client when no host is
// listening on the control socket — Chrome is not running the native host, the
// extension is not loaded, or it has not connected yet. Its message is written
// for the end user: a browser tool surfaces it verbatim as a non-fatal tool
// result telling them how to connect.
var ErrChromeNotConnected = errors.New(
	"Chrome is not connected — load the gocode browser extension and ensure it shows the green badge")

// ControlEndpoint describes where the chrome-host control socket lives. Network
// is "unix" (a filesystem socket path) on linux/darwin, or "pipe" (a Windows
// named-pipe name) on Windows. Both the server listener and the client dialer
// derive it from ControlSocketEndpoint, so they always agree.
type ControlEndpoint struct {
	Network string
	Address string
}

// ControlSocketEndpoint derives the chrome-host control-socket endpoint for the
// given per-user config dir, reusing the daemon's runtime-dir layout so the
// socket sits beside the daemon control socket under a distinct basename. On
// Windows it returns a user-only named-pipe endpoint.
//
// Both the host (server) and the session (client) call this with the SAME
// configDir (see DefaultConfigDir), so they resolve to the same address.
func ControlSocketEndpoint(configDir string) (ControlEndpoint, error) {
	if runtime.GOOS == "windows" {
		// A distinct, per-install pipe name derived from the same instance
		// discriminator the daemon uses (see ControlPipeName and
		// control_windows.go for the takeover/security design).
		return ControlEndpoint{Network: "pipe", Address: ControlPipeName(daemon.InstanceDiscriminator(configDir))}, nil
	}
	rt := daemon.RuntimeDir(daemon.RuntimeBaseDir(), os.Getuid(), daemon.InstanceDiscriminator(configDir))
	return ControlEndpoint{Network: "unix", Address: filepath.Join(rt, ChromeHostSocketName)}, nil
}

// DefaultConfigDir resolves the gocode root the control-socket address is
// derived from (internal/confighome): GOCODE_CONFIG_DIR, else
// $XDG_CONFIG_HOME/gocode, else <home>/.config/gocode. The daemon resolves the
// same root, so the chrome-host socket lands in the same runtime-dir family,
// and the host (launched by Chrome, which may not carry GOCODE_CONFIG_DIR but
// does carry HOME) and the session resolve the same path.
//
// getenv and homeDir are injected so the resolution is testable; production
// passes os.Getenv and os.UserHomeDir.
func DefaultConfigDir(getenv func(string) string, homeDir func() (string, error)) string {
	return confighome.ResolveDefault(getenv, homeDir)
}

// ControlPipeName is the Windows named-pipe name of the chrome-host control
// socket for the given instance discriminator.
func ControlPipeName(instance string) string {
	return `\\.\pipe\gocode-chrome-host-` + instance
}

// hostTakeoverMethod is the reserved control request a newly started host
// sends to the host currently serving the Windows control pipe, asking it to
// stop accepting new connections so the newest host owns the endpoint. A unix
// socket path can simply be unlinked and re-bound; a named pipe cannot be
// "removed" from under its owner, so the handover is explicit. It is only
// honoured by listeners that support ceding (the Windows pipe listener); on
// unix it is relayed like any other method, exactly as before.
const hostTakeoverMethod = "gocode.host/takeover"

// ceder is implemented by a control listener that can hand its endpoint over
// to a newer host: Cede stops accepting new clients (in-flight connections
// keep running) without making Accept fail, so the serve loop stays up and
// can re-bind if the newer host goes away.
type ceder interface {
	Cede()
}

// controlFrameDelim terminates a control-socket frame. Requests and replies are
// newline-delimited JSON (one frame per line), mirroring the daemon control
// socket's line framing. The host->extension native-messaging codec (ReadMessage
// /WriteMessage) is a separate, length-prefixed binary protocol and is NOT used
// here.
const controlFrameDelim = '\n'

// maxControlFrame bounds a single control-socket frame. A screenshot reply
// carries a base64 PNG data URL, so this is generous (it must comfortably exceed
// the native-messaging MaxReadSize payloads the host can relay).
const maxControlFrame = MaxReadSize + (1 << 20)

// ControlRequest is one session->host command: the browser method and its
// params, relayed verbatim to the extension via Host.Send.
type ControlRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ControlResponse is the host->session reply. Exactly one of Result or Error is
// meaningful: Error is a non-empty string when the extension (or the relay)
// failed, Result is the extension's raw result payload otherwise.
type ControlResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// writeControlFrame writes v as one newline-delimited JSON frame.
func writeControlFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("chromehost: marshal control frame: %w", err)
	}
	payload = append(payload, controlFrameDelim)
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("chromehost: write control frame: %w", err)
	}
	return nil
}

// readControlFrame reads one newline-delimited JSON frame from br, rejecting a
// frame larger than maxControlFrame. It stops reading as soon as the frame is
// too large, so a peer that never sends a newline cannot make it buffer an
// unbounded line.
func readControlFrame(br *bufio.Reader) ([]byte, error) {
	return readFrameLimit(br, maxControlFrame)
}

func readFrameLimit(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice(controlFrameDelim)
		size := len(line) + len(chunk)
		if err == nil {
			size-- // the delimiter is not part of the frame
		}
		if size > limit {
			return nil, fmt.Errorf("chromehost: control frame exceeds %d bytes", limit)
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err == io.EOF && len(line) > 0:
			// A final frame without a trailing newline is still usable.
			return line, nil
		default:
			return nil, err
		}
	}
}
