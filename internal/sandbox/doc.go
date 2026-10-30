// Package sandbox implements the OS sandboxing subsystem used to confine
// tool / shell command execution — the machinery behind the
// `--sandbox` flag and the SandboxManager.wrapWithSandbox / wrapWithSandboxArgv
// entry points.
//
// COMPONENTS:
//
//   - the macOS seatbelt profile generator; emits the
//     "(version 1) (deny default ...)" S-expression policy.
//   - the macOS argv builder; wraps a shell command as
//     `env -u … NAME=VAL … /usr/bin/sandbox-exec -p <profile> <shell> -c <cmd>`.
//   - the Linux argv builder; wraps a command with bubblewrap
//     (`bwrap --new-session --die-with-parent … --unshare-net --unshare-pid
//     --unshare-user … -- <shell> -c <cmd>`).
//   - the file-read / file-write clause builders that turn the resolved
//     allow/deny path sets into seatbelt clauses.
//   - the shared readConfig / writeConfig normalizers used by both the
//     macOS and Linux builders.
//
// PLATFORMS. Three back-ends exist in principle: macOS (sandbox-exec /
// "seatbelt"), Linux (bubblewrap + socat, plus an optional seccomp Unix-socket
// filter from the @gocode-ai/sandbox-runtime helper), and Windows (a WFP
// egress fence + filesystem ACLs provisioned by `npx sandbox-runtime
// windows-install`). This package implements macOS and Linux; Windows reports
// the sandbox as unsupported, and the seccomp Unix-socket filter is not
// shipped (a "seccomp not available" warning is reported instead).
//
// RUNTIME. Manager (manager.go) is the session's SandboxManager: settings.go
// resolves the `sandbox` settings object across the settings sources (with
// the policy / trusted-tier rules), config.go derives the per-command
// confinement (writable roots, write-protected and read-denied paths, network
// lists), ShouldSandbox decides per Bash command (dangerouslyDisableSandbox,
// excludedCommands, strict mode), and Wrap renders the bwrap / sandbox-exec
// argv. The network is always confined: a sandboxed command reaches only the
// in-process filtering proxy (proxy.go: HTTP CONNECT/forward + SOCKS5 on one
// 127.0.0.1 port, allowedDomains / deniedDomains with wildcard and port
// matching, a resolved-address check), on Linux through a Unix socket bound
// into bubblewrap and bridged to localhost:3128/1080 inside by socat, on macOS
// through the localhost ports the seatbelt profile opens. The proxy lives for
// the session and forwards only what a sandboxed command itself requests.
//
// RELATIONSHIP TO pathguard / envsec. The sandbox profile can also be
// GENERATED from the same resolved permission rules that internal/pathguard
// evaluates: the read policy is an allow-by-default denylist
// (pathguard.ModeAllowAllExcept over filesystem.denyRead) and the write policy
// is a deny-by-default allowlist (pathguard.ModeAllowOnly over
// filesystem.allowWrite, with filesystem.denyWrite as DenyWithinAllow
// carve-outs). FromRuleSets bridges those engines into a Profile. Profile.
// ApplyEnv layers the Profile's SetEnv / UnsetEnv over an environment
// sanitized by internal/envsec.
//
// SPAWNING. The renderers return the profile string and the argv slice only.
// The Manager probes the installed bwrap (`--version`, `--help`) and starts
// the proxy; the Bash tool (internal/tools/exec) spawns the wrapped argv.
//
// HARDENING:
//
//   - envstrip.go: credential-looking variables (provider keys, cloud and
//     forge tokens, *_SECRET / *_TOKEN / *_PASSWORD …) are unset for every
//     sandboxed command; sandbox.credentials.passThroughEnvVars (trusted
//     tiers only) re-admits named ones.
//   - ipc.go: host IPC endpoints (runtime dir, D-Bus, agents, docker,
//     tmux/X11, this session's peer / daemon / browser sockets) are hidden
//     from the bwrap view and their env vars unset — a substitute for a
//     seccomp unix-socket filter. Directory-level only
//     (temp dirs under an empty tmpfs with the writable roots / allowRead /
//     allowUnixSockets bound back; socket-holding dirs shadowed with
//     --ro-bind-try), so socket churn on the host never aborts bwrap. Trade-
//     off: other programs' files in /tmp are not visible inside.
//   - config.go / linux.go: every writable root is protected like the cwd;
//     the whole config home and the home rc files are protected; a missing
//     .git is protected (no repository with hooks can be created in a
//     writable root); the nested scan goes 8 levels deep; a protected path
//     reached through a symlink inside a writable root refuses the command.
//   - placeholder_unix.go: placeholders are created and removed without
//     following symlinks and removed only while they are the same inode.
//   - proxy.go / domains.go: hosts are canonicalized once (case, trailing
//     dot, IDNA, legacy IPv4 spellings) and that string is matched and
//     dialled; IP literals and names resolving to loopback / private /
//     link-local / metadata addresses need an explicit allowlist entry.
//
// RESIDUAL: a dangerous name created during a command at a location that did
// not exist when the command started (e.g. a new nested repository's hooks)
// cannot be blocked by mount placement; it is found by the next command's
// scan, but the first command can still write it.
package sandbox
