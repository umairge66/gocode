package systemprompt

import (
	"strings"
	"testing"
)

func sampleInfo() Info {
	return Info{
		Cwd:             "/home/dev/project",
		IsGitRepo:       true,
		GitBranch:       "main",
		Platform:        "linux",
		Shell:           "zsh",
		OSVersion:       "Linux 6.1.0",
		Date:            "2026-10-23",
		Model:           "anthropic/claude-opus-4",
		ModelName:       "Claude Opus 4",
		KnowledgeCutoff: "January 2026",
		Provider:        "openrouter",
	}
}

func TestBuildContainsIdentityAndGuidance(t *testing.T) {
	out := Build(sampleInfo())

	for _, want := range []string{
		"You are gocode, Gocode's official CLI",               // identity
		"working with the user toward their goals",            // role
		"defensive security",                                  // security posture
		"# System",                                            // system rules
		"# Doing tasks",                                       // conventions
		"Prefer editing existing files to creating new ones.", // code convention
		"# Executing actions with care",                       // safety
		"# Using your tools",                                  // tool-use guidance
		"make all independent tool calls in parallel",         // parallelism
		"Use TodoWrite to plan and track work",                // task management
		"# Tone and style",                                    // tone
		"file_path:line_number",                               // code references
		"# Delivering work",                                   // delivery posture
		"# Corrections",                                       // corrections
		"# Being proactive",                                   // proactiveness
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Build output missing %q", want)
		}
	}
}

func TestBuildEnvBlockReflectsInfo(t *testing.T) {
	out := Build(sampleInfo())

	for _, want := range []string{
		"# Environment",
		"You have been invoked in the following environment:",
		" - Primary working directory: /home/dev/project",
		" - Is a git repository: true",
		" - Current branch: main",
		" - Platform: linux",
		" - Shell: zsh",
		" - OS Version: Linux 6.1.0",
		"You are powered by the model named Claude Opus 4. The exact model ID is anthropic/claude-opus-4.",
		"Assistant knowledge cutoff is January 2026.",
		"The active provider is openrouter.",
		"Today's date is 2026-10-23.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("env block missing %q\n---\n%s", want, out)
		}
	}
}

func TestBuildNonGitRepo(t *testing.T) {
	info := sampleInfo()
	info.IsGitRepo = false
	info.GitBranch = ""
	out := Build(info)
	if !strings.Contains(out, " - Is a git repository: false") {
		t.Errorf("expected git-repo false row")
	}
	if strings.Contains(out, "Current branch:") {
		t.Errorf("branch row should be omitted when GitBranch is empty")
	}
}

func TestBuildBareModelLine(t *testing.T) {
	info := sampleInfo()
	info.ModelName = ""
	info.KnowledgeCutoff = ""
	out := Build(info)
	if !strings.Contains(out, "You are powered by the model anthropic/claude-opus-4.") {
		t.Errorf("bare model line missing")
	}
	if strings.Contains(out, "knowledge cutoff") {
		t.Errorf("cutoff sentence should be omitted when empty")
	}
}

// TestBuildNoTelemetryOrAccountText guards the gocode directives: no telemetry,
// no feedback-upload pointer, no OAuth / account / login lines.
func TestBuildNoTelemetryOrAccountText(t *testing.T) {
	out := strings.ToLower(Build(sampleInfo()))
	for _, forbidden := range []string{
		"telemetry",
		"github.com/umairge66/gocode/issues",
		"report the issue",
		"give feedback",
		"oauth",
		"log in",
		"login",
		"sign in",
		"account email",
		"useremail",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("Build output must not contain %q", forbidden)
		}
	}
}

func TestBuildIsPure(t *testing.T) {
	info := sampleInfo()
	if Build(info) != Build(info) {
		t.Errorf("Build is not deterministic for the same Info")
	}
}

func TestBuildMinimalInfo(t *testing.T) {
	// An empty Info must still produce the identity + guidance and not panic.
	out := Build(Info{})
	if !strings.Contains(out, "You are gocode") {
		t.Errorf("identity missing for empty Info")
	}
	if strings.Contains(out, "Primary working directory:") {
		t.Errorf("directory rows should be omitted when Cwd is empty")
	}
}

func TestBuildHeadlessDropsChatOnlyGuidance(t *testing.T) {
	info := sampleInfo()
	interactive := Build(info)
	info.Headless = true
	headless := Build(info)

	for _, gone := range []string{
		"# Corrections",
		"Assume users can't see most tool calls",
		"For exploratory questions",
		"Reserve blocking questions",
		"repeats or reaffirms",
	} {
		if !strings.Contains(interactive, gone) {
			t.Errorf("interactive prompt missing %q", gone)
		}
		if strings.Contains(headless, gone) {
			t.Errorf("headless prompt still contains %q", gone)
		}
	}
	for _, kept := range []string{
		"Your final message is the result of the run: state what you did and what you verified, briefly.",
		"Finish the whole task, not just easy parts",
		"The requested scope is the deliverable",
		"report what you verified",
		actionsSection,
		securityGuidance,
		urlCaution,
		doingTasksRest,
		"# Being proactive",
		"For scripts longer than a few lines",
	} {
		if !strings.Contains(headless, kept) {
			t.Errorf("headless prompt missing %q", kept)
		}
	}
	if len(headless) >= len(interactive) {
		t.Errorf("headless prompt (%d chars) not shorter than interactive (%d)", len(headless), len(interactive))
	}
}

func TestBuildConditionalSystemBullets(t *testing.T) {
	const prompted = "the user will be prompted so that they can approve or deny"
	const retry = "do not re-attempt the exact same tool call"
	info := sampleInfo()

	base := Build(info)
	for _, s := range []string{prompted, retry, permissionModeSentence} {
		if !strings.Contains(base, s) {
			t.Errorf("default mode missing %q", s)
		}
	}
	if strings.Contains(base, "untrusted-web-content") || strings.Contains(base, "configure 'hooks'") {
		t.Errorf("browser/hooks bullets present without browser tools or hooks")
	}

	info.SkipPermissions = true
	bypass := Build(info)
	if strings.Contains(bypass, prompted) || strings.Contains(bypass, retry) {
		t.Errorf("bypass mode still describes approval prompts")
	}
	if !strings.Contains(bypass, " - "+permissionModeSentence+"\n") {
		t.Errorf("bypass mode lost the permission-mode sentence")
	}
	info.SkipPermissions = false

	info.BrowserTools = true
	if got := Build(info); !strings.Contains(got, systemBrowserBullet) {
		t.Errorf("browser bullet missing with browser tools")
	}
	info.BrowserTools = false

	info.Hooks = true
	withHooks := Build(info)
	if !strings.Contains(withHooks, systemHooksBullet) {
		t.Errorf("hooks bullet missing with hooks configured")
	}
	if strings.Replace(withHooks, systemHooksBullet+"\n", "", 1) != base {
		t.Errorf("hooks bullet is not the only difference")
	}
}

func TestBuildScriptFileGuidanceInAllModes(t *testing.T) {
	const want = "write them to a file with Write once, then edit and re-run that file"
	for _, headless := range []bool{false, true} {
		info := sampleInfo()
		info.Headless = headless
		if !strings.Contains(Build(info), want) {
			t.Errorf("headless=%v: script-file guidance missing", headless)
		}
	}
}
