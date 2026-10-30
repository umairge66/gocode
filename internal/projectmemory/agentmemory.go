package projectmemory

import (
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/fsfacade"
)

// Per-agent memory: a custom agent definition with `memory: user|project|local`
// gets a persistent, file-based memory directory of its own, and its system
// prompt is extended with the "Persistent Agent Memory" section describing how
// to use it plus the current contents of the directory's MEMORY.md index.

// Agent memory scopes.
const (
	AgentMemoryUser    = "user"
	AgentMemoryProject = "project"
	AgentMemoryLocal   = "local"
)

// Agent-memory constants.
const (
	// agentMemoryDisplayName titles the section.
	agentMemoryDisplayName = "Persistent Agent Memory"
	// agentMemoryEntrypoint is the always-loaded index file.
	agentMemoryEntrypoint = "MEMORY.md"
	// agentMemoryMaxLines and agentMemoryMaxBytes cap the loaded index.
	agentMemoryMaxLines = 200
	agentMemoryMaxBytes = 25000
)

// AgentMemoryConfig locates one agent's memory.
type AgentMemoryConfig struct {
	// AgentType is the agent's name; it is sanitized into the directory name.
	AgentType string
	// Scope is "user", "project" or "local".
	Scope string
	// ConfigDir is the user config home (user scope: <ConfigDir>/agent-memory).
	ConfigDir string
	// Cwd is the project root (project / local scope).
	Cwd string
	// ExtraGuideline is appended to the scope guideline when non-blank.
	ExtraGuideline string
	// FS reads MEMORY.md. When nil, fsfacade.OSFS{} is used.
	FS fsfacade.FS
	// MkdirAll, when set, is called to create the memory directory before the
	// section is built. Errors are ignored.
	MkdirAll func(dir string) error
}

// ValidAgentMemoryScope reports whether s is a recognized scope.
func ValidAgentMemoryScope(s string) bool {
	return s == AgentMemoryUser || s == AgentMemoryProject || s == AgentMemoryLocal
}

