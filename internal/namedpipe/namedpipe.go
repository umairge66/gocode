// Package namedpipe is a small, dependency-free Windows named-pipe transport
// (net.Listener / net.Conn) for gocode's LOCAL IPC endpoints: the daemon
// control socket (\\.\pipe\cc-daemon-<nonce>-control) and the Chrome
// native-host control socket (\\.\pipe\gocode-chrome-host-<instance>).
//
// It is built on golang.org/x/sys/windows only (no go-winio). This file holds
// the platform-independent pieces (name validation, security-descriptor
// construction, sentinel errors) so they are unit-tested on every GOOS; the
// Win32 transport lives in namedpipe_windows.go.
//
// Security model (see namedpipe_windows.go for the mechanics):
//
//   - Every pipe is created with an explicit, protected DACL granting access to
//     the current user's SID and LocalSystem only (SecurityDescriptor).
//   - PIPE_REJECT_REMOTE_CLIENTS: the pipe is never reachable over SMB.
//   - The server verifies every connecting client's user SID (client pid ->
//     process token -> TokenUser) before Accept returns it, and the client
//     verifies the server's user SID before Dial returns. Both FAIL CLOSED.
//   - Clients open the pipe with SECURITY_SQOS_PRESENT|SECURITY_IDENTIFICATION
//     so a (hypothetical) rogue server can identify but never impersonate them.
package namedpipe

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// LocalPrefix is the only pipe namespace this package accepts: the local
// machine's pipe namespace. A "\\server\pipe\..." name would be a remote (SMB)
// pipe and is rejected outright.
const LocalPrefix = `\\.\pipe\`

// maxNameLen is the documented limit for a full pipe name (including the
// \\.\pipe\ prefix): 256 characters.
const maxNameLen = 256

// ErrPipeInUse reports that a listener could not create the pipe because an
// instance with that name already exists (FILE_FLAG_FIRST_PIPE_INSTANCE failed
// with ERROR_ACCESS_DENIED). For the daemon this means another daemon is
// already serving; for the chrome host it triggers the takeover handshake.
var ErrPipeInUse = errors.New("named pipe already exists (another process holds this endpoint)")

// ErrPeerMismatch reports that the process on the other end of a pipe runs as
// a different Windows user than this process. Both Accept (client check) and
// Dial (server check) refuse such a peer.
var ErrPeerMismatch = errors.New("named pipe peer runs as a different user")

// ValidateName checks that name is a local pipe name this package may serve or
// dial: it must start with \\.\pipe\, have a non-empty remainder without
// further backslashes or forward slashes, and fit the 256-character limit.
func ValidateName(name string) error {
	if !strings.HasPrefix(name, LocalPrefix) {
		return fmt.Errorf("namedpipe: %q is not a local pipe name (want prefix %s)", name, LocalPrefix)
	}
	rest := name[len(LocalPrefix):]
	if rest == "" {
		return fmt.Errorf("namedpipe: %q has an empty pipe name", name)
	}
	if strings.ContainsAny(rest, `\/`) {
		return fmt.Errorf("namedpipe: %q must not contain path separators after the prefix", name)
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("namedpipe: %q exceeds %d characters", name, maxNameLen)
	}
	return nil
}

// sidPattern matches a textual SID such as S-1-5-21-1004336348-1177238915-
// 682003330-512 or S-1-5-18. It exists so SecurityDescriptor can refuse to
// splice anything other than a SID into the SDDL string.
var sidPattern = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)

// SecurityDescriptor builds the SDDL for a pipe that only userSID (and, when
// includeSystem is set, LocalSystem) may open:
//
//	D:P(A;;GA;;;<userSID>)(A;;GA;;;SY)
//
// "D:P" is a PROTECTED DACL (no inherited ACEs), and no other principal —
// not Everyone, not Anonymous, not BUILTIN\Administrators — gets an ACE, so a
// different user cannot open the pipe at all. GA (GENERIC_ALL) includes
// FILE_CREATE_PIPE_INSTANCE, which lets later processes of the SAME user add
// instances (the chrome host's takeover path); another user cannot.
func SecurityDescriptor(userSID string, includeSystem bool) (string, error) {
	if !sidPattern.MatchString(userSID) {
		return "", fmt.Errorf("namedpipe: %q is not a valid SID string", userSID)
	}
	sddl := "D:P(A;;GA;;;" + userSID + ")"
	if includeSystem {
		sddl += "(A;;GA;;;SY)"
	}
	return sddl, nil
}

// Addr is the net.Addr of a named-pipe endpoint: Network "pipe", String the
// full \\.\pipe\... name.
type Addr string

// Network implements net.Addr.
func (Addr) Network() string { return "pipe" }

// String implements net.Addr.
func (a Addr) String() string { return string(a) }
