package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// sandboxBoundaryText frames what the sandbox's limits mean for the task.
const sandboxBoundaryText = "The sandbox marks out what this session was given: the directories listed below, the network destinations the task involves, and the credentials the user supplied for it. Treat that as the boundary even where a limit below is not enforced. Commands can reach more than that — credentials and keys elsewhere on this machine, the user's other projects and configuration, sockets that control this machine or other workloads, cloud metadata endpoints — but being reachable does not make them provided; those are the user's, not the task's, unless the user's request calls for them. If the task cannot be finished with what you were given, do what you can and tell the user plainly what is missing instead of finding another way to it; that report is a complete answer."

// promptListLimit truncates long lists in the prompt.
const promptListLimit = 50

// truncateList keeps the first 50 and say how many were left out.
func truncateList(l []string) []string {
	if len(l) <= promptListLimit {
		return l
	}
	n := len(l) - promptListLimit
	return append(append([]string{}, l[:promptListLimit]...), fmt.Sprintf("... and %d more (truncated for prompt size)", n))
}

// compactJSON renders v like JSON.stringify (no HTML escaping).
func compactJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// PromptSection is the "## Bash command sandbox" guidance the model gets when
// Bash commands run sandboxed: what the sandbox confines in this session (the
// filesystem lists, denied hosts), when `dangerouslyDisableSandbox` may be
// used, and how to treat sandbox failures. "" when sandboxing is off.
func (m *Manager) PromptSection(interactive bool) string {
	if m == nil || !m.IsSandboxingEnabled() {
		return ""
	}
	rc := m.RuntimeConfig("")
	unsandboxedOK := m.UnsandboxedCommandsAllowed()
	tmp := map[string]bool{}
	if d := m.opts.Paths.TempDir; d != "" {
		tmp[d] = true
	}
	writeAllow := []string{}
	for _, p := range rc.AllowWrite {
		if tmp[p] {
			p = "$TMPDIR"
		}
		writeAllow = append(writeAllow, p)
	}
	type readCfg struct {
		DenyOnly        []string `json:"denyOnly"`
		AllowWithinDeny []string `json:"allowWithinDeny"`
	}
	type writeCfg struct {
		AllowOnly       []string `json:"allowOnly"`
		DenyWithinAllow []string `json:"denyWithinAllow"`
	}
	type fsCfg struct {
		Read  readCfg  `json:"read"`
		Write writeCfg `json:"write"`
	}
	nonNil := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}
	denyRead := rc.DenyRead
	if m.platform == "linux" && !rc.FilesystemDisabled {
		denyRead = append(append([]string{}, denyRead...), "/etc/ssh/ssh_config.d")
	}
	var filesystem string
	if rc.FilesystemDisabled {
		filesystem = "Filesystem: in this configuration the sandbox does not confine commands to particular directories for reading or writing. " +
			"Leave gocode's own settings, hooks, skills and plugin files, and the repository's git hooks and git config, unchanged unless the user asks; credential files and keys elsewhere on this machine are the user's, not the task's."
	} else {
		filesystem = "Filesystem: " + compactJSON(fsCfg{
			Read:  readCfg{DenyOnly: nonNil(truncateList(dedupe(denyRead))), AllowWithinDeny: nonNil(truncateList(rc.AllowRead))},
			Write: writeCfg{AllowOnly: nonNil(truncateList(dedupe(writeAllow))), DenyWithinAllow: nonNil(truncateList(rc.DenyWrite))},
		})
	}
	config := []string{filesystem}
	type netCfg struct {
		DeniedHosts      []string `json:"deniedHosts,omitempty"`
		AllowUnixSockets []string `json:"allowUnixSockets,omitempty"`
	}
	if n := (netCfg{DeniedHosts: truncateList(rc.DeniedDomains), AllowUnixSockets: truncateList(rc.AllowUnixSockets)}); len(n.DeniedHosts) > 0 || len(n.AllowUnixSockets) > 0 {
		config = append(config, "Network: "+compactJSON(n))
	}
	if len(rc.IgnoreViolations) > 0 {
		config = append(config, "Ignored violations: "+compactJSON(rc.IgnoreViolations))
	}

	explain := "Briefly explain what sandbox restriction likely caused the failure. Be sure to mention that the user can use the `/sandbox` command to manage restrictions."
	var items []any
	if unsandboxedOK {
		items = append(items,
			"You should always default to running commands within the sandbox. Do NOT attempt to set `dangerouslyDisableSandbox: true` unless:",
			[]string{
				"The user *explicitly* asks you to bypass sandbox",
				"A specific command just failed and you see evidence of sandbox restrictions causing the failure. Note that commands can fail for many reasons unrelated to the sandbox (missing files, wrong arguments, network issues, etc.).",
			},
			"Evidence of sandbox-caused failures includes:",
			[]string{
				`"Operation not permitted" errors for file/network operations`,
				"Access denied to specific paths outside allowed directories",
				"Network connection failures to non-whitelisted hosts",
				"Unix socket connection errors",
			},
			"When you see evidence of sandbox-caused failure:",
			[]string{
				"Retry with `dangerouslyDisableSandbox: true` directly rather than asking in prose first — the retry itself goes through the permission gate (a user prompt, or the auto-mode classifier when auto mode is active)",
				explain,
			},
			"A sandbox denial on a credential, a file or a host that the task does not involve is the boundary above at work: tell the user rather than retrying with `dangerouslyDisableSandbox: true`.",
			"Treat each command you execute with `dangerouslyDisableSandbox: true` individually. Even if you have recently run a command with this setting, you should default to running future commands within the sandbox.",
			"Do not suggest adding sensitive paths like ~/.bashrc, ~/.zshrc, ~/.ssh/*, or credential files to the sandbox allowlist.",
		)
	} else {
		items = append(items,
			"The `dangerouslyDisableSandbox` parameter is disabled in this session's configuration; setting it does not take a command out of the sandbox.",
			"If a command the task needs fails on a sandbox restriction, tell the user which restriction it hit; changing the sandbox settings is their decision, not yours.",
		)
	}
	items = append(items, "Network egress goes through a filtering proxy. Attempt requests and read the error rather than predicting whether a host is reachable.")
	if m.platform == "macos" && !rc.AllowLocalBinding {
		if unsandboxedOK {
			items = append(items, `If a command fails to bind or listen on a local port with "Operation not permitted" (EPERM), local port binding is off in this sandbox. Treat it as the sandbox-caused failure described above, and tell the user that `+"`sandbox.network.allowLocalBinding: true`"+` in their settings (it applies without a restart) allows it without leaving the sandbox.`)
		} else {
			items = append(items, `If a command fails to bind or listen on a local port with "Operation not permitted" (EPERM), local port binding is off in this sandbox. Tell the user they can allow it with `+"`sandbox.network.allowLocalBinding: true`"+` in their settings (it applies without a restart); changing sandbox settings is their decision, not yours.`)
		}
	}
	if m.platform == "macos" && !rc.AllowAppleEvents {
		items = append(items, "Opening an app, file or URL with `open`, or scripting another app with `osascript`, is blocked inside this sandbox (macOS Launch Services and Apple Events are off) and fails with errors such as -10822 (kLSServerCommunicationErr), -54, -600 or \"LSOpenURLsWithRole() failed\". That failure is the sandbox, not a problem with what you built; do not retry it with `dangerouslyDisableSandbox: true`. Tell the user the sandbox blocked it and give them the exact command to run themselves.")
	}
	if rc.FilesystemDisabled {
		items = append(items, "For temporary files, create a scratch directory with `mktemp -d` and reference it by absolute path. Do NOT assume `$TMPDIR` is set — the sandbox does not export it in this configuration.")
	} else {
		items = append(items, "For temporary files, always use the `$TMPDIR` environment variable. TMPDIR is automatically set to the correct sandbox-writable directory in sandbox mode. Do NOT use `/tmp` directly - use `$TMPDIR` instead.")
	}
	if interactive {
		items = append(items, "If a clipboard utility such as `pbcopy`, `xclip`, or `wl-copy` fails inside the sandbox and the user wants the text on their clipboard, put the text in a fenced code block in your response and tell them to run `/copy` (it copies from outside the sandbox; when the picker appears they can select just that block), rather than writing a file for them to copy manually.")
	}

	backend := ""
	switch m.platform {
	case "macos":
		backend = " (macOS Seatbelt)"
	case "linux":
		backend = " (Linux bubblewrap)"
	}
	lines := []string{
		"## Bash command sandbox",
		"By default, Bash commands run inside an OS-level sandbox" + backend + " applied to each command separately, not to the session as a whole; how it is configured in this session is described below.",
		"",
		sandboxBoundaryText,
		"",
		"How the sandbox is configured in this session:",
		strings.Join(config, "\n"),
		"",
	}
	for _, it := range items {
		switch v := it.(type) {
		case string:
			lines = append(lines, " - "+v)
		case []string:
			for _, s := range v {
				lines = append(lines, "  - "+s)
			}
		}
	}
	return strings.Join(lines, "\n")
}