// SanitizeAgentName maps an agent type to a directory name: every character
// outside [a-zA-Z0-9-_] becomes "-", and an empty result is "unknown".
func SanitizeAgentName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < utf8.RuneSelf && (r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')) {
			b.WriteRune(r)
			continue
		}
		// Replacement is per UTF-16 code unit; a character outside the BMP is
		// two units and so two dashes.
		if r > 0xFFFF {
			b.WriteString("--")
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// AgentMemoryDir returns the agent's memory directory, with a trailing
// separator, or "" for an unknown scope or a missing base directory.
func AgentMemoryDir(c AgentMemoryConfig) string {
	name := SanitizeAgentName(c.AgentType)
	var dir string
	switch c.Scope {
	case AgentMemoryProject:
		if c.Cwd == "" {
			return ""
		}
		dir = filepath.Join(c.Cwd, ".gocode", "agent-memory", name)
	case AgentMemoryLocal:
		if c.Cwd == "" {
			return ""
		}
		dir = filepath.Join(c.Cwd, ".gocode", "agent-memory-local", name)
	case AgentMemoryUser:
		if c.ConfigDir == "" {
			return ""
		}
		dir = filepath.Join(c.ConfigDir, "agent-memory", name)
	default:
		return ""
	}
	return dir + string(filepath.Separator)
}

// agentScopeGuideline is the scope-specific line.
func agentScopeGuideline(scope string) string {
	switch scope {
	case AgentMemoryUser:
		return "- Since this memory is user-scope, keep learnings general since they apply across all projects"
	case AgentMemoryProject:
		return "- Since this memory is project-scope and shared with your team via version control, tailor your memories to this project"
	case AgentMemoryLocal:
		return "- Since this memory is local-scope (not checked into version control), tailor your memories to this project and machine"
	}
	return ""
}

// AgentMemoryPrompt builds the "Persistent Agent Memory" section for one agent,
// or "" when the scope or directory cannot be resolved.
func AgentMemoryPrompt(c AgentMemoryConfig) string {
	dir := AgentMemoryDir(c)
	if dir == "" {
		return ""
	}
	fsys := c.FS
	if fsys == nil {
		fsys = fsfacade.OSFS{}
	}
	mode := entrypointReadMode(fsys, c.Scope, dir, c.Cwd)
	if mode != entrypointNone && c.MkdirAll != nil {
		_ = c.MkdirAll(strings.TrimRight(dir, `/\`))
	}
	guidelines := []string{agentScopeGuideline(c.Scope)}
	if strings.TrimSpace(c.ExtraGuideline) != "" {
		guidelines = append(guidelines, c.ExtraGuideline)
	}

	content, refused := readEntrypoint(fsys, dir+agentMemoryEntrypoint, mode)
	lines := memoryGuidelines(agentMemoryDisplayName, dir, guidelines, refused)
	switch {
	case strings.TrimSpace(content) != "":
		lines = append(lines, "## "+agentMemoryEntrypoint, "", sanitizeMemoryText(truncateEntrypoint(content)))
	case refused:
		lines = append(lines, "## "+agentMemoryEntrypoint, "", "Your "+agentMemoryEntrypoint+" was not loaded: it or its folder is a link or a special file, or could not be verified to be inside this working copy. Treat this agent memory as read-only in this session: do not create or write "+agentMemoryEntrypoint+" or other files in its folder.")
	default:
		lines = append(lines, "## "+agentMemoryEntrypoint, "", "Your "+agentMemoryEntrypoint+" is currently empty. When you save new memories, they will appear here.")
	}
	return strings.Join(lines, "\n")
}

// entrypointRead modes.
type entrypointRead int

const (
	entrypointFollow entrypointRead = iota
	entrypointRegularFile
	entrypointNone
)

// entrypointReadMode decides how MEMORY.md may be read. User-scope memory is
// followed through links; project and local memory must stay inside the
// working copy: when the directory resolves (through a link) to somewhere
// else, or cannot be resolved, MEMORY.md is not read at all, and otherwise it
// is read only when it is a regular file.
func entrypointReadMode(fsys fsfacade.FS, scope, dir, cwd string) entrypointRead {
	if scope == AgentMemoryUser {
		return entrypointFollow
	}
	d := normalizePath(dir)
	root := normalizePath(cwd)
	if !within(d, root) || d == root {
		return entrypointRegularFile
	}
	realRoot, err := fsfacade.Realpath(fsys, root)
	if err != nil {
		return entrypointNone
	}
	realRoot = normalizePath(realRoot)
	if r, err := fsfacade.Realpath(fsys, d); err == nil {
		if within(normalizePath(r), realRoot) {
			return entrypointRegularFile // "beneath"
		}
		return entrypointNone // "elsewhere"
	}
	if _, err := fsys.Lstat(d); err == nil {
		return entrypointNone // a dangling link
	}
	// Absent: resolve the deepest existing ancestor and re-append the rest.
	existing, missing := d, []string{}
	for {
		if _, err := fsys.Lstat(existing); err == nil {
			break
		}
		parent := dirOf(existing)
		if parent == existing {
			return entrypointNone
		}
		missing = append([]string{existing[len(strings.TrimSuffix(parent, "/"))+1:]}, missing...)
		existing = parent
	}
	r, err := fsfacade.Realpath(fsys, existing)
	if err != nil {
		return entrypointNone
	}
	full := normalizePath(strings.Join(append([]string{normalizePath(r)}, missing...), "/"))
	if within(full, realRoot) {
		return entrypointRegularFile // "absent"
	}
	return entrypointNone
}

// readEntrypoint reads MEMORY.md under mode. refused reports that the file was
// deliberately not read (the memory is then read-only for the session); a
// missing or unreadable file is simply empty.
func readEntrypoint(fsys fsfacade.FS, p string, mode entrypointRead) (content string, refused bool) {
	if mode == entrypointNone {
		return "", true
	}
	if mode == entrypointRegularFile {
		fi, err := fsys.Lstat(p)
		if err != nil {
			return "", false
		}
		if !fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0 {
			return "", true
		}
	}
	data, err := fsys.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(data), false
}

// truncateEntrypoint trims MEMORY.md and caps it at agentMemoryMaxLines lines
// and agentMemoryMaxBytes characters, appending a warning that says what was
// cut.
func truncateEntrypoint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	lineCount := strings.Count(trimmed, "\n") + 1
	byteCount := jsLen(trimmed)
	overLines := lineCount > agentMemoryMaxLines
	overBytes := byteCount > agentMemoryMaxBytes
	if !overLines && !overBytes {
		return trimmed
	}
	kept := trimmed
	if overLines {
		kept = strings.Join(strings.SplitN(trimmed, "\n", agentMemoryMaxLines+1)[:agentMemoryMaxLines], "\n")
	}
	if len(kept) > agentMemoryMaxBytes {
		if i := strings.LastIndex(kept[:agentMemoryMaxBytes], "\n"); i > 0 {
			kept = kept[:i]
		} else {
			kept = kept[:agentMemoryMaxBytes]
		}
	}
	cutLine := 0
	if len(kept) < len(trimmed) && trimmed[len(kept)] == '\n' {
		cutLine = strings.Count(kept, "\n") + 1
	}
	next := ""
	if start := len(kept) + 1; start < len(trimmed) {
		rest := trimmed[start:]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[:i]
		}
		next = strings.TrimSpace(rest)
	}
	var what string
	if cutLine == 0 {
		what = "everything after the first " + strconv.Itoa(len(kept)) + " characters of line 1 was cut off"
	} else {
		what = strconv.Itoa(lineCount-cutLine) + " of " + strconv.Itoa(lineCount) + " lines were cut off, starting at line " + strconv.Itoa(cutLine+1)
		if next != "" {
			what += " (\"" + truncateRunes(next, 80) + "\")"
		}
	}
	var size string
	switch {
	case overBytes && !overLines:
		size = formatKB(byteCount) + " (limit: " + formatKB(agentMemoryMaxBytes) + ") — index entries are too long"
	case overLines && !overBytes:
		size = strconv.Itoa(lineCount) + " lines (limit: " + strconv.Itoa(agentMemoryMaxLines) + ")"
	default:
		size = strconv.Itoa(lineCount) + " lines and " + formatKB(byteCount)
	}
	warning := agentMemoryEntrypoint + " is " + size + ". Only part of it was loaded: " + what + ". Keep index entries to one line under ~200 chars; move detail into topic files."
	return kept + "\n\n> WARNING: " + warning
}

// jsLen is the length of s in UTF-16 code units.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// truncateRunes shortens s to at most n characters, marking a cut with "…".
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// formatKB formats a byte count the way the guidelines quote sizes:
// "N bytes" below 1 KiB, else one decimal KB/MB with a trailing ".0" dropped.
func formatKB(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + " bytes"
	}
	f := func(v float64, unit string) string {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + unit
	}
	kb := float64(n) / 1024
	if kb < 1024 {
		return f(kb, "KB")
	}
	return f(kb/1024, "MB")
}

// sanitizeMemoryText drops invisible format and control characters (keeping
// tab and newline) from memory text before it reaches the prompt.

func sanitizeMemoryText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cs, r) {
			return -1
		}
		return r
	}, s)
}

// memoryGuidelines builds the guideline text of a memory section.
func memoryGuidelines(displayName, dir string, extra []string, readOnly bool) []string {
	access := "This directory already exists — write to it directly with the Write tool (do not run mkdir or check for its existence)."
	if readOnly {
		access = "This memory is read-only this session — you cannot save new memories to it."
	}
	out := []string{"# " + displayName, "", "You have a persistent, file-based memory system at `" + dir + "`. " + access, ""}
	if !readOnly {
		out = append(out,
			"You should build up this memory system over time so that future conversations can have a complete picture of who the user is, how they'd like to collaborate with you, what behaviors to avoid or repeat, and the context behind the work the user gives you.",
			"",
			"If the user explicitly asks you to remember something, save it immediately as whichever type fits best. If they ask you to forget something, find and remove the relevant entry.",
			"")
	}
	out = append(out, agentMemoryTypes...)
	out = append(out, memoryNotToSave...)
	out = append(out, "")
	if !readOnly {
		out = append(out, memorySaving()...)
		out = append(out, "")
	}
	out = append(out, memoryWhenToAccess...)
	out = append(out, "")
	out = append(out, memoryBeforeRecommending...)
	out = append(out, "")
	out = append(out, memoryOtherPersistence...)
	out = append(out, "")
	out = append(out, extra...)
	out = append(out, "")
	return out
}

// memorySaving is the two-step save procedure with the frontmatter template
// and its link note.
func memorySaving() []string {
	e := agentMemoryEntrypoint
	out := []string{
		"## How to save memories",
		"",
		"Saving a memory is a two-step process:",
		"",
		"**Step 1** — write the memory to its own file (e.g., `user_role.md`, `feedback_testing.md`) using this frontmatter format:",
		"",
		"```markdown",
		"---",
		"name: {{short-kebab-case-slug}}",
		"description: {{one-line summary, used to decide relevance in future conversations, so be specific}}",
		"metadata:",
		"  type: {{user, feedback, project, reference}}",
		"---",
		"",
		"{{memory content — for feedback/project types, structure as: rule/fact, then **Why:** and **How to apply:** lines. Link related memories with [[their-name]].}}",
		"```",
		"",
		"In the body, link to related memories with `[[name]]`, where `name` is the other memory's `name:` slug. Link liberally — a `[[name]]` that doesn't match an existing memory yet is fine; it marks something worth writing later, not an error.",
		"",
		"**Step 2** — add a pointer to that file in `" + e + "`. `" + e + "` is an index, not a memory — each entry should be one line, under ~150 characters: `- [Title](file.md) — one-line hook`. It has no frontmatter. Never write memory content directly into `" + e + "`.",
		"",
		"- `" + e + "` is always loaded into your conversation context — lines after " + strconv.Itoa(agentMemoryMaxLines) + " will be truncated, so keep the index concise",
	}
	return append(out, memoryUpkeep...)
}

// memoryUpkeep.
var memoryUpkeep = []string{
	"- Keep the name, description, and type fields in memory files up-to-date with the content",
	"- Organize memory semantically by topic, not chronologically",
	"- Update or remove memories that turn out to be wrong or outdated",
	"- Do not write duplicate memories. First check if there is an existing memory you can update before writing a new one.",
}

// memoryNotToSave.
var memoryNotToSave = []string{
	"## What NOT to save in memory",
	"",
	"- Code patterns, conventions, architecture, file paths, or project structure — these can be derived by reading the current project state.",
	"- Git history, recent changes, or who-changed-what — `git log` / `git blame` are authoritative.",
	"- Debugging solutions or fix recipes — the fix is in the code; the commit message has the context.",
	"- Anything already documented in gocode.md files.",
	"- Ephemeral task details: in-progress work, temporary state, current conversation context.",
	"",
	"These exclusions apply even when the user explicitly asks you to save. If they ask you to save a PR list or activity summary, ask what was *surprising* or *non-obvious* about it — that is the part worth keeping.",
}

// memoryWhenToAccess.
var memoryWhenToAccess = []string{
	"## When to access memories",
	"- When memories seem relevant, or the user references prior-conversation work.",
	"- You MUST access memory when the user explicitly asks you to check, recall, or remember.",
	"- If the user says to *ignore* or *not use* memory: Do not apply remembered facts, cite, compare against, or mention memory content.",
	"- Memory records can become stale over time. Use memory as context for what was true at a given point in time. Before answering the user or building assumptions based solely on information in memory records, verify that the memory is still correct and up-to-date by reading the current state of the files or resources. If a recalled memory conflicts with current information, trust what you observe now — and update or remove the stale memory rather than acting on it.",
}

// memoryBeforeRecommending.
var memoryBeforeRecommending = []string{
	"## Before recommending from memory",
	"",
	"A memory that names a specific function, file, or flag is a claim that it existed *when the memory was written*. It may have been renamed, removed, or never merged. Before recommending it:",
	"",
	"- If the memory names a file path: check the file exists.",
	"- If the memory names a function or flag: grep for it.",
	"- If the user is about to act on your recommendation (not just asking about history), verify first.",
	"",
	"\"The memory says X exists\" is not the same as \"X exists now.\"",
	"",
	"A memory that summarizes repo state (activity logs, architecture snapshots) is frozen in time. If the user asks about *recent* or *current* state, prefer `git log` or reading the code over recalling the snapshot.",
}

// memoryOtherPersistence closes the guideline text.
var memoryOtherPersistence = []string{
	"## Memory and other forms of persistence",
	"Memory is one of several persistence mechanisms available to you as you assist the user in a given conversation. The distinction is often that memory can be recalled in future conversations and should not be used for persisting information that is only useful within the scope of the current conversation.",
	"- When to use or update a plan instead of memory: If you are about to start a non-trivial implementation task and would like to reach alignment with the user on your approach you should use a Plan rather than saving this information to memory. Similarly, if you already have a plan within the conversation and you have changed your approach persist that change by updating the plan rather than saving a memory.",
	"- When to use or update tasks instead of memory: When you need to break your work in current conversation into discrete steps or keep track of your progress use tasks instead of saving to memory. Tasks are great for persisting information about the work that needs to be done in the current conversation, but memory should be reserved for information that will be useful in future conversations.",
}

// agentMemoryTypes is the inline memory-type catalog.
var agentMemoryTypes = []string{
	"## Types of memory",
	"",
	"There are several discrete types of memory that you can store in your memory system:",
	"",
	"<types>",
	"<type>",
	"    <name>user</name>",
	"    <description>Contain information about the user's role, goals, responsibilities, and knowledge. Great user memories help you tailor your future behavior to the user's preferences and perspective. Your goal in reading and writing these memories is to build up an understanding of who the user is and how you can be most helpful to them specifically. For example, you should collaborate with a senior software engineer differently than a student who is coding for the very first time. Keep in mind, that the aim here is to be helpful to the user. Avoid writing memories about the user that could be viewed as a negative judgement or that are not relevant to the work you're trying to accomplish together.</description>",
	"    <when_to_save>When you learn any details about the user's role, preferences, responsibilities, or knowledge</when_to_save>",
	"    <how_to_use>When your work should be informed by the user's profile or perspective. For example, if the user is asking you to explain a part of the code, you should answer that question in a way that is tailored to the specific details that they will find most valuable or that helps them build their mental model in relation to domain knowledge they already have.</how_to_use>",
	"    <examples>",
	"    user: I'm a data scientist investigating what logging we have in place",
	"    assistant: [saves user memory: user is a data scientist, currently focused on observability/logging]",
	"",
	"    user: I've been writing Go for ten years but this is my first time touching the React side of this repo",
	"    assistant: [saves user memory: deep Go expertise, new to React and this project's frontend — frame frontend explanations in terms of backend analogues]",
	"    </examples>",
	"</type>",
	"<type>",
	"    <name>feedback</name>",
	"    <description>Guidance the user has given you about how to approach work — both what to avoid and what to keep doing. These are a very important type of memory to read and write as they allow you to remain coherent and responsive to the way you should approach work in the project. Record from failure AND success: if you only save corrections, you will avoid past mistakes but drift away from approaches the user has already validated, and may grow overly cautious.</description>",
	"    <when_to_save>Any time the user corrects your approach (\"no not that\", \"don't\", \"stop doing X\") OR confirms a non-obvious approach worked (\"yes exactly\", \"perfect, keep doing that\", accepting an unusual choice without pushback). Corrections are easy to notice; confirmations are quieter — watch for them. In both cases, save what is applicable to future conversations, especially if surprising or not obvious from the code. Include *why* so you can judge edge cases later.</when_to_save>",
	"    <how_to_use>Let these memories guide your behavior so that the user does not need to offer the same guidance twice.</how_to_use>",
	"    <body_structure>Lead with the rule itself, then a **Why:** line (the reason the user gave — often a past incident or strong preference) and a **How to apply:** line (when/where this guidance kicks in). Knowing *why* lets you judge edge cases instead of blindly following the rule.</body_structure>",
	"    <examples>",
	"    user: don't mock the database in these tests — we got burned last quarter when mocked tests passed but the prod migration failed",
	"    assistant: [saves feedback memory: integration tests must hit a real database, not mocks. Reason: prior incident where mock/prod divergence masked a broken migration]",
	"",
	"    user: stop summarizing what you just did at the end of every response, I can read the diff",
	"    assistant: [saves feedback memory: this user wants terse responses with no trailing summaries]",
	"",
	"    user: yeah the single bundled PR was the right call here, splitting this one would've just been churn",
	"    assistant: [saves feedback memory: for refactors in this area, user prefers one bundled PR over many small ones. Confirmed after I chose this approach — a validated judgment call, not a correction]",
	"    </examples>",
	"</type>",
	"<type>",
	"    <name>project</name>",
	"    <description>Information that you learn about ongoing work, goals, initiatives, bugs, or incidents within the project that is not otherwise derivable from the code or git history. Project memories help you understand the broader context and motivation behind the work the user is doing within this working directory.</description>",
	"    <when_to_save>When you learn who is doing what, why, or by when. These states change relatively quickly so try to keep your understanding of this up to date. Always convert relative dates in user messages to absolute dates when saving (e.g., \"Thursday\" → \"2026-03-05\"), so the memory remains interpretable after time passes.</when_to_save>",
	"    <how_to_use>Use these memories to more fully understand the details and nuance behind the user's request and make better informed suggestions.</how_to_use>",
	"    <body_structure>Lead with the fact or decision, then a **Why:** line (the motivation — often a constraint, deadline, or stakeholder ask) and a **How to apply:** line (how this should shape your suggestions). Project memories decay fast, so the why helps future-you judge whether the memory is still load-bearing.</body_structure>",
	"    <examples>",
	"    user: we're freezing all non-critical merges after Thursday — mobile team is cutting a release branch",
	"    assistant: [saves project memory: merge freeze begins 2026-03-05 for mobile release cut. Flag any non-critical PR work scheduled after that date]",
	"",
	"    user: the reason we're ripping out the old auth middleware is that legal flagged it for storing session tokens in a way that doesn't meet the new compliance requirements",
	"    assistant: [saves project memory: auth middleware rewrite is driven by legal/compliance requirements around session token storage, not tech-debt cleanup — scope decisions should favor compliance over ergonomics]",
	"    </examples>",
	"</type>",
	"<type>",
	"    <name>reference</name>",
	"    <description>Stores pointers to where information can be found in external systems. These memories allow you to remember where to look to find up-to-date information outside of the project directory.</description>",
	"    <when_to_save>When you learn about resources in external systems and their purpose. For example, that bugs are tracked in a specific project in Linear or that feedback can be found in a specific Slack channel.</when_to_save>",
	"    <how_to_use>When the user references an external system or information that may be in an external system.</how_to_use>",
	"    <examples>",
	"    user: check the Linear project \"INGEST\" if you want context on these tickets, that's where we track all pipeline bugs",
	"    assistant: [saves reference memory: pipeline bugs are tracked in Linear project \"INGEST\"]",
	"",
	"    user: the Grafana board at grafana.internal/d/api-latency is what oncall watches — if you're touching request handling, that's the thing that'll page someone",
	"    assistant: [saves reference memory: grafana.internal/d/api-latency is the oncall latency dashboard — check it when editing request-path code]",
	"    </examples>",
	"</type>",
	"</types>",
	"",
}
