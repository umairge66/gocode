package sandbox

import (
	"strings"
)

// NetworkPolicy describes the network confinement for a Profile.
type NetworkPolicy struct {
	// Restricted turns network confinement on. When false the macOS profile
	// emits "(allow network*)" and the Linux argv omits "--unshare-net"
	// (the command keeps full host network access).
	Restricted bool
	// AllowLocalBinding permits binding/inbound on local addresses and outbound
	// to localhost even while Restricted.
	AllowLocalBinding bool
	// HTTPProxyPort, when non-zero, is a localhost port the sandboxed command may
	// reach (the host HTTP egress bridge).
	HTTPProxyPort int
	// SOCKSProxyPort, when non-zero and distinct from HTTPProxyPort, is a second
	// reachable localhost port (the SOCKS bridge).
	SOCKSProxyPort int
	// Bridge is the Linux network bridge (the host proxy's Unix sockets bound
	// into bubblewrap and bridged to localhost ports inside by socat). Nil on
	// macOS, where the profile opens the localhost proxy ports instead.
	Bridge *LinuxBridge
}

// Profile is the platform-neutral sandbox policy model. It is produced by
// FromRuleSets (or built directly) and then handed to a platform renderer
// (Seatbelt* on macOS, Bubblewrap* on Linux).
//
// The read policy is allow-by-default (every path is readable unless denied);
// the write policy is deny-by-default (no path is writable unless allowed).
// This matches the readConfig (an allowAllExcept/denyOnly shape) and
// writeConfig (a denyAllExcept/allowOnly shape).
type Profile struct {
	// ReadDeny are paths/subtrees made unreadable (file-read* denied). A
	// trailing wildcard or metacharacter marks a glob; otherwise the entry is a
	// directory subtree.
	ReadDeny []string
	// ReadAllow re-permits specific paths that fall inside a ReadDeny subtree
	// (read carve-outs).
	ReadAllow []string

	// WriteAllow are the roots the command may write to; nothing else is
	// writable. An empty WriteAllow together with WriteUnrestricted == false
	// means no writes are permitted beyond the always-on essentials.
	WriteAllow []string
	// WriteDeny are deny carve-outs scanned inside the WriteAllow roots, so a
	// sensitive subpath of a writable tree stays read-only.
	WriteDeny []string
	// WriteUnrestricted, when true, allows all writes (no writeConfig was set):
	// the macOS profile emits "(allow file-write*)".
	WriteUnrestricted bool

	// Network is the network confinement.
	Network NetworkPolicy

	// AllowUnixSockets lists directories whose Unix-domain sockets may be used
	// (bind + connect). Ignored when AllowAllUnixSockets is set.
	AllowUnixSockets []string
	// AllowAllUnixSockets permits every Unix-domain socket.
	AllowAllUnixSockets bool

	// AllowMachLookup lists extra Mach/XPC global-names the command may look up
	// beyond the built-in set (a trailing "*" is a global-name-prefix). macOS
	// only.
	AllowMachLookup []string

	// AllowPty permits pseudo-terminal access (/dev/ptmx, /dev/ttys*).
	AllowPty bool
	// EnableWeakerNetworkIsolation allows trustd.agent (Go TLS cert verification).
	// macOS only.
	EnableWeakerNetworkIsolation bool
	// AllowAppleEvents permits Apple Events / Launch Services open. macOS only.
	AllowAppleEvents bool
	// AllowGitConfig relaxes the built-in git config/hooks write protection.
	AllowGitConfig bool

	// UnsetEnv are environment variables to drop before exec (env -u NAME on
	// macOS, --unsetenv NAME on Linux).
	UnsetEnv []string
	// SetEnv are environment variables to force before exec (NAME=VAL on macOS,
	// --setenv NAME VAL on Linux).
	SetEnv map[string]string

	// Shell is the interpreter the wrapped command runs under; empty defaults to
	// "bash".
	Shell string

	// LinuxMounts, when non-nil, is the precomputed bwrap filesystem argument
	// list (PlanLinuxMounts); nil computes a side-effect-free one.
	LinuxMounts []string
	// LinuxMountsFinal are bwrap arguments applied after every mount,
	// including the proxy bridge binds (the IPC masks' --remount-ro).
	LinuxMountsFinal []string
	// EnableWeakerNestedSandbox binds the host /proc instead of mounting a fresh
	// one (unprivileged containers). Linux only.
	EnableWeakerNestedSandbox bool
	// DisableUserns adds --disable-userns (bubblewrap 0.8+, not setuid): the
	// command may not create user namespaces of its own. Linux only.
	DisableUserns bool

	// LogTag is the message attached to "(deny default ...)" and every clause's
	// "(with message ...)", used to attribute violations in the system log.
	LogTag string
}

// defaultLogTag is the fallback violation tag (a stable string suffices for
// the renderers).
const defaultLogTag = "gocode-sandbox"

// shell returns the configured shell or "bash".
func (p Profile) shell() string {
	if p.Shell == "" {
		return "bash"
	}
	return p.Shell
}

// isGlob reports whether an entry carries shell/seatbelt glob metacharacters
// and must be rendered as a regex rather than a subpath.
func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }
