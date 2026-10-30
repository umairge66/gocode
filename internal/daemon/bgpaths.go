package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/confighome"
)

// -----------------------------------------------------------------------------
// On-disk layout of background sessions.
//
//	<config>/jobs/<short>/state.json      per-session state (jobstate.go)
//	<config>/daemon.log                   daemon log ("[ISO] [component] msg")
//	<config>/daemon/control.key           control-socket auth token
//	<config>/daemon/roster.json           live worker roster
//	<config>/daemon/auth/<short>.tokens.json  one-shot pty/rendezvous tokens
//	<config>/daemon/pty-pids/             Windows: pty-host breadcrumbs
//	/tmp/cc-daemon-<uid>/<disc>/pty/<short>.sock          pty-host socket
//	/tmp/cc-daemon-<uid>/<disc>/pty/<short>.sock.err      pty-host stderr
//	/tmp/cc-daemon-<uid>/<disc>/pty/<short>.sock.late     exec output left
//	                                                      when nobody listened
//	/tmp/cc-daemon-<uid>/<disc>/pty/<short>.sock.exec-exit  exec exit record
//	/tmp/cc-daemon-<uid>/<disc>/spare/<id>.pty.sock|.claim.sock  warm spare
//
// On Windows the sockets are named pipes \\.\pipe\cc-daemon-<nonce>-pty-<short>
// and the .err/.late/.exec-exit breadcrumbs live under <config>/daemon/pty-pids.

// ShortIDPattern matches a background-session short id (8 lowercase hex).
var ShortIDPattern = regexp.MustCompile(`^[a-f0-9]{8}$`)

// DefaultConfigHome resolves the gocode root the way the CLI does
// (internal/confighome): GOCODE_CONFIG_DIR, else $XDG_CONFIG_HOME/gocode, else
// <home>/.config/gocode. A nil getenv consults no environment variables.
func DefaultConfigHome(getenv func(string) string) string {
	return confighome.ResolveDefault(getenv, os.UserHomeDir)
}

// RuntimeDirFor returns /tmp/cc-daemon-<uid>/<discriminator> for configDir.
func RuntimeDirFor(configDir string) string {
	return RuntimeDir(tempRoot(), os.Getuid(), InstanceDiscriminator(configDir))
}

// JobsDir is <config>/jobs.
func JobsDir(configDir string) string { return filepath.Join(configDir, "jobs") }

// JobDir is <config>/jobs/<short>.
func JobDir(configDir, short string) string { return filepath.Join(JobsDir(configDir), short) }

// DaemonLogPath is <config>/daemon.log.
func DaemonLogPath(configDir string) string { return filepath.Join(configDir, "daemon.log") }

// RosterPath is <config>/daemon/roster.json.
func RosterPath(configDir string) string {
	return filepath.Join(daemonConfigDir(configDir), "roster.json")
}

// AuthDir is <config>/daemon/auth.
func AuthDir(configDir string) string { return filepath.Join(daemonConfigDir(configDir), "auth") }

// SocketTokensPath is <config>/daemon/auth/<short>.tokens.json.
func SocketTokensPath(configDir, short string) string {
	return filepath.Join(AuthDir(configDir), short+".tokens.json")
}

// PTYPidsDir is <config>/daemon/pty-pids (Windows breadcrumbs).
func PTYPidsDir(configDir string) string {
	return filepath.Join(daemonConfigDir(configDir), "pty-pids")
}

// PTYDir is the runtime pty socket directory.
func PTYDir(configDir string) string { return filepath.Join(RuntimeDirFor(configDir), "pty") }

// SpareDir is the runtime warm-spare directory.
func SpareDir(configDir string) string { return filepath.Join(RuntimeDirFor(configDir), "spare") }

// PTYEndpoint is the pty-host socket address for a job: a unix socket under
// PTYDir, or a named pipe on Windows.
func PTYEndpoint(configDir, short string) (ControlEndpoint, error) {
	if runtime.GOOS == "windows" {
		nonce, err := ensurePipeNonce(configDir)
		if err != nil {
			return ControlEndpoint{}, err
		}
		return ControlEndpoint{Network: "pipe", Address: `\\.\pipe\cc-daemon-` + nonce + `-pty-` + short}, nil
	}
	return ControlEndpoint{Network: "unix", Address: filepath.Join(PTYDir(configDir), PTYSocketName(short))}, nil
}

// SpareEndpoints returns the pty-host and claim socket paths of a warm spare.
func SpareEndpoints(configDir, id string) (pty, claim string) {
	d := SpareDir(configDir)
	return filepath.Join(d, id+".pty.sock"), filepath.Join(d, id+".claim.sock")
}

// breadcrumb returns the pty-host side file for a socket address: the address
// plus a suffix on unix, or <config>/daemon/pty-pids/<pipe basename>.<suffix>
// on Windows.
func breadcrumb(configDir, sock, suffix string) string {
	if runtime.GOOS == "windows" {
		base := sock
		if i := strings.LastIndex(base, `\`); i >= 0 {
			base = base[i+1:]
		}
		return filepath.Join(PTYPidsDir(configDir), base+"."+suffix)
	}
	return sock + "." + suffix
}

// PTYErrPath is the pty host's stderr breadcrumb.
func PTYErrPath(configDir, sock string) string { return breadcrumb(configDir, sock, "err") }

// PTYLatePath holds exec output nobody was connected to receive.
func PTYLatePath(configDir, sock string) string { return breadcrumb(configDir, sock, "late") }

// PTYExecExitPath records an exec job's exit {code,signal,tail}.
func PTYExecExitPath(configDir, sock string) string {
	return breadcrumb(configDir, sock, "exec-exit")
}
