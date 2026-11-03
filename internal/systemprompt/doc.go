// Package systemprompt builds the main agent's system prompt for the gocode
// CLI: the product identity and role, the standing behavior guidance (system
// rules, task conventions, action safety, tool use, tone, communication,
// delivering work, corrections, proactiveness), and a runtime environment
// block filled per request (working directory, git state, platform/OS, the
// active model + provider, and today's date).
//
// Build is a pure function: every value that varies at runtime is injected
// through Info, so the assembly is deterministic and offline-testable. The
// package collects no telemetry and performs no I/O; the caller (internal/cli)
// resolves the environment values and passes them in.
//
// # Sections
//
// The prompt is emitted as these sections, held as constants:
//
//   - intro
//   - "# System"
//   - "# Doing tasks"
//   - "# Executing actions with care"
//   - "# Using your tools" (with gocode's real tool names)
//   - "# Tone and style"
//   - "# Text output" communication rules
//   - "# Delivering work"
//   - "# Corrections"
//   - "# Being proactive"
//   - "# Environment" (the field block, the model line and the date line)
//
// # Session-dependent content
//
// "# System" carries the browser-content guidance only when the browser tools
// are offered, the hooks guidance only when hooks are configured, and the
// approval-prompt guidance only when tool calls can prompt (not in bypass
// permissions mode).
//
// A headless (non-interactive) session has nobody reading progress updates or
// answering questions: "# Text output" shrinks to a line about the final
// message, "# Delivering work" drops the parts about asking the user, and
// "# Corrections" and the exploratory-question bullet are left out. The
// safety, security and task-quality sections are the same in every mode.
//
// # Provider-key setup
//
// gocode uses provider API keys (OpenRouter / Anthropic / OpenAI), has no
// telemetry, and no gocode OAuth / gateway / account. Accordingly:
//
//   - "# Doing tasks" carries no feedback block: no "/help" line and no GitHub
//     feedback/issue-upload pointer is emitted.
//   - No account/email/login/OAuth line is emitted.
//   - The env block adds an explicit provider line (documented on envBlock)
//     naming the active provider and stating gocode uses provider API keys.
//
// Not included: a "lean" prompt variant, output-style variants
// (Proactive/Concise/…), model-identity lines, focus mode, background-session,
// surface, brief, autonomy-append, and a session-context (userEmail /
// attachedProject / gitStatus) attachment block. --system-prompt and
// --append-system-prompt are applied by the caller (internal/cli
// promptflags.go), which replaces or extends what Build returns.
package systemprompt
