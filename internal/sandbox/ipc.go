package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Host IPC isolation (Linux backend).
//
// bubblewrap's --unshare-net isolates abstract Unix sockets (they live in the
// network namespace) but not filesystem sockets: a read-only bind of / still
// lets connect(2) reach every socket file on the host (a read-only mount does
// not stop a socket connect). Through those a sandboxed command could drive
// the systemd user manager or the session D-Bus (`systemd-run --user …`
// runs outside the sandbox), the ssh/gpg agents, docker/podman (root on the
// host), a tmux/screen server (keystrokes into the user's shells), the
// display server, this session's own peer-messaging socket, the background
// daemon and the browser-control host. A seccomp filter on socket(AF_UNIX)
// would close this, but none is shipped.
//
// The sandbox therefore hides the host's IPC endpoints from the sandbox's view,
// using only directory-level mounts so the result does not depend on which
// socket files happen to exist at the instant bwrap starts (a per-file mask
// over a socket that vanished in between makes bwrap abort):
//
//   - the temp directories (/tmp, the real os.TempDir / $TMPDIR, the
//     GOCODE_CODE_TMPDIR base) are covered with an empty tmpfs, and only what
//     must stay visible there is bound back on top: the writable roots
//     (cwd, --add-dir, additionalDirectories, allowWrite, the sandbox's own
//     temp root), allowRead entries and allowUnixSockets paths. Every current
//     AND future socket in a temp dir is hidden with no per-file scan.
//     TRADE-OFF: a sandboxed command can no longer read other programs'
//     files in /tmp — only the re-bound paths;
//   - the per-user runtime directories ($XDG_RUNTIME_DIR, /run/user/<uid>,
//     /run/dbus, /run/screen) get an empty tmpfs as well;
//   - directories that hold sockets among files the command may legitimately
//     read (/run, ~/.ssh, ~/.gnupg, ~/.docker, the config home, the directory
//     of a socket named by an env var) are "shadowed": an empty tmpfs with
//     every non-socket entry bound back with --ro-bind-try (so an entry that
//     vanished in between is skipped, not fatal), recursing into
//     subdirectories that hold sockets;
//   - all of these go right after the root bind, before the writable-root
//     binds and the write/read denies, which therefore still apply on top.
//
// The only socket bound in is the proxy bridge (bound last).
// sandbox.network.allowUnixSockets paths stay reachable, and
// allowAllUnixSockets turns the masking off. The environment variables that
// point a client at such a socket are unset (socketEnvNames). A socket created
// later inside a subdirectory that was bound back whole (one that held no
// socket at start) is not hidden; the temp and runtime dirs have no such gap.

// socketEnvNames are variables that point a client at a host IPC endpoint.
var socketEnvNames = []string{
	"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GPG_AGENT_INFO",
	"DBUS_SESSION_BUS_ADDRESS", "DBUS_SYSTEM_BUS_ADDRESS",
	"WAYLAND_DISPLAY", "WAYLAND_SOCKET", "DISPLAY", "XAUTHORITY",
	"PULSE_SERVER", "PIPEWIRE_REMOTE",
	"DOCKER_HOST", "CONTAINER_HOST",
	"TMUX", "TMUX_PANE", "STY", "KITTY_LISTEN_ON", "KITTY_PUBLIC_KEY",
	"VSCODE_IPC_HOOK", "VSCODE_IPC_HOOK_CLI", "VSCODE_GIT_IPC_HANDLE",
	"VSCODE_GIT_ASKPASS_MAIN", "VSCODE_GIT_ASKPASS_NODE", "VSCODE_GIT_ASKPASS_EXTRA_ARGS", "GIT_ASKPASS",
	"NVIM", "NVIM_LISTEN_ADDRESS", "EMACS_SOCKET_NAME",
	"GNOME_KEYRING_CONTROL", "KRB5CCNAME", "JUPYTER_RUNTIME_DIR",
}

