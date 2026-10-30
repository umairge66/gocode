package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// BubblewrapPath is the default Linux sandbox helper the argv invokes.
const BubblewrapPath = "bwrap"

// Linux proxy bridge ports: inside the sandbox's own network namespace the
// command reaches the host proxy through socat listeners on these localhost
// ports, which forward to the bridge sockets bound in from the host.
const (
	LinuxBridgeHTTPPort  = 3128
	LinuxBridgeSOCKSPort = 1080
)

// LinuxBridge describes the network bridge of a Linux sandbox: the host-side
// Unix sockets of the proxy and the socat that bridges them inside.
type LinuxBridge struct {
	// HTTPSocket / SOCKSSocket are the host bridge sockets (the same path when
	// one mux proxy serves both protocols).
	HTTPSocket  string
	SOCKSSocket string
	// Socat is the socat binary run inside the sandbox ("" = "socat" on PATH).
	Socat string
}

// BubblewrapArgv builds the bubblewrap argv that launches command under Linux
// confinement, excluding the leading bwrap binary itself (the caller spawns
// bwrap with these args). The argument assembly:
//
//   - --new-session --die-with-parent                     (session hardening)
//   - --unsetenv NAME / --setenv NAME VAL                 (env scrubbing)
//   - --unshare-net, the bridge socket binds and the proxy environment
//     (when Network.Restricted)
//   - filesystem binds (see PlanLinuxMounts)
//   - --dev /dev --unshare-pid --unshare-user --cap-drop ALL --proc /proc
//   - --disable-userns (when the bwrap supports it)
//   - -- <shell> -c <command>  (or the socat bridge script around it)
//
// shellPath is the resolved interpreter path (empty → the Profile's shell
// name); no PATH lookup or exec happens here.
func BubblewrapArgv(p Profile, command, shellPath string) []string {
	if shellPath == "" {
		shellPath = p.shell()
	}
	args := []string{"--new-session", "--die-with-parent"}

	// Environment scrubbing.
	for _, name := range p.UnsetEnv {
		args = append(args, "--unsetenv", name)
	}
	for _, name := range sortedKeys(p.SetEnv) {
		args = append(args, "--setenv", name, p.SetEnv[name])
	}

	// Network isolation: a private network namespace (loopback only), with the
	// host proxy reachable through the bridge sockets.
	bridged := false
	if p.Network.Restricted {
		args = append(args, "--unshare-net")
		if b := p.Network.Bridge; b != nil && b.HTTPSocket != "" && b.SOCKSSocket != "" {
			bridged = true
			for _, kv := range ProxyEnv(LinuxBridgeHTTPPort, LinuxBridgeSOCKSPort, false) {
				k, v, _ := strings.Cut(kv, "=")
				args = append(args, "--setenv", k, v)
			}
			if p.Network.HTTPProxyPort != 0 {
				args = append(args, "--setenv", "GOCODE_CODE_HOST_HTTP_PROXY_PORT", strconv.Itoa(p.Network.HTTPProxyPort))
			}
			if p.Network.SOCKSProxyPort != 0 {
				args = append(args, "--setenv", "GOCODE_CODE_HOST_SOCKS_PROXY_PORT", strconv.Itoa(p.Network.SOCKSProxyPort))
			}
		}
	}

	// Filesystem view.
	if p.LinuxMounts != nil {
		args = append(args, p.LinuxMounts...)
	} else {
		plan, _ := PlanLinuxMounts(p, nil)
		args = append(args, plan.Args...)
	}

	// The bridge sockets are bound last among the filesystem mounts, so no
	// mount (the read-only root, an IPC mask) can cover them.
	if bridged {
		b := p.Network.Bridge
		args = append(args, "--bind", b.HTTPSocket, b.HTTPSocket)
		if b.SOCKSSocket != b.HTTPSocket {
			args = append(args, "--bind", b.SOCKSSocket, b.SOCKSSocket)
		}
	}

	args = append(args, p.LinuxMountsFinal...)

	// Namespaces and device/proc.
	args = append(args, "--dev", "/dev", "--unshare-pid", "--unshare-user", "--cap-drop", "ALL")
	if p.EnableWeakerNestedSandbox {
		args = append(args, "--bind", "/proc", "/proc")
	} else {
		args = append(args, "--proc", "/proc")
	}
	args = append(args, "--unsetenv", "SRT_ALLOW_NESTED_USERNS")
	if p.DisableUserns {
		args = append(args, "--disable-userns")
	}

	// Command.
	args = append(args, "--", shellPath, "-c")
	if bridged {
		args = append(args, bridgeScript(p.Network.Bridge, command))
	} else {
		args = append(args, command)
	}
	return args
}

