package agentsdef

// StatuslineSetupPrompt returns the statusline-setup agent's system prompt.
func StatuslineSetupPrompt() string { return statuslineSetupPrompt }

// Built-in agent types. These ship with the app at SourceBuiltin (lowest
// precedence: any same-named user/project/plugin/policy agent overrides them).
//
// Which built-ins are offered depends on runtime/session gates (statusline
// setup, Explore/Plan, web-fetch only when WebFetch is on, gocode-guide outside
// SDK entrypoints); BuiltinAgents returns the full set and leaves gating to the
// caller. The agent-view background-job agent is not included: there is no
// agent view or job-list classifier here.
//
// Tool references use the tool-name strings (the same identifiers the
// Agent tool and internal/protocol use, e.g. "Read", "Edit", "WebFetch",
// "Agent"). A nil Tools slice means "inherit the default tool set".

// Built-in agent type names.
const (
	AgentGeneralPurpose  = "general-purpose"
	AgentExplore         = "Explore"
	AgentPlan            = "Plan"
	AgentStatuslineSetup = "statusline-setup"
	AgentGocodeGuide     = "gocode-guide"
	AgentWebFetch        = "web-fetch"
)

// BuiltinAgents returns the built-in agent descriptors.
//
// The long system prompts of some built-ins are assembled at runtime from
// several fragments and session state. Where the prompt is static and
// self-contained it is included; where it is runtime-assembled the field is
// left empty with a note and only the stable metadata (type, description,
// tools, model, color) is carried.
func BuiltinAgents() []Descriptor {
	return []Descriptor{
		{
			AgentType:    AgentGeneralPurpose,
			WhenToUse:    "General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks. When you are searching for a keyword or file and are not confident that you will find the right match in the first few tries use this agent to perform the search for you.",
			Tools:        nil, // inherit all
			SystemPrompt: generalPurposePrompt,
			Source:       SourceBuiltin,
			BaseDir:      "built-in",
		},
		{
			AgentType:       AgentExplore,
			WhenToUse:       `Fast read-only search agent for locating code. Use it to find files by pattern (eg. "src/components/**/*.tsx"), grep for symbols or keywords (eg. "API endpoints"), or answer "where is X defined / which files reference Y." Do NOT use it for code review, design-doc auditing, cross-file consistency checks, or open-ended analysis — it reads excerpts rather than whole files and will miss content past its read window. When calling, specify search breadth: "quick" for a single targeted lookup, "medium" for moderate exploration, or "very thorough" to search across multiple locations and naming conventions.`,
			DisallowedTools: writeToolKit,
			Model:           "inherit",
			OmitGocodeMd:    true,
			Source:          SourceBuiltin,
			BaseDir:         "built-in",
		},
		{
			// Shares Explore's tool restrictions.
			AgentType:       AgentPlan,
			WhenToUse:       "Software architect agent for designing implementation plans. Use this when you need to plan the implementation strategy for a task. Returns step-by-step plans, identifies critical files, and considers architectural trade-offs.",
			DisallowedTools: writeToolKit,
			Model:           "inherit",
			OmitGocodeMd:    true,
			Source:          SourceBuiltin,
			BaseDir:         "built-in",
		},
		{
			AgentType:    AgentStatuslineSetup,
			WhenToUse:    "Use this agent to configure the user's gocode status line setting.",
			Tools:        []string{"Read", "Edit"},
			Model:        "sonnet",
			Color:        "orange",
			SystemPrompt: statuslineSetupPrompt,
			Source:       SourceBuiltin,
			BaseDir:      "built-in",
		},
		{
			AgentType:      AgentGocodeGuide,
			WhenToUse:      `Use this agent when the user asks questions ("Can gocode...", "Does gocode...", "How do I...") about: (1) gocode (the CLI tool); (2) gocode Agent SDK; (3) gocode API; (4) gocode Tag (gocode in Slack); (5) 'gocode plugin eval' and the '/skill-doctor' report. IMPORTANT: Before spawning a new agent, check if there is already a running or recently completed gocode-guide agent that you can continue.`,
			Model:          "haiku",
			PermissionMode: "dontAsk",
			Source:         SourceBuiltin,
			BaseDir:        "built-in",
			// SystemPrompt omitted: assembled at runtime from the user's
			// configured commands, agents, MCP servers and settings keys.
		},
		{
			// Added only when the WebFetch tool is enabled.
			AgentType:    AgentWebFetch,
			WhenToUse:    "Use this to fetch and read web pages / URLs when you do not have a direct WebFetch tool of your own. Put the full URL(s) in the prompt along with the question or task itself; its report is what enters your context, so it should already be the answer.",
			Tools:        []string{"WebFetch"},
			Color:        "blue",
			OmitGocodeMd: true,
			Source:       SourceBuiltin,
			BaseDir:      "built-in",
			// Model is chosen dynamically at runtime.
		},
	}
}

// writeToolKit is the set of state-changing tools Explore and Plan disallow.
// The Artifact tools are not implemented; listing them is harmless.
var writeToolKit = []string{
	"Agent",
	"Artifact", "ArtifactComments", "ArtifactData", "ArtifactCheck",
	"ExitPlanMode", "Edit", "Write", "NotebookEdit",
}

// generalPurposePrompt is the static system prompt of the general-purpose agent.
const generalPurposePrompt = `You are an agent for gocode, Gocode's official CLI for gocode. Given the user's message, you should use the tools available to complete the task. Complete the task fully—don't gold-plate, but don't leave it half-done. When you complete the task, respond with a concise report covering what was done and any key findings — the caller will relay this to the user, so it only needs the essentials.

Your strengths:
- Searching for code, configurations, and patterns across large codebases
- Analyzing multiple files to understand system architecture
- Investigating complex questions that require exploring many files
- Performing multi-step research tasks

Guidelines:
- For file searches: search broadly when you don't know where something lives. Use Read when you know the specific file path.
- For analysis: Start broad and narrow down. Use multiple search strategies if the first doesn't yield results.
- Be thorough: Check multiple locations, consider different naming conventions, look for related files.
- NEVER create files unless they're absolutely necessary for achieving your goal. ALWAYS prefer editing an existing file to creating a new one.
- NEVER proactively create documentation files (*.md) or README files. Only create documentation files if explicitly requested.
- You are already the dedicated agent for this task. Do the work directly — do not re-delegate your entire assignment to another single subagent.`
