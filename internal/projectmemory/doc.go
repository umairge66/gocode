// Package projectmemory discovers and renders the user's gocode.md project and
// user memory into a single text block that the system-prompt assembler can
// inject.
//
// # What it does
//
// Load walks the filesystem for the set of memory files, reads them (bounded
// in size), resolves their recursive @path imports (with a cycle guard and a
// depth cap), dedups repeats, and renders each source as a
// "Contents of <path>:" block in discovery order. The result is a single
// string plus the list of files read.
//
// The package is offline: all filesystem access goes through an
// injected fsfacade.FS (fsfacade.MemFS in tests, fsfacade.OSFS in production),
// so it never touches the real disk or network on its own, collects no
// telemetry, and treats file contents strictly as data.
//
// # Discovery and format
//
//   - Project filenames are "gocode.md", ".gocode/gocode.md" and
//     "gocode.local.md". Each file renders as `Contents of ${path}:\n\n${content}`,
//     and sources are deduped by path and by trimmed content.
//   - Import paths resolve "~/" against home, absolute paths as-is, and
//     relative paths against the importing file's directory. Imports match
//     /(?:^|\s)@((?:[^\s\\]|\\ )+)/g, nest at most 5 deep, and a
//     processed-path set guards against cycles. Each file is capped at
//     4 MiB.
//   - Memory kinds are User, Project, Local, Managed and AutoMem. User memory
//     is <ConfigDir>/gocode.md; the project walk goes from the filesystem root
//     down to cwd, so outer ancestors come first.
//
// # Behaviour notes
//
//   - Imports are found by stripping fenced code blocks (``` and ~~~) before
//     scanning for @imports; there is no full Markdown tokenizer, so inline
//     `code span` exclusion and an HTML-comment-only rule are not applied.
//     See parseImports.
//   - Rules directories (<ManagedDir>/.gocode/rules, <ConfigDir>/rules and
//     <dir>/.gocode/rules on the project walk) are loaded recursively (*.md),
//     after the tier's gocode.md files and before gocode.local.md (see
//     rules.go). Every memory file has its frontmatter stripped; a `paths:`
//     frontmatter makes a rule conditional: it is left out of the startup
//     block and injected by Nested once a matching file is touched (see
//     nested.go).
//   - Per-agent memory (an agent definition's `memory: user|project|local`)
//     is built by AgentMemoryPrompt (see agentmemory.go).
//   - Not supported: managed-policy injected gocodeMd, AutoMem pinned memory,
//     AGENTS.md fallback/merge modes, an external-include approval flow (links
//     leading out of the working directory are simply not followed outside the
//     User tier), per-type "Contents of" suffixes, --add-dir memory
//     directories, setting-source gates and a gocodeMdExcludes glob matcher.
//     The Managed tier is an optional <ManagedDir>/gocode.md plus its rules
//     directory.
//   - Besides the 4 MiB per-file cap, a total cap (DefaultMaxTotalBytes)
//     bounds the rendered block; set Config.MaxTotalBytes to a negative value
//     to disable it.
package projectmemory
