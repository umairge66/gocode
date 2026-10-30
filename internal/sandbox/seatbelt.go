package sandbox

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SeatbeltExecPath is the macOS sandbox binary the argv invokes.
const SeatbeltExecPath = "/usr/bin/sandbox-exec"

// SeatbeltProfile renders the macOS sandbox-exec ("seatbelt") policy for p: the
// "(version 1) (deny default …)" S-expression document passed to
// `sandbox-exec -p`.
//
// The essential-permission preamble (process/IPC/IOKit/sysctl/mach-lookup) is a
// compacted fixed header: the structurally load-bearing clauses are present
// and the long sysctl-name and global-name allowlists are represented by their
// shape, since the generated file-read / file-write /
// network sections are what the Profile actually drives.
func SeatbeltProfile(p Profile) string {
	tag := p.LogTag
	if tag == "" {
		tag = defaultLogTag
	}
	var b lines
	b.add("(version 1)")
	b.add(fmt.Sprintf("(deny default (with message %s))", quote(tag)))
	b.add("")
	b.add("; LogTag: " + tag)
	b.add("")

	// Essential permissions — Chrome-sandbox-derived fixed header.
	b.add("; Essential permissions - based on Chrome sandbox policy")
	b.add("; Process permissions")
	b.add("(allow process-exec)")
	b.add("(allow process-fork)")
	b.add("(allow process-info* (target same-sandbox))")
	b.add("(allow signal (target same-sandbox))")
	b.add("(allow mach-priv-task-port (target same-sandbox))")
	b.add("")
	b.add("; User preferences")
	b.add("(allow user-preference-read)")
	b.add("")
	b.add("; Mach IPC - specific services only (no wildcard)")
	b.add("(allow mach-lookup")
	for _, name := range seatbeltBaseMachServices {
		b.add("  (global-name " + quote(name) + ")")
	}
	b.add(")")
	if p.EnableWeakerNetworkIsolation {
		b.add("; trustd.agent - needed for Go TLS certificate verification (weaker network isolation)")
		b.add(`(allow mach-lookup (global-name "com.apple.trustd.agent"))`)
	}
	if p.AllowAppleEvents {
		b.add("; Apple Events - opt-in; needed for open/osascript to talk to other apps")
		b.add("(allow appleevent-send)")
		b.add(`(allow mach-lookup (global-name "com.apple.coreservices.appleevents"))`)
		b.add("(allow lsopen)")
		b.add(`(allow mach-lookup (global-name "com.apple.CoreServices.coreservicesd"))`)
	}
	for _, name := range p.AllowMachLookup {
		if strings.HasSuffix(name, "*") {
			b.add("(allow mach-lookup (global-name-prefix " + quote(strings.TrimSuffix(name, "*")) + "))")
		} else {
			b.add("(allow mach-lookup (global-name " + quote(name) + "))")
		}
	}
	b.add("")
	b.add("; POSIX IPC - shared memory and semaphores")
	b.add("(allow ipc-posix-shm)")
	b.add("(allow ipc-posix-sem)")
	b.add("")
	b.add("; IOKit - specific operations only")
	b.add("(allow iokit-open")
	b.add(`  (iokit-registry-entry-class "IOSurfaceRootUserClient")`)
	b.add(`  (iokit-registry-entry-class "RootDomainUserClient")`)
	b.add(`  (iokit-user-client-class "IOSurfaceSendRight")`)
	b.add(")")
	b.add("(allow iokit-get-properties)")
	b.add("")
	b.add("; Specific safe system-sockets, doesn't allow network access")
	b.add("(allow system-socket (require-all (socket-domain AF_SYSTEM) (socket-protocol 2)))")
	b.add("")
	b.add("; sysctl - specific sysctls only")
	b.add("(allow sysctl-read")
	for _, name := range seatbeltBaseSysctls {
		b.add("  (sysctl-name " + quote(name) + ")")
	}
	b.add(")")
	b.add("(allow sysctl-write")
	b.add(`  (sysctl-name "kern.tcsm_enable")`)
	b.add(")")
	b.add("")
	b.add(`(allow mach-lookup (global-name "com.apple.SecurityServer"))`)
	b.add("")
	b.add("; File I/O on device files")
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper", "/dev/tty"} {
		b.add("(allow file-ioctl (literal " + quote(dev) + "))")
	}
	b.add("")

	// Network.
	b.add("; Network")
	b.addAll(seatbeltNetwork(p.Network, p.AllowAllUnixSockets, p.AllowUnixSockets))
	b.add("")

	// File read.
	b.add("; File read")
	b.addAll(seatbeltFileRead(p, tag))
	b.add("")

	// File write.
	b.add("; File write")
	b.addAll(seatbeltFileWrite(p, tag))

	// Pseudo-terminal.
	if p.AllowPty {
		b.add("")
		b.add("; Pseudo-terminal (pty) support")
		b.add("(allow pseudo-tty)")
		b.add("(allow file-ioctl")
		b.add(`  (literal "/dev/ptmx")`)
		b.add(`  (regex #"^/dev/ttys")`)
		b.add(")")
		b.add("(allow file-read* file-write*")
		b.add(`  (literal "/dev/ptmx")`)
		b.add(`  (regex #"^/dev/ttys")`)
		b.add(")")
	}

	return b.join()
}