// bridgeScript wraps command in the in-sandbox socat bridge: two background
// listeners (3128 → the HTTP socket, 1080 → the SOCKS socket), a short wait
// until both are listening, then the command itself. The listeners are killed
// when the shell exits.
func bridgeScript(b *LinuxBridge, command string) string {
	socat := b.Socat
	if socat == "" {
		socat = "socat"
	}
	s := ShellQuote(socat)
	listen := func(port int, sock string) string {
		p := strconv.Itoa(port)
		return "{ " + s + " TCP6-LISTEN:" + p + ",ipv6only=0,fork,reuseaddr,backlog=4096 UNIX-CONNECT:" + sock +
			" || exec " + s + " TCP4-LISTEN:" + p + ",fork,reuseaddr,backlog=4096 UNIX-CONNECT:" + sock + "; } >/dev/null 2>&1 &"
	}
	lines := []string{
		listen(LinuxBridgeHTTPPort, b.HTTPSocket),
		"_srt_http=$!",
		listen(LinuxBridgeSOCKSPort, b.SOCKSSocket),
		"_srt_socks=$!",
		"trap 'rc=$?; kill %1 %2 2>/dev/null; exit $rc' EXIT",
	}
	lines = append(lines, bridgeWaitLoop...)
	lines = append(lines, "eval "+ShellQuote(command))
	return strings.Join(lines, "\n")
}

