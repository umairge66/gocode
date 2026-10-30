package cli

import (
	"runtime"
	"strings"

	"github.com/umairge66/gocode/internal/sandbox"
)

// `gocode sandbox <install|status>` (hidden): sandbox posture management.
//
// install provisions the Windows sandbox (a sandbox user plus WFP network
// filters, self-elevating through UAC). There is no Windows sandbox, so
// install always reports an "unavailable" result. status reports
// the live posture from the session's sandbox manager (internal/sandbox): the
// macOS (sandbox-exec) and Linux (bubblewrap + socat) backends are real, so
// `enabled` is true when the sandbox would confine Bash commands right now.
// On macOS and Linux the available/installed/reasons trio reports the live
// dependency probe (sandbox-exec; bwrap, socat and kernel user-namespace
// support). Native Windows reports the Windows-install gate.

// sandboxWindowsGate reports why the Windows install path is not
// available, or "" when it is. There is no Windows sandbox, so on native
// Windows "not enabled on this build" applies; that check precedes the
// enabledPlatforms one.
func sandboxWindowsGate(goos string, _ bool) string {
	if goos != "windows" {
		return "Windows sandbox install is only available on native Windows."
	}
	return "The Windows sandbox is not enabled on this build."
}

// sandboxSource names where the sandbox setting comes from (forced, policy
// or settings).
func sandboxSource(forced, policy, settings bool) string {
	switch {
	case forced:
		return "forced"
	case policy:
		return "policy"
	case settings:
		return "settings"
	default:
		return "off"
	}
}

// sandboxStatus is the status JSON (statusVersion 3), keys in output order.
type sandboxStatus struct {
	StatusVersion                  int      `json:"statusVersion"`
	Available                      bool     `json:"available"`
	Installed                      bool     `json:"installed"`
	PolicyLocked                   bool     `json:"policyLocked"`
	Reasons                        []string `json:"reasons"`
	Supported                      bool     `json:"supported"`
	Enabled                        bool     `json:"enabled"`
	EnabledSource                  string   `json:"enabledSource"`
	UnavailableReason              *string  `json:"unavailableReason"`
	StrictMode                     bool     `json:"strictMode"`
	StrictModeSource               string   `json:"strictModeSource"`
	FilesystemPolicy               string   `json:"filesystemPolicy"`
	AutoAllowBashIfSandboxed       bool     `json:"autoAllowBashIfSandboxed"`
	AutoAllowBashIfSandboxedSource string   `json:"autoAllowBashIfSandboxedSource"`
}

// computeSandboxStatus builds the status report from the session's sandbox
// manager.
func computeSandboxStatus(m *sandbox.Manager, goos string) sandboxStatus {
	s := m.Settings()
	st := sandboxStatus{StatusVersion: 3, Reasons: []string{}}
	// On macOS and Linux the backend is a real local sandbox (sandbox-exec /
	// bubblewrap + socat), with no out-of-band install step: report the live
	// dependency probe in available/installed/reasons. Only native Windows —
	// or an unsupported platform — falls back to the Windows-install gate text.
	if m.SupportedPlatform() {
		d := m.CheckDependencies()
		st.Available = true
		st.Installed = len(d.Errors) == 0
		st.PolicyLocked = s.LockedByPolicy()
		st.Reasons = append(st.Reasons, d.Errors...)
	} else if gate := sandboxWindowsGate(goos, m.PlatformListed()); gate != "" {
		st.Reasons = []string{gate}
	}
	st.Supported = m.SupportedPlatform() && m.PlatformListed()
	st.Enabled = m.IsSandboxingEnabled()
	st.EnabledSource = sandboxSource(false, s.LockedKeys["enabled"], m.EnabledInSettings())
	if reason := m.UnavailableReason(); reason != "" {
		// The in-session install hint does not apply to the CLI.
		reason = strings.Replace(reason, "Run /sandbox install, or see .", "See .", 1)
		st.UnavailableReason = &reason
	}
	st.StrictMode = m.StrictMode()
	st.StrictModeSource = sandboxSource(false, s.LockedKeys["allowUnsandboxedCommands"], st.StrictMode)

	// Windows is always strict; otherwise sandbox.filesystem.disabled
	// selects relaxed, defaulting to strict.
	st.FilesystemPolicy = "strict"
	if goos != "windows" && s.Filesystem.Disabled {
		st.FilesystemPolicy = "relaxed"
	}

	st.AutoAllowBashIfSandboxed = m.AutoAllowEnabled()
	switch {
	case !m.AutoAllowSupported():
		st.AutoAllowBashIfSandboxedSource = "unsupported"
	case s.LockedKeys["autoAllowBashIfSandboxed"]:
		st.AutoAllowBashIfSandboxedSource = "policy"
	case !s.AutoAllowSet:
		st.AutoAllowBashIfSandboxedSource = "default"
	default:
		st.AutoAllowBashIfSandboxedSource = "settings"
	}
	return st
}

// runSandboxCmd routes `gocode sandbox <install|status>`.
func runSandboxCmd(env *Env, t []string) int {
	args := argsAfter(t, "sandbox")
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb = args[0]
	}
	m := newSessionSandbox(env, nil, false)
	defer m.Close()
	switch verb {
	case "install":
		// {status:"unavailable", message} and exit 1 unless "ok".
		msg := sandboxWindowsGate(runtime.GOOS, m.PlatformListed())
		if msg == "" && m.Settings().LockedByPolicy() {
			msg = "Sandbox settings are locked by policy. Sandbox provisioning is managed by your administrator."
		}
		writeJSONLine(env.Stdout, struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}{"unavailable", msg})
		return 1
	case "status":
		writeJSONLine(env.Stdout, computeSandboxStatus(m, runtime.GOOS))
		return 0
	default:
		writeCommandUsage(env, "sandbox")
		return 1
	}
}