// socketEnvUnset returns the socketEnvNames to unset: all of them, except a
// variable whose socket path is in allowUnixSockets (none at all with
// allowAll).
func socketEnvUnset(getenv func(string) string, allow []string, allowAll bool) []string {
	if allowAll {
		return nil
	}
	var out []string
	for _, name := range socketEnvNames {
		if p := socketPathFromEnv(name, getenv(name)); p != "" && pathAllowed(p, allow) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// socketPathFromEnv extracts a filesystem socket path from a variable value
// ("" when it names none).
func socketPathFromEnv(name, v string) string {
	switch {
	case v == "":
		return ""
	case strings.HasPrefix(v, "unix://"):
		return strings.TrimPrefix(v, "unix://")
	case strings.HasPrefix(v, "unix:"):
		for _, kv := range strings.Split(strings.TrimPrefix(v, "unix:"), ",") {
			if p, ok := strings.CutPrefix(kv, "path="); ok {
				return p
			}
		}
		return ""
	case filepath.IsAbs(v) && (name == "SSH_AUTH_SOCK" || strings.HasSuffix(name, "_SOCKET") || name == "NVIM" || name == "NVIM_LISTEN_ADDRESS"):
		return v
	}
	return ""
}

func pathAllowed(p string, allow []string) bool {
	p = filepath.Clean(p)
	for _, a := range allow {
		if a != "" && filepath.Clean(a) == p {
			return true
		}
	}
	return false
}

// ipcMasks are the bwrap arguments that hide host IPC: Early goes right after
// the root bind (before the writable binds and denies), Late after every
// other filesystem mount.
type ipcMasks struct {
	Early []string
	Late  []string
	// Final remounts the masking tmpfs mounts read-only once every mount
	// point inside them exists (after the bridge binds): a write outside the
	// writable roots still fails, as it did on the read-only root.
	Final []string
}

// ipcInputs are the session facts the masks are computed from.
type ipcInputs struct {
	Getenv    func(string) string
	UID       int
	Home      string
	ConfigDir string
	// Writable are the writable roots; bound back (read-write) where a mask
	// covers them.
	Writable []string
	// ReadOnly are allowRead entries; bound back read-only where covered.
	ReadOnly []string
	// Allow are sandbox.network.allowUnixSockets paths.
	Allow []string
	// FSDisabled (sandbox.filesystem.disabled): shadowed entries are bound
	// back read-write.
	FSDisabled bool

	// Test seams: nil uses the defaults.
	TmpDirs     []string
	RuntimeDirs []string
	ShadowDirs  []shadowDir
}

type shadowDir struct {
	dir   string
	depth int
}

const maxIPCScanEntries = 20000

// hostIPCMasks computes the masks.
func hostIPCMasks(in ipcInputs) ipcMasks {
	getenv := in.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	uid := strconv.Itoa(in.UID)
	realDir := func(p string) string {
		if p == "" || !filepath.IsAbs(p) {
			return ""
		}
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return ""
		}
		fi, err := os.Stat(r)
		if err != nil || !fi.IsDir() || r == "/" {
			return ""
		}
		return r
	}
	var writable []string
	for _, w := range in.Writable {
		if w == "" || isGlob(w) {
			continue
		}
		if r, err := filepath.EvalSymlinks(w); err == nil {
			writable = append(writable, r)
		}
	}
	insideWritable := func(p string) bool { return containsPath(writable, p) }

	// 1. Temp dirs: a plain tmpfs.
	tmpCands := in.TmpDirs
	if tmpCands == nil {
		tmpCands = []string{"/tmp", os.TempDir(), getenv("TMPDIR"), getenv("GOCODE_CODE_TMPDIR"), getenv("gocode_CODE_TMPDIR")}
	}
	var tmps []string
	for _, c := range tmpCands {
		if r := realDir(c); r != "" && !insideWritable(r) {
			tmps = append(tmps, r)
		}
	}
	tmps = outermost(tmps)

	// 2. Runtime dirs: a plain tmpfs (Early, or Late when inside a writable
	// root, where an early tmpfs would be covered by the root's bind).
	rtCands := in.RuntimeDirs
	if rtCands == nil {
		rtCands = []string{getenv("XDG_RUNTIME_DIR"), "/run/user/" + uid, "/run/screen", "/run/dbus"}
		if t := getenv("TMUX_TMPDIR"); t != "" {
			rtCands = append(rtCands, filepath.Join(t, "tmux-"+uid))
		}
	}
	var runtimeEarly, runtimeLate []string
	for _, c := range rtCands {
		r := realDir(c)
		if r == "" || containsPath(tmps, r) {
			continue
		}
		if insideWritable(r) {
			runtimeLate = append(runtimeLate, r)
		} else {
			runtimeEarly = append(runtimeEarly, r)
		}
	}
	runtimeEarly, runtimeLate = dedupe(runtimeEarly), dedupe(runtimeLate)
	hidden := append(append([]string{}, tmps...), runtimeEarly...)

	// 3. Shadow dirs.
	shCands := in.ShadowDirs
	if shCands == nil {
		shCands = []shadowDir{{"/run", 2}}
		if in.Home != "" {
			shCands = append(shCands, shadowDir{filepath.Join(in.Home, ".ssh"), 2},
				shadowDir{filepath.Join(in.Home, ".gnupg"), 2}, shadowDir{filepath.Join(in.Home, ".docker"), 2})
		}
		if in.ConfigDir != "" {
			shCands = append(shCands, shadowDir{in.ConfigDir, 3})
		}
		for _, name := range socketEnvNames {
			if p := socketPathFromEnv(name, getenv(name)); p != "" && !pathAllowed(p, in.Allow) {
				shCands = append(shCands, shadowDir{filepath.Dir(p), 0})
			}
		}
		if in.Home != "" {
			// The home directory itself only when it holds a socket now.
			if hasSocket(in.Home, 0, new(int)) {
				shCands = append(shCands, shadowDir{in.Home, 0})
			}
		}
	}
	bindFlag := "--ro-bind-try"
	if in.FSDisabled {
		bindFlag = "--bind-try"
	}
	var shadows []string
	var shadowArgs, tmpfsDirs []string
	budget := maxIPCScanEntries
	var shadow func(dir string, depth int)
	shadow = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		shadowArgs = append(shadowArgs, "--tmpfs", dir)
		tmpfsDirs = append(tmpfsDirs, dir)
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if containsPath(hidden, p) {
				continue // covered by its own tmpfs below
			}
			switch t := e.Type(); {
			case t&fs.ModeSocket != 0:
				continue // the point: not bound back
			case t&fs.ModeSymlink != 0:
				if target, err := os.Readlink(p); err == nil {
					shadowArgs = append(shadowArgs, "--symlink", target, p)
				}
			case e.IsDir() && depth > 0 && hasSocket(p, depth-1, &budget):
				shadow(p, depth-1)
			default:
				shadowArgs = append(shadowArgs, bindFlag, p, p)
			}
		}
	}
	var shDirs []shadowDir
	seen := map[string]bool{}
	for _, c := range shCands {
		r := realDir(c.dir)
		if r == "" || seen[r] || containsPath(hidden, r) || insideWritable(r) {
			continue
		}
		seen[r] = true
		shDirs = append(shDirs, shadowDir{r, c.depth})
		shadows = append(shadows, r)
	}
	// Parents first: a child's shadow overrides the parent's bind of it.
	sort.SliceStable(shDirs, func(i, j int) bool { return len(shDirs[i].dir) < len(shDirs[j].dir) })
	for _, d := range shDirs {
		shadow(d.dir, d.depth)
	}

	var m ipcMasks
	tmpfsDirs = append(append(append(tmpfsDirs, tmps...), runtimeEarly...), runtimeLate...)
	if !in.FSDisabled {
		for _, d := range tmpfsDirs {
			m.Final = append(m.Final, "--remount-ro", d)
		}
	}
	for _, t := range tmps {
		m.Early = append(m.Early, "--tmpfs", t)
	}
	m.Early = append(m.Early, shadowArgs...)
	for _, r := range runtimeEarly {
		m.Early = append(m.Early, "--tmpfs", r)
	}
	// Bind back what must stay visible under a covered directory: writable
	// roots read-write, allowRead / allowUnixSockets read-only; parents
	// first. "-try": a path that vanished is skipped, never fatal.
	covered := append(append([]string{}, hidden...), shadows...)
	type rebind struct{ flag, path string }
	var rbs []rebind
	add := func(flag string, paths []string) {
		for _, p := range paths {
			if p == "" || isGlob(p) || !filepath.IsAbs(p) {
				continue
			}
			r, err := filepath.EvalSymlinks(p)
			if err != nil {
				r = filepath.Clean(p)
			}
			if containsPath(covered, r) {
				rbs = append(rbs, rebind{flag, r})
			}
		}
	}
	add("--bind-try", writable)
	add("--ro-bind-try", in.ReadOnly)
	add("--ro-bind-try", in.Allow)
	sort.SliceStable(rbs, func(i, j int) bool { return len(rbs[i].path) < len(rbs[j].path) })
	for _, rb := range rbs {
		m.Early = append(m.Early, rb.flag, rb.path, rb.path)
	}
	for _, r := range runtimeLate {
		m.Late = append(m.Late, "--tmpfs", r)
		for _, a := range in.Allow {
			if a = filepath.Clean(a); withinPath(a, r) && a != r {
				m.Late = append(m.Late, "--ro-bind-try", a, a)
			}
		}
	}
	return m
}

