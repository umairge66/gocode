package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Manager is the session's sandbox: it answers whether a Bash command runs
// confined, and wraps the commands that do. It owns the session's filtering
// proxy (and, on Linux, the bridge socket) for the session's lifetime.
type Manager struct {
	opts     ManagerOptions
	settings Settings
	platform string

	depsOnce sync.Once
	deps     Dependencies

	mu                  sync.Mutex
	initialized         bool
	initErr             error
	disabledThisSession bool
	closed              bool
	proxy               *Proxy
	bridge              *LinuxBridge
	forwarders          []net.Listener
	httpPort, socksPort int
	emptyDir            string
	runDir              string
	disableUserns       bool
	active              int
	placeholders        []Placeholder
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Layers are the session's settings sources, lowest precedence first.
	Layers []Layer
	// Paths are the session directories (TempDir defaults to TempRoot).
	Paths SessionPaths
	// GOOS overrides runtime.GOOS (tests).
	GOOS string
	// LookPath overrides exec.LookPath (tests).
	LookPath func(string) (string, error)
	// Getenv reads the environment (gocode_CODE_TMPDIR); nil uses os.Getenv.
	Getenv func(string) string
	// Environ enumerates the environment a sandboxed command would inherit, in
	// os.Environ "NAME=VALUE" form; nil uses os.Environ. It is read only to
	// decide which secret-bearing variable NAMES to strip (SecretEnvNames);
	// values are never inspected.
	Environ func() []string
	// ReadFile reads the /proc/sys user-namespace settings in the dependency
	// probe (tests); nil uses os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// Logf receives debug lines; nil discards them. Nothing is sent anywhere.
	Logf func(format string, args ...any)
	// Interactive selects the interactive wording of hints.
	Interactive bool
	// AskNetwork, when set, decides a host neither allowedDomains nor
	// deniedDomains names (the interactive network prompt); nil denies it.
	AskNetwork func(host string, port int) bool
}

// Dependencies is the result of the platform dependency probe.
type Dependencies struct {
	Errors   []string
	Warnings []string
}

// NewManager resolves the settings and returns the session's Manager. Nothing
// is started until Start or the first Wrap.
func NewManager(opts ManagerOptions) *Manager {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Paths.TempDir == "" && opts.GOOS != "windows" {
		opts.Paths.TempDir = TempRoot(opts.Getenv)
	}
	m := &Manager{opts: opts}
	m.settings = ParseSettings(opts.Layers, PathContext{Cwd: opts.Paths.Cwd, Home: opts.Paths.Home, ConfigDir: opts.Paths.ConfigDir})
	m.platform = PlatformName(opts.GOOS)
	return m
}