// seatbeltNetwork renders the "; Network" section.
func seatbeltNetwork(n NetworkPolicy, allowAllUnix bool, allowUnix []string) []string {
	if !n.Restricted {
		return []string{"(allow network*)"}
	}
	var out []string
	if n.AllowLocalBinding {
		out = append(out,
			`(allow network-bind (local ip "*:*"))`,
			`(allow network-inbound (local ip "*:*"))`,
			`(allow network-outbound (remote ip "localhost:*"))`,
		)
	}
	switch {
	case allowAllUnix:
		out = append(out,
			"(allow system-socket (socket-domain AF_UNIX))",
			`(allow network-bind (local unix-socket (path-regex #"^/")))`,
			`(allow network-outbound (remote unix-socket (path-regex #"^/")))`,
		)
	case len(allowUnix) > 0:
		out = append(out, "(allow system-socket (socket-domain AF_UNIX))")
		for _, dir := range allowUnix {
			out = append(out,
				fmt.Sprintf("(allow network-bind (local unix-socket (subpath %s)))", quote(dir)),
				fmt.Sprintf("(allow network-outbound (remote unix-socket (subpath %s)))", quote(dir)),
			)
		}
	}
	if n.HTTPProxyPort != 0 {
		out = append(out, localhostPortClauses(n.HTTPProxyPort)...)
	}
	if n.SOCKSProxyPort != 0 && n.SOCKSProxyPort != n.HTTPProxyPort {
		out = append(out, localhostPortClauses(n.SOCKSProxyPort)...)
	}
	return out
}

func localhostPortClauses(port int) []string {
	p := strconv.Itoa(port)
	return []string{
		fmt.Sprintf(`(allow network-bind (local ip "localhost:%s"))`, p),
		fmt.Sprintf(`(allow network-inbound (local ip "localhost:%s"))`, p),
		fmt.Sprintf(`(allow network-outbound (remote ip "localhost:%s"))`, p),
	}
}

// seatbeltFileRead renders the "; File read" section: read is allowed by
// default, then ReadDeny subtrees are denied, then ReadAllow paths re-permitted.
func seatbeltFileRead(p Profile, tag string) []string {
	if len(p.ReadDeny) == 0 && len(p.ReadAllow) == 0 {
		return []string{"(allow file-read*)"}
	}
	out := []string{"(allow file-read*)"}
	out = append(out, clause("deny", []string{"file-read*"}, denyEntries(p.ReadDeny), tag)...)
	out = append(out, clause("allow", []string{"file-read*"}, allowEntries(p.ReadAllow), tag)...)
	if len(p.ReadDeny) > 0 {
		out = append(out, "(allow file-read-metadata", "  (vnode-type DIRECTORY))")
	}
	return out
}

// seatbeltFileWrite renders the "; File write" section: write is denied by
// default; WriteAllow roots are permitted, then WriteDeny carve-outs re-denied.
func seatbeltFileWrite(p Profile, tag string) []string {
	if p.WriteUnrestricted {
		return []string{"(allow file-write*)"}
	}
	var out []string
	out = append(out, clause("allow", []string{"file-write*"}, allowEntries(p.WriteAllow), tag)...)
	if len(p.WriteDeny) > 0 {
		out = append(out, clause("deny", []string{"file-write*"}, denyEntries(p.WriteDeny), tag)...)
	}
	return out
}

