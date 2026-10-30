package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

// -----------------------------------------------------------------------------
// Control-socket address / instance discriminator derivation.
//
//	discriminator   first 8 hex chars of sha256(resolve(configDir))
//	runtime dir     <tmp>/cc-daemon-<uid>/<discriminator>
//	                (<tmp> is $PREFIX/tmp under Termux, else "/tmp")
//	control socket  <runtime dir>/control.sock; on Windows the named pipe
//	                \\.\pipe\cc-daemon-<nonce>-control (nonce from pipe.key)
//	<configDir>/daemon/pipe.key     Windows pipe nonce file
//	<configDir>/daemon/control.key  control-key auth token
//
// configDir is the per-user config directory. It is supplied to these helpers
// so the daemon package does not depend on the config subsystem's resolution.
// -----------------------------------------------------------------------------

// InstanceDiscriminator derives the per-install control-socket discriminator:
// the first 8 hex characters of sha256(resolve(configDir)). It is what keeps
// two daemons started from different config directories (or by different users,
// whose config dirs differ) from colliding on one control socket, and it is how
// a client reaches the daemon serving its own config dir — both sides compute
// it from the same configDir.
func InstanceDiscriminator(configDir string) string {
	sum := sha256.Sum256([]byte(resolveDir(configDir)))
	return hex.EncodeToString(sum[:])[:8]
}

// resolveDir makes configDir absolute (against the cwd for a relative input)
// and normalises it. The config
// dir is normally already absolute, in which case this is just Clean.
func resolveDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// tempRoot returns the temp root the runtime directory lives under: the Termux
// prefixed tmp when running under Termux, else the literal "/tmp" (NOT
// os.TempDir / $TMPDIR).
func tempRoot() string {
	if os.Getenv("TERMUX_VERSION") != "" {
		if prefix := os.Getenv("PREFIX"); prefix != "" {
			return filepath.Join(prefix, "tmp")
		}
	}
	return "/tmp"
}

// RuntimeBaseDir returns the temp root the per-user runtime directory tree lives
// under ("/tmp", or $PREFIX/tmp under Termux). It is exported so sibling local-IPC
// features (e.g. internal/chromehost's control socket) can derive a socket path
// inside the SAME per-user runtime-dir layout the daemon uses — RuntimeDir(
// RuntimeBaseDir(), uid, InstanceDiscriminator(configDir)) — rather than
// re-deriving the "/tmp" vs Termux base themselves.
func RuntimeBaseDir() string { return tempRoot() }

// daemonConfigDir returns <configDir>/daemon, where the per-install auth
// material (pipe.key, control.key) lives.
func daemonConfigDir(configDir string) string {
	return filepath.Join(configDir, "daemon")
}

// pipeKeyPath is the file holding the Windows named-pipe nonce.
func pipeKeyPath(configDir string) string {
	return filepath.Join(daemonConfigDir(configDir), "pipe.key")
}

// ControlKeyPath is the file holding the control-socket auth token. The token
// is the secondary handshake layer carried in the request `Auth`
// field (Request.Auth); the primary authorisation is the peer-uid check. It is
// exposed here so the daemon wiring can read/write it, but reading it is left
// to the caller to keep this package free of a mandatory config dependency.
func ControlKeyPath(configDir string) string {
	return filepath.Join(daemonConfigDir(configDir), "control.key")
}

// pipeNoncePattern validates a stored Windows pipe nonce (16 lowercase hex).
var pipeNoncePattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

// ControlPipeName builds the Windows named-pipe control-socket name from the
// per-install nonce. The nonce (not the sha256 discriminator used on unix) is
// the Windows instance discriminator: it is stored in <configDir>/daemon/pipe.key
// so client and server derive the same pipe name.
func ControlPipeName(nonce string) string {
	return `\\.\pipe\cc-daemon-` + nonce + `-control`
}

// ensurePipeNonce reads the Windows pipe nonce from <configDir>/daemon/pipe.key,
// creating a fresh randomBytes(8).hex (16 hex chars) one when it is missing or
// invalid. This is portable filesystem IO so it builds on every GOOS, but it is
// only consulted by the Windows control-address path.
func ensurePipeNonce(configDir string) (string, error) {
	path := pipeKeyPath(configDir)
	if b, err := os.ReadFile(path); err == nil {
		if nonce := string(trimSpace(b)); pipeNoncePattern.MatchString(nonce) {
			return nonce, nil
		}
	}
	nonce, err := randomHex(8)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(daemonConfigDir(configDir), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(nonce), 0o600); err != nil {
		return "", err
	}
	return nonce, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// ControlEndpoint describes where one daemon instance's control socket lives.
// Network is "unix" (a filesystem socket path) or "pipe" (a Windows named-pipe
// name). Both the client dialer and the server listener derive it from
// ControlAddress, so they always agree on the address.
type ControlEndpoint struct {
	Network string
	Address string
}

// ControlAddress derives the control-socket endpoint for the daemon instance
// identified by configDir, for the current user and OS. On unix it is the
// control.sock inside the per-user /tmp/cc-daemon-<uid>/<discriminator>
// runtime directory; on Windows it is the \\.\pipe\cc-daemon-<nonce>-control
// named pipe. This single helper is the shared source of truth for the
// client-address and server-listen sides.
func ControlAddress(configDir string) (ControlEndpoint, error) {
	if runtime.GOOS == "windows" {
		nonce, err := ensurePipeNonce(configDir)
		if err != nil {
			return ControlEndpoint{}, err
		}
		return ControlEndpoint{Network: "pipe", Address: ControlPipeName(nonce)}, nil
	}
	rt := RuntimeDir(tempRoot(), os.Getuid(), InstanceDiscriminator(configDir))
	return ControlEndpoint{Network: "unix", Address: filepath.Join(rt, ControlSocketName)}, nil
}