// bridgeWaitLoop waits (briefly) until both bridge listeners show up in the
// namespace's TCP socket count.
var bridgeWaitLoop = []string{
	"_srt_since=",
	`while [ -r /proc/net/sockstat ] && kill -0 "$_srt_http" "$_srt_socks" 2>/dev/null; do`,
	"  _srt_up=0",
	"  for _srt_counts in /proc/net/sockstat /proc/net/sockstat6; do",
	`    [ -r "$_srt_counts" ] || continue`,
	"    while read -r _srt_kind _srt_skip _srt_inuse _srt_skip; do",
	`      case "$_srt_kind" in TCP: | TCP6:) _srt_up=$((_srt_up + _srt_inuse)); break ;; esac`,
	`    done < "$_srt_counts"`,
	"  done",
	`  [ "$_srt_up" -lt 2 ] || break`,
	"  read -r _srt_now _srt_skip 2>/dev/null < /proc/uptime || break",
	"  _srt_now=1${_srt_now%.*}${_srt_now#*.}",
	`  [ "$((_srt_now - ${_srt_since:=$_srt_now}))" -lt 30 ] || break`,
	"done",
	"unset _srt_since _srt_now _srt_up _srt_counts _srt_kind _srt_skip _srt_inuse _srt_http _srt_socks",
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@+,-]+$`)

// ShellQuote quotes s for a POSIX shell (single quotes; a bare word when it
// needs none).
func ShellQuote(s string) string {
	if s != "" && shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ProxyEnv is the proxy environment a sandboxed command gets: every common
// spelling of the HTTP/HTTPS/ALL proxy variables pointing at the sandbox
// proxy, NO_PROXY for loopback and the private ranges, and the SOCKS-based
// helpers (git over ssh, ftp, rsync, docker, gcloud). macos selects the
// `nc -X 5` ssh ProxyCommand; elsewhere ssh goes through socat's HTTP CONNECT.
func ProxyEnv(httpPort, socksPort int, macos bool) []string {
	env := []string{"SANDBOX_RUNTIME=1"}
	if httpPort == 0 && socksPort == 0 {
		return env
	}
	noProxy := "localhost,127.0.0.1,::1,169.254.0.0/16,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	env = append(env, "NO_PROXY="+noProxy, "no_proxy="+noProxy)
	if httpPort != 0 {
		u := "http://localhost:" + strconv.Itoa(httpPort)
		env = append(env, "HTTP_PROXY="+u, "HTTPS_PROXY="+u, "http_proxy="+u, "https_proxy="+u)
	}
	all := "socks5h://localhost:" + strconv.Itoa(socksPort)
	if httpPort != 0 {
		all = "http://localhost:" + strconv.Itoa(httpPort)
	}
	env = append(env, "ALL_PROXY="+all, "all_proxy="+all, "GRPC_PROXY="+all, "grpc_proxy="+all)
	if socksPort != 0 {
		sp := strconv.Itoa(socksPort)
		switch {
		case macos:
			env = append(env, "GIT_SSH_COMMAND=ssh -o ControlMaster=no -o ControlPath=none -o ProxyCommand='nc -X 5 -x localhost:"+sp+" %h %p'")
		case httpPort != 0:
			env = append(env, "GIT_SSH_COMMAND=ssh -o ControlMaster=no -o ControlPath=none -o ProxyCommand='socat - PROXY:localhost:%h:%p,proxyport="+strconv.Itoa(httpPort)+"'")
		}
		env = append(env,
			"FTP_PROXY=socks5h://localhost:"+sp, "ftp_proxy=socks5h://localhost:"+sp,
			"RSYNC_PROXY=localhost:"+sp)
		dp := httpPort
		if dp == 0 {
			dp = socksPort
		}
		env = append(env,
			"DOCKER_HTTP_PROXY=http://localhost:"+strconv.Itoa(dp),
			"DOCKER_HTTPS_PROXY=http://localhost:"+strconv.Itoa(dp))
		if httpPort != 0 {
			env = append(env, "CLOUDSDK_PROXY_TYPE=http", "CLOUDSDK_PROXY_ADDRESS=localhost", "CLOUDSDK_PROXY_PORT="+strconv.Itoa(httpPort))
		}
	}
	return env
}

// LinuxMountPlan is the filesystem part of a bwrap argv computed against the
// live filesystem, with the creation-blocking placeholders it made on the host.
type LinuxMountPlan struct {
	Args []string
	// Placeholders are the empty files/directories created on the host as
	// mount points that block creating a write-protected path that did not
	// exist yet; remove them (when still empty) once the command has exited
	// (RemovePlaceholders).
	Placeholders []Placeholder
}

// PlaceholderSource supplies the read-only empty directory mounted over a
// directory placeholder.
type PlaceholderSource interface {
	EmptyDir() (string, error)
}

// PlanLinuxMounts computes the bwrap filesystem arguments for p. With src
// non-nil it also blocks creation of write-protected paths that do not exist
// yet, by creating an empty placeholder (file or directory) at the first
// missing component and mounting /dev/null or an empty read-only directory
// over it; with src nil it has no side effects and such paths are skipped.
func PlanLinuxMounts(p Profile, src PlaceholderSource) (LinuxMountPlan, error) {
	var plan LinuxMountPlan
	args := []string{}
	var writable []string

	if p.WriteUnrestricted {
		args = append(args, "--bind", "/", "/")
		writable = []string{"/"}
	} else {
		args = append(args, "--ro-bind", "/", "/")
		for _, root := range sortedCopy(p.WriteAllow) {
			if isGlob(root) || root == "/dev" || strings.HasPrefix(root, "/dev/") {
				continue
			}
			real, err := filepath.EvalSymlinks(root)
			if err != nil {
				continue // a write root that does not exist is skipped
			}
			if hasExactPath(writable, real) {
				continue
			}
			args = append(args, "--bind", real, real)
			writable = append(writable, real)
		}

		// A protected path reached through a symbolic link that lives inside
		// a writable root cannot be protected by a mount: the mount lands on
		// the link's target, and the command can replace the link itself
		// (rm it, create the real thing). Refuse to run rather than leave it
		// writable.
		var lexRoots []string
		for _, root := range p.WriteAllow {
			if !isGlob(root) && root != "" {
				lexRoots = append(lexRoots, filepath.Clean(root))
				if real, err := filepath.EvalSymlinks(root); err == nil {
					lexRoots = append(lexRoots, real)
				}
			}
		}
		var resolvedDenies []string
		for _, d := range sortedCopy(dedupe(p.WriteDeny)) {
			if isGlob(d) {
				continue
			}
			if link := symlinkBelowRoot(lexRoots, filepath.Clean(d)); link != "" {
				return plan, fmt.Errorf("sandbox: the protected path %s is reached through the symbolic link %s inside a writable directory, so a sandboxed command could replace the link; replace the link with the real file or directory (or make that directory non-writable) to run commands in the sandbox", d, link)
			}
			resolvedDenies = append(resolvedDenies, resolveExisting(d))
		}
		sort.Strings(resolvedDenies)
		isAncestorOfDeny := func(path string) bool {
			for _, d := range resolvedDenies {
				if d != path && withinPath(d, path) {
					return true
				}
			}
			return false
		}
		var blocked, denied []string
		for _, d := range resolvedDenies {
			if d == "/" || !containsPath(writable, d) {
				continue
			}
			if containsPath(blocked, d) || hasExactPath(denied, d) {
				continue
			}
			if _, err := os.Lstat(d); err == nil {
				args = append(args, "--ro-bind", d, d)
				denied = append(denied, d)
				continue
			}
			if src == nil {
				continue
			}
			miss := firstMissing(d)
			if miss == "" || !containsPath(writable, miss) || containsPath(blocked, miss) || containsPath(denied, miss) {
				continue
			}
			asDir := miss != d || isAncestorOfDeny(miss)
			source := "/dev/null"
			if asDir {
				empty, err := src.EmptyDir()
				if err != nil {
					return plan, fmt.Errorf("sandbox: cannot protect %s: %w", d, err)
				}
				source = empty
			}
			ph, err := createPlaceholder(miss, asDir)
			if err != nil {
				if placeholderSkippable(err) {
					// The user cannot create it either (read-only or
					// unwritable parent), so neither can the command.
					continue
				}
				// Fail closed: running the command without the mount point
				// would leave the protected path creatable.
				return plan, fmt.Errorf("sandbox: cannot protect %s (placeholder %s): %w", d, miss, err)
			}
			plan.Placeholders = append(plan.Placeholders, ph)
			args = append(args, "--ro-bind", source, miss)
			blocked = append(blocked, miss)
		}
	}

	// Read denies.
	allowRead := cleanAll(p.ReadAllow)
	for _, d := range sortedCopy(dedupe(p.ReadDeny)) {
		if isGlob(d) {
			continue
		}
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			continue
		}
		fi, err := os.Stat(real)
		if err != nil {
			continue
		}
		if hasExactPath(allowRead, real) || hasExactPath(allowRead, filepath.Clean(d)) {
			continue
		}
		if !fi.IsDir() {
			args = append(args, "--ro-bind", "/dev/null", real)
			continue
		}
		args = append(args, "--tmpfs", real)
		for _, a := range sortedCopy(allowRead) {
			if isGlob(a) {
				continue
			}
			ra, err := filepath.EvalSymlinks(a)
			if err != nil || ra == real || !withinPath(ra, real) {
				continue
			}
			args = append(args, "--ro-bind", ra, ra)
		}
		for _, w := range writable {
			if w != real && withinPath(w, real) {
				args = append(args, "--bind", w, w)
			}
		}
	}
	plan.Args = args
	return plan, nil
}

// symlinkBelowRoot returns the first path component strictly below a root
// containing p that is a symbolic link ("" when there is none).
func symlinkBelowRoot(roots []string, p string) string {
	for _, root := range roots {
		if root == p || !withinPath(p, root) {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		cur := root
		for _, comp := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, comp)
			fi, err := os.Lstat(cur)
			if err != nil {
				break
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return cur
			}
		}
	}
	return ""
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// re-attaches the missing remainder.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir := filepath.Dir(p)
	if dir == p {
		return p
	}
	return filepath.Join(resolveExisting(dir), filepath.Base(p))
}

// firstMissing returns the shallowest missing component of p ("" when p
// exists).
func firstMissing(p string) string {
	var chain []string
	for cur := p; ; cur = filepath.Dir(cur) {
		chain = append(chain, cur)
		if filepath.Dir(cur) == cur {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if _, err := os.Lstat(chain[i]); err != nil {
			return chain[i]
		}
	}
	return ""
}

// withinPath reports whether p is root or beneath it.
func withinPath(p, root string) bool {
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

func containsPath(roots []string, p string) bool {
	for _, r := range roots {
		if withinPath(p, r) {
			return true
		}
	}
	return false
}

func hasExactPath(list []string, p string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

// sortedCopy returns a sorted copy of s without mutating the input.
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// WindowsUnsupportedError is the reason for having no
// shell-string sandbox on Windows: confinement there is a WFP egress fence plus
// filesystem ACLs provisioned out of band (`npx sandbox-runtime
// windows-install`), reachable only through wrapWithSandboxArgv, and
// wrapWithSandbox throws rather than returning a wrapped shell string.
//
// Only the macOS and Linux renderers are implemented; the Windows WFP/
// ACL path is intentionally not implemented.
const WindowsUnsupportedError = "sandbox: shell-string wrapping is not supported on Windows; " +
	"Windows uses a WFP egress fence and filesystem ACLs provisioned by sandbox-runtime"