// PlatformName maps a GOOS to the sandbox platform names.
func PlatformName(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

// Settings returns the resolved sandbox settings.
func (m *Manager) Settings() Settings { return m.settings }

// Platform is the platform name ("macos", "linux", "windows", …).
func (m *Manager) Platform() string { return m.platform }

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// environ returns the environment a sandboxed command would inherit, in
// os.Environ form. It is read only to choose which secret variable names to
// strip; values are never used.
func (m *Manager) environ() []string {
	if m.opts.Environ != nil {
		return m.opts.Environ()
	}
	return os.Environ()
}

// EnabledInSettings is sandbox.enabled.
func (m *Manager) EnabledInSettings() bool { return m.settings.Enabled }

// PlatformListed applies the managed sandbox.enabledPlatforms list (absent =
// every platform; empty = none).
func (m *Manager) PlatformListed() bool {
	if m.settings.EnabledPlatforms == nil {
		return true
	}
	for _, p := range m.settings.EnabledPlatforms {
		if p == m.platform {
			return true
		}
	}
	return false
}

// SupportedPlatform reports a platform with a sandbox backend: macOS
// (sandbox-exec) and Linux (bubblewrap). There is no Windows backend.
func (m *Manager) SupportedPlatform() bool {
	return m.platform == "macos" || m.platform == "linux"
}

// passesCheapGates reports enabled, listed and supported.
func (m *Manager) passesCheapGates() bool {
	return m.EnabledInSettings() && m.PlatformListed() && m.SupportedPlatform()
}

// CheckDependencies probes the platform's sandbox tools once.
func (m *Manager) CheckDependencies() Dependencies {
	m.depsOnce.Do(func() { m.deps = m.probeDependencies() })
	return m.deps
}

func (m *Manager) probeDependencies() Dependencies {
	var d Dependencies
	switch m.platform {
	case "windows":
		d.Errors = append(d.Errors, "Windows sandbox is not enabled")
	case "macos":
		if _, err := os.Stat(SeatbeltExecPath); err != nil {
			d.Errors = append(d.Errors, "sandbox-exec not available at "+SeatbeltExecPath)
		}
	case "linux":
		if p := m.settings.BwrapPath; p != "" {
			if !isExecutable(p) {
				d.Errors = append(d.Errors, "bubblewrap (bwrap) not executable at "+p)
			}
		} else if _, err := m.opts.LookPath(BubblewrapPath); err != nil {
			d.Errors = append(d.Errors, "bubblewrap (bwrap) not installed")
		}
		if p := m.settings.SocatPath; p != "" {
			if !isExecutable(p) {
				d.Errors = append(d.Errors, "socat not executable at "+p)
			}
		} else if _, err := m.opts.LookPath("socat"); err != nil {
			d.Errors = append(d.Errors, "socat not installed")
		}
		d.Errors = append(d.Errors, m.usernsErrors()...)
		if !m.settings.Network.AllowAllUnixSockets {
			// No seccomp helper is shipped: Unix-socket creation is not
			// filtered on Linux. Host IPC sockets are hidden from the
			// sandbox's filesystem view instead (ipc.go).
			d.Warnings = append(d.Warnings, "seccomp not available - unix socket access not restricted (host IPC sockets are hidden from the sandbox instead)")
		}
		if v := m.readSysctl("kernel/apparmor_restrict_unprivileged_userns"); v == "1" {
			d.Warnings = append(d.Warnings, "AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns = 1): bubblewrap needs an AppArmor profile that permits them")
		}
		if bw := m.bwrapBinary(); bw != "" {
			if v := bwrapVersion(bw); v != "" && compareVersions(v, "0.5.0") < 0 {
				d.Warnings = append(d.Warnings, fmt.Sprintf("bubblewrap %s at %s is older than 0.5.0: a denyRead entry or credential mask naming a path that is not a regular file (a fifo, socket or device node) cannot be applied on it. Everything else is unaffected", v, bw))
			}
		}
	}
	return d
}

// usernsErrors reports kernel settings that make bubblewrap's user namespace
// impossible for an unprivileged (non-setuid) bwrap.
func (m *Manager) usernsErrors() []string {
	var errs []string
	if v := m.readSysctl("user/max_user_namespaces"); v == "0" {
		errs = append(errs, "user namespaces are disabled (user.max_user_namespaces = 0)")
	}
	if v := m.readSysctl("kernel/unprivileged_userns_clone"); v == "0" {
		setuid := false
		if bw := m.bwrapBinary(); bw != "" {
			if fi, err := os.Stat(bw); err == nil && fi.Mode()&os.ModeSetuid != 0 {
				setuid = true
			}
		}
		if !setuid {
			errs = append(errs, "unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone = 0)")
		}
	}
	return errs
}

// readSysctl reads /proc/sys/<name> ("" when absent).
func (m *Manager) readSysctl(name string) string {
	read := m.opts.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	b, err := read("/proc/sys/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// bwrapBinary resolves the bubblewrap binary ("" when absent).
func (m *Manager) bwrapBinary() string {
	if m.settings.BwrapPath != "" {
		return m.settings.BwrapPath
	}
	p, err := m.opts.LookPath(BubblewrapPath)
	if err != nil {
		return ""
	}
	return p
}

func (m *Manager) socatBinary() string {
	if m.settings.SocatPath != "" {
		return m.settings.SocatPath
	}
	p, err := m.opts.LookPath("socat")
	if err != nil {
		return "socat"
	}
	return p
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)*`)

func bwrapVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return ""
	}
	return versionRe.FindString(string(out))
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// IsSandboxingEnabled reports whether Bash commands run sandboxed in this
// session: enabled in settings, on a listed and supported platform, with the
// dependencies present, and not switched off by a failed start.
func (m *Manager) IsSandboxingEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	off := m.disabledThisSession
	m.mu.Unlock()
	if off || !m.passesCheapGates() {
		return false
	}
	return len(m.CheckDependencies().Errors) == 0
}

// IsSandboxRequired is sandbox.failIfUnavailable for an enabled sandbox on a
// listed platform.
func (m *Manager) IsSandboxRequired() bool {
	return m != nil && m.EnabledInSettings() && m.PlatformListed() && m.settings.FailIfUnavailable
}

// UnavailableReason explains why an enabled sandbox cannot run ("" when it
// can, or is not enabled).
func (m *Manager) UnavailableReason() string {
	if m == nil || !m.EnabledInSettings() || !m.PlatformListed() {
		return ""
	}
	if !m.SupportedPlatform() {
		return fmt.Sprintf("sandbox is enabled but %s is not supported (requires macOS, Linux, or WSL2)", m.platform)
	}
	m.mu.Lock()
	initErr := m.initErr
	m.mu.Unlock()
	if d := m.CheckDependencies(); len(d.Errors) > 0 {
		hint := "run /sandbox for details"
		if !m.opts.Interactive {
			hint = "run `gocode sandbox status` for details"
		}
		if m.platform == "linux" {
			hint = "install missing tools (e.g. apt install bubblewrap socat) or " + hint
		}
		return "sandbox is enabled but dependencies are missing: " + strings.Join(d.Errors, ", ") + " · " + hint
	}
	if initErr != nil {
		return "sandbox is enabled but failed to initialize: " + initErr.Error()
	}
	return ""
}

// AutoAllowSupported is false on Windows.
func (m *Manager) AutoAllowSupported() bool { return m.platform != "windows" }

// AutoAllowEnabled is sandbox.autoAllowBashIfSandboxed (default true) where
// supported.
func (m *Manager) AutoAllowEnabled() bool {
	return m != nil && m.AutoAllowSupported() && m.settings.AutoAllowBashIfSandboxed
}

// StrictMode reports allowUnsandboxedCommands: false (the
// dangerouslyDisableSandbox parameter is ignored).
func (m *Manager) StrictMode() bool { return m.settings.UnsandboxedForbidden }

// UnsandboxedCommandsAllowed reports whether dangerouslyDisableSandbox may take
// a command out of the sandbox. A required sandbox that stopped being
// available refuses that too.
func (m *Manager) UnsandboxedCommandsAllowed() bool {
	if m.StrictMode() {
		return false
	}
	if m.IsSandboxRequired() && !m.IsSandboxingEnabled() {
		return false
	}
	return true
}

// ExcludedCommands is sandbox.excludedCommands (merged).
func (m *Manager) ExcludedCommands() []string { return m.settings.ExcludedCommands }

// ShouldSandbox decides whether a Bash command runs inside the sandbox.
//
//   - no sandbox in this session → unsandboxed;
//   - dangerouslyDisableSandbox → unsandboxed, unless strict mode
//     (allowUnsandboxedCommands: false) ignores the parameter;
//   - a command whose every subcommand matches sandbox.excludedCommands →
//     unsandboxed (not in strict mode);
//   - otherwise sandboxed.
//
// An unsandboxed command still goes through the normal permission flow.
func (m *Manager) ShouldSandbox(command string, dangerouslyDisableSandbox bool) bool {
	if m == nil || !m.IsSandboxingEnabled() {
		return false
	}
	strict := !m.UnsandboxedCommandsAllowed()
	if dangerouslyDisableSandbox && !strict {
		return false
	}
	if strings.TrimSpace(command) == "" {
		return strict
	}
	if !strict && ContainsExcludedCommand(command, m.settings.ExcludedCommands) {
		return false
	}
	return true
}

// StartupNotice is the startup diagnosis: fatal when the sandbox is required
// but unavailable (the session must not start), otherwise a warning that
// commands will run unsandboxed. Both are "" when there is nothing to say.
func (m *Manager) StartupNotice() (fatal, warning string) {
	reason := m.UnavailableReason()
	if reason == "" {
		return "", ""
	}
	if m.IsSandboxRequired() {
		return "Error: sandbox required but unavailable: " + reason + "\n" +
			"  sandbox.failIfUnavailable is set — refusing to start without a working sandbox.", ""
	}
	return "", "⚠ Sandbox disabled: " + reason + "\n" +
		"  Commands will run WITHOUT sandboxing. Network and filesystem restrictions will NOT be enforced."
}

// RequiredError is the refusal for a command when the sandbox must run but
// cannot.
type RequiredError struct{ Message string }

func (e *RequiredError) Error() string { return e.Message }

// Start initializes the sandbox eagerly (proxy, bridge), so a start failure
// is known before the first command is permission-checked. It is a no-op when
// sandboxing is not enabled.
func (m *Manager) Start() error {
	if m == nil || !m.IsSandboxingEnabled() {
		return nil
	}
	return m.ensureInitialized()
}

// ensureInitialized starts the proxy (and the Linux bridge) once.
func (m *Manager) ensureInitialized() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("sandbox: session closed")
	}
	if m.initialized {
		return nil
	}
	if m.initErr != nil {
		return m.startError(m.initErr)
	}
	if err := m.initLocked(); err != nil {
		m.initErr = err
		m.logf("Failed to initialize sandbox: %v", err)
		m.teardownLocked()
		if !m.IsSandboxRequired() {
			m.disabledThisSession = true
		}
		return m.startError(err)
	}
	m.initialized = true
	return nil
}

func (m *Manager) startError(err error) error {
	if m.IsSandboxRequired() {
		return &RequiredError{Message: fmt.Sprintf("Sandbox is required but failed to initialize: %v. Restart to retry.", err)}
	}
	return &RequiredError{Message: fmt.Sprintf("Sandbox is enabled but failed to initialize: %v. Sandboxing is disabled for the rest of this session; restart to retry.", err)}
}

func (m *Manager) initLocked() error {
	if dir := m.opts.Paths.TempDir; dir != "" {
		if err := ensurePrivateDir(dir); err != nil {
			return fmt.Errorf("temp dir %s: %w", dir, err)
		}
	}
	if m.platform == "linux" {
		if err := m.makeRunDirLocked(); err != nil {
			return fmt.Errorf("sandbox runtime dir: %w", err)
		}
	}
	n := m.settings.Network
	needOwn := n.HTTPProxyPort == 0 || n.SOCKSProxyPort == 0
	if needOwn {
		p, err := StartProxy(ProxyPolicy{
			AllowedDomains:  n.AllowedDomains,
			DeniedDomains:   n.DeniedDomains,
			StrictAllowlist: n.StrictAllowlist,
			Ask:             m.opts.AskNetwork,
			Logf:            m.opts.Logf,
		})
		if err != nil {
			return err
		}
		m.proxy = p
	}
	m.httpPort, m.socksPort = n.HTTPProxyPort, n.SOCKSProxyPort
	if m.httpPort == 0 {
		m.httpPort = m.proxy.Port()
	}
	if m.socksPort == 0 {
		m.socksPort = m.proxy.Port()
	}
	if m.platform == "linux" {
		if err := m.startLinuxBridge(); err != nil {
			return err
		}
		if bw := m.bwrapBinary(); bw != "" && !m.settings.AllowNestedUserNamespaces && !m.settings.EnableWeakerNestedSandbox {
			m.disableUserns = bwrapSupportsDisableUserns(bw)
		}
	}
	return nil
}

// startLinuxBridge creates the host bridge socket(s) in the temp dir: the
// in-process proxy serves its own socket directly; a user-configured proxy
// port gets a small Unix→TCP forwarder.
func (m *Manager) startLinuxBridge() error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(b[:])
	dir := m.runDir
	httpSock := filepath.Join(dir, "gocode-http-"+id+".sock")
	socksSock := filepath.Join(dir, "gocode-socks-"+id+".sock")
	serve := func(path string, port int) error {
		if m.proxy != nil && port == m.proxy.Port() {
			return m.proxy.ServeUnix(path)
		}
		return m.forwardUnix(path, port)
	}
	if err := serve(httpSock, m.httpPort); err != nil {
		return err
	}
	if m.socksPort == m.httpPort {
		socksSock = httpSock
	} else if err := serve(socksSock, m.socksPort); err != nil {
		return err
	}
	m.bridge = &LinuxBridge{HTTPSocket: httpSock, SOCKSSocket: socksSock, Socat: m.socatBinary()}
	return nil
}

// forwardUnix relays a Unix socket to a local TCP port (the user's own proxy).
func (m *Manager) forwardUnix(path string, port int) error {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	m.forwarders = append(m.forwarders, ln)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					_ = c.Close()
					return
				}
				pipe(c, up)
			}()
		}
	}()
	return nil
}

// bwrapSupportsDisableUserns probes `bwrap --help` for --disable-userns on a
// non-setuid bwrap.
func bwrapSupportsDisableUserns(bin string) bool {
	fi, err := os.Stat(bin)
	if err != nil || fi.Mode()&os.ModeSetuid != 0 {
		return false
	}
	out, _ := exec.Command(bin, "--help").CombinedOutput()
	return strings.Contains(string(out), "--disable-userns")
}

// makeRunDirLocked creates the session's private sandbox runtime directory:
// a fresh 0700 directory next to (not inside) the sandbox-writable temp dir,
// holding the proxy bridge sockets and the empty placeholder source. A
// sandboxed command can write in its temp dir, so keeping these out of it
// means it can neither unlink / replace the bridge socket nor fill the
// "empty" directory mounted over protected paths.
func (m *Manager) makeRunDirLocked() error {
	if m.runDir != "" {
		return nil
	}
	base := os.TempDir()
	if t := m.opts.Paths.TempDir; t != "" && len(filepath.Dir(t)) < 40 {
		// Next to the temp dir, when that keeps the bridge socket paths
		// within the Unix-socket path limit (~108 bytes).
		base = filepath.Dir(t)
	}
	d, err := os.MkdirTemp(base, "gocode-sbx-")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(d); err != nil {
		_ = os.Remove(d)
		return err
	}
	m.runDir = d
	return nil
}

// EmptyDir is the read-only source mounted over a directory placeholder.
func (m *Manager) EmptyDir() (string, error) {
	if m.emptyDir != "" {
		if entries, err := os.ReadDir(m.emptyDir); err == nil && len(entries) == 0 {
			return m.emptyDir, nil
		}
	}
	if m.runDir == "" {
		if err := m.makeRunDirLocked(); err != nil {
			return "", err
		}
	}
	d, err := os.MkdirTemp(m.runDir, "gocode-empty-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return "", err
	}
	m.emptyDir = d
	return d, nil
}

// Wrapped is a command prepared to run inside the sandbox.
type Wrapped struct {
	// Argv is the full argv to spawn (argv[0] is bwrap / env).
	Argv []string
	// Release must be called once the command has exited (removes the empty
	// creation-blocking placeholders when no other sandboxed command runs).
	Release func()
}

// Wrap prepares command (run by shellPath -c in dir) to run inside the
// sandbox. It starts the sandbox on first use.
func (m *Manager) Wrap(ctx context.Context, command, shellPath, dir string) (Wrapped, error) {
	if m == nil || !m.passesCheapGates() {
		return Wrapped{}, errors.New("sandbox: not enabled")
	}
	if !m.IsSandboxingEnabled() {
		if m.IsSandboxRequired() {
			return Wrapped{}, &RequiredError{Message: "Sandbox is required (sandbox.failIfUnavailable) and stopped being available in this session: " + m.UnavailableReason() + ". The command was refused, and so is a request to run it outside the sandbox (dangerouslyDisableSandbox): do not retry it that way."}
		}
		return Wrapped{}, errors.New("sandbox: not available in this session")
	}
	if err := ctx.Err(); err != nil {
		return Wrapped{}, err
	}
	if err := m.ensureInitialized(); err != nil {
		return Wrapped{}, err
	}
	rc := m.runtimeConfig(dir)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Wrapped{}, errors.New("sandbox: session closed")
	}
	p := m.profile(rc)
	var argv []string
	switch m.platform {
	case "linux":
		if !rc.FilesystemDisabled {
			p.ReadDeny = append(p.ReadDeny, "/etc/ssh/ssh_config.d")
		}
		plan, err := PlanLinuxMounts(p, m)
		if err != nil {
			RemovePlaceholders(plan.Placeholders)
			return Wrapped{}, err
		}
		if !rc.AllowAllUnixSockets {
			masks := m.ipcMasks(rc)
			plan.Args = withIPCMasks(plan.Args, masks)
			p.LinuxMountsFinal = masks.Final
		}
		p.LinuxMounts = plan.Args
		m.placeholders = append(m.placeholders, plan.Placeholders...)
		bw := m.bwrapBinary()
		if bw == "" {
			return Wrapped{}, errors.New("bubblewrap (bwrap) not installed")
		}
		argv = append([]string{bw}, BubblewrapArgv(p, command, shellPath)...)
		m.logf("[Sandbox Linux] Wrapped command with bwrap (network, filesystem restrictions)")
	case "macos":
		argv = SeatbeltArgv(p, command, shellPath)
	default:
		return Wrapped{}, errors.New(WindowsUnsupportedError)
	}
	m.active++
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.active--
			if m.active <= 0 {
				m.active = 0
				RemovePlaceholders(m.placeholders)
				m.placeholders = nil
			}
		})
	}
	return Wrapped{Argv: argv, Release: release}, nil
}

// WrapCommand adapts Wrap to the Bash tool's sandbox seam.
func (m *Manager) WrapCommand(ctx context.Context, command, shellPath, dir string) ([]string, func(), error) {
	w, err := m.Wrap(ctx, command, shellPath, dir)
	if err != nil {
		return nil, nil, err
	}
	return w.Argv, w.Release, nil
}

// RuntimeConfig is the confinement a command run in dir gets ("" = the
// session cwd).
func (m *Manager) RuntimeConfig(dir string) RuntimeConfig { return m.runtimeConfig(dir) }

func (m *Manager) runtimeConfig(dir string) RuntimeConfig {
	paths := m.opts.Paths
	if dir != "" && paths.Cwd != "" && !withinPath(filepath.Clean(dir), filepath.Clean(paths.Cwd)) {
		paths.AddDirs = append(append([]string{}, paths.AddDirs...), dir)
	}
	if paths.Cwd == "" {
		paths.Cwd = dir
	}
	return BuildRuntimeConfig(m.settings, paths)
}

// ipcMasks computes the host IPC masks for one command (see ipc.go).
func (m *Manager) ipcMasks(rc RuntimeConfig) ipcMasks {
	return hostIPCMasks(ipcInputs{
		Getenv:     m.opts.Getenv,
		UID:        os.Getuid(),
		Home:       m.opts.Paths.Home,
		ConfigDir:  m.opts.Paths.ConfigDir,
		Writable:   rc.AllowWrite,
		ReadOnly:   rc.AllowRead,
		Allow:      rc.AllowUnixSockets,
		FSDisabled: rc.FilesystemDisabled,
	})
}

// profile renders the runtime configuration as a Profile.
func (m *Manager) profile(rc RuntimeConfig) Profile {
	p := Profile{
		Network: NetworkPolicy{
			Restricted:        true,
			AllowLocalBinding: rc.AllowLocalBinding,
		},
		AllowUnixSockets:             rc.AllowUnixSockets,
		AllowAllUnixSockets:          rc.AllowAllUnixSockets,
		AllowMachLookup:              rc.AllowMachLookup,
		EnableWeakerNetworkIsolation: rc.EnableWeakerNetworkIsolation,
		AllowAppleEvents:             rc.AllowAppleEvents,
		EnableWeakerNestedSandbox:    rc.EnableWeakerNestedSandbox,
		DisableUserns:                m.disableUserns,
		SetEnv:                       map[string]string{},
		// rc.UnsetEnv is the user's explicit credential-deny list; merge in the
		// default secret-env scrub so provider API keys and other credential
		// variables never reach a sandboxed command (unless the user allowed
		// them via sandbox.credentials.envVars mode "allow").
		UnsetEnv: append(mergeSecretUnset(rc.UnsetEnv, m.environ(), m.settings.CredentialEnvAllow),
			socketEnvUnset(m.opts.Getenv, rc.AllowUnixSockets, rc.AllowAllUnixSockets)...),
		LogTag: defaultLogTag,
	}
	if rc.FilesystemDisabled {
		p.WriteUnrestricted = true
	} else {
		p.ReadDeny = rc.DenyRead
		p.ReadAllow = rc.AllowRead
		p.WriteAllow = rc.AllowWrite
		p.WriteDeny = rc.DenyWrite
		if m.opts.Paths.TempDir != "" {
			// TMPDIR = the session's sandbox-writable temp dir.
			p.SetEnv["TMPDIR"] = m.opts.Paths.TempDir
		}
	}
	switch m.platform {
	case "linux":
		p.Network.Bridge = m.bridge
		if m.settings.Network.HTTPProxyPort != 0 {
			p.Network.HTTPProxyPort = m.httpPort
		}
		if m.settings.Network.SOCKSProxyPort != 0 {
			p.Network.SOCKSProxyPort = m.socksPort
		}
	case "macos":
		p.Network.HTTPProxyPort = m.httpPort
		p.Network.SOCKSProxyPort = m.socksPort
		for _, kv := range ProxyEnv(m.httpPort, m.socksPort, true) {
			k, v, _ := strings.Cut(kv, "=")
			p.SetEnv[k] = v
		}
	}
	for k, v := range gitSafeDirectoryEnv(rc.GitSafeDirectories) {
		p.SetEnv[k] = v
	}
	return p
}

// gitSafeDirectoryEnv marks the repository roots safe for git inside the
// sandbox through GIT_CONFIG_COUNT/KEY/VALUE, after any the user set.
func gitSafeDirectoryEnv(dirs []string) map[string]string {
	if len(dirs) == 0 || os.Getenv("GIT_CONFIG_COUNT") == "0" {
		return nil
	}
	start, _ := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if start < 0 {
		start = 0
	}
	n := start
	out := map[string]string{}
	add := func(k, v string) {
		out["GIT_CONFIG_KEY_"+strconv.Itoa(n)] = k
		out["GIT_CONFIG_VALUE_"+strconv.Itoa(n)] = v
		n++
	}
	dirs = dedupe(dirs)
	if len(dirs) > 8 {
		add("safe.directory", "*")
	} else {
		for _, d := range dirs {
			d = strings.TrimRight(d, "/")
			add("safe.directory", d)
			add("safe.directory", d+"/*")
		}
	}
	out["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
	return out
}

// ProxyPort is the port of the session's own filtering proxy (0 when none is
// running).
func (m *Manager) ProxyPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proxy == nil {
		return 0
	}
	return m.proxy.Port()
}

// Close stops the proxy and the bridge and removes the sandbox's temporary
// files. Idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.teardownLocked()
	RemovePlaceholders(m.placeholders)
	m.placeholders = nil
	if m.emptyDir != "" {
		_ = os.Remove(m.emptyDir)
		m.emptyDir = ""
	}
	if m.runDir != "" {
		// Only empty: the sockets are unlinked by teardown; anything else
		// left there is kept rather than removed recursively.
		_ = os.Remove(m.runDir)
		m.runDir = ""
	}
	return nil
}

func (m *Manager) teardownLocked() {
	if m.proxy != nil {
		_ = m.proxy.Close()
		m.proxy = nil
	}
	for _, f := range m.forwarders {
		_ = f.Close()
	}
	m.forwarders = nil
	if m.bridge != nil {
		_ = os.Remove(m.bridge.HTTPSocket)
		_ = os.Remove(m.bridge.SOCKSSocket)
		m.bridge = nil
	}
}