// outermost drops paths contained in another one of the list.
func outermost(paths []string) []string {
	paths = dedupe(paths)
	var out []string
	for _, p := range paths {
		inner := false
		for _, q := range paths {
			if q != p && withinPath(p, q) {
				inner = true
				break
			}
		}
		if !inner {
			out = append(out, p)
		}
	}
	return out
}

// hasSocket reports a socket file in dir or (depth levels) below it.
func hasSocket(dir string, depth int, budget *int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if *budget--; *budget < 0 {
			return true // unknown: shadow rather than expose
		}
		if e.Type()&fs.ModeSocket != 0 {
			return true
		}
		if e.IsDir() && depth > 0 && hasSocket(filepath.Join(dir, e.Name()), depth-1, budget) {
			return true
		}
	}
	return false
}

// withIPCMasks splices the masks into a mount plan's arguments: Early right
// after the leading root bind ("--ro-bind / /" or "--bind / /"), Late at the
// end.
func withIPCMasks(args []string, m ipcMasks) []string {
	if len(args) < 3 || (args[0] != "--ro-bind" && args[0] != "--bind") || args[1] != "/" || args[2] != "/" {
		// Unexpected shape: keep the masks after everything (still safe for
		// the tmpfs parts, which then also hide writable roots below them).
		return append(append(append([]string{}, args...), m.Early...), m.Late...)
	}
	out := append([]string{}, args[:3]...)
	out = append(out, m.Early...)
	out = append(out, args[3:]...)
	return append(out, m.Late...)
}