// clause renders an (action ops entries) block with its message:
//
//	(action op1 op2
//	  entry1
//	  entry2
//	  (with message "msg"))
//
// An empty entry set renders nothing.
func clause(action string, ops []string, entries []string, msg string) []string {
	if len(entries) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("(%s %s", action, strings.Join(ops, " "))}
	for _, e := range entries {
		out = append(out, "  "+e)
	}
	out = append(out, fmt.Sprintf(`  (with message %s))`, quote(msg)))
	return out
}

// allowEntries / denyEntries turn a path set into sorted seatbelt path entries:
// a glob becomes a (regex …), a plain path a (subpath …).
func allowEntries(paths []string) []string { return pathEntries(paths) }
func denyEntries(paths []string) []string  { return pathEntries(paths) }

func pathEntries(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if isGlob(p) {
			out = append(out, "(regex "+quoteRegex(globToRegex(p))+")")
		} else {
			out = append(out, "(subpath "+quote(p)+")")
		}
	}
	sort.Strings(out)
	return out
}

// quote renders a string as a seatbelt string literal. Go's strconv.Quote
// produces the same escaping as JSON.stringify for the ASCII paths involved.
func quote(s string) string { return strconv.Quote(s) }

// quoteRegex renders a seatbelt extended-regex literal: #"...".
func quoteRegex(re string) string { return `#"` + re + `"` }

// globToRegex converts a glob path to an anchored seatbelt regex ("^…" with
// per-segment semantics); "*" matches within a segment, "**" across segments.
func globToRegex(g string) string {
	var sb strings.Builder
	sb.WriteByte('^')
	runes := []rune(g)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '\\':
			sb.WriteByte('\\')
			sb.WriteRune(c)
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
}

// SeatbeltArgv builds the argv that launches command under sandbox-exec:
//
//	env [-u NAME …] [NAME=VAL …] /usr/bin/sandbox-exec -p <profile> <shell> -c <command>
//
// shellPath is the resolved interpreter path (the caller supplies it so no
// lookup or exec happens here). An empty shellPath falls back to the Profile's
// shell name. The profile is passed inline with -p.
func SeatbeltArgv(p Profile, command, shellPath string) []string {
	if shellPath == "" {
		shellPath = p.shell()
	}
	argv := []string{"env"}
	for _, name := range p.UnsetEnv {
		argv = append(argv, "-u", name)
	}
	for _, name := range sortedKeys(p.SetEnv) {
		argv = append(argv, name+"="+p.SetEnv[name])
	}
	argv = append(argv, SeatbeltExecPath, "-p", SeatbeltProfile(p), shellPath, "-c", command)
	return argv
}

// sortedKeys returns the map keys in sorted order for a deterministic argv.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lines is a small ordered string builder for the profile document.
type lines struct{ s []string }

func (l *lines) add(line string)    { l.s = append(l.s, line) }
func (l *lines) addAll(ls []string) { l.s = append(l.s, ls...) }
func (l *lines) join() string       { return strings.Join(l.s, "\n") }

// seatbeltBaseMachServices is the fixed Mach/XPC global-name allowlist (the
// "(allow mach-lookup …)" leaf list).
var seatbeltBaseMachServices = []string{
	"com.apple.audio.systemsoundserver",
	"com.apple.FontObjectsServer",
	"com.apple.fonts",
	"com.apple.logd",
	"com.apple.lsd.mapdb",
	"com.apple.system.logger",
	"com.apple.system.notification_center",
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.bsd.dirhelper",
	"com.apple.securityd.xpc",
	"com.apple.coreservices.launchservicesd",
}

// seatbeltBaseSysctls is a representative subset of the fixed sysctl-read
// allowlist.
var seatbeltBaseSysctls = []string{
	"hw.activecpu", "hw.byteorder", "hw.cputype", "hw.logicalcpu", "hw.machine",
	"hw.memsize", "hw.ncpu", "hw.pagesize", "hw.physicalcpu",
	"kern.argmax", "kern.hostname", "kern.maxfiles", "kern.osproductversion",
	"kern.osrelease", "kern.ostype", "kern.osversion", "kern.version",
	"machdep.cpu.brand_string", "vm.loadavg",
}
