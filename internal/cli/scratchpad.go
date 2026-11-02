package cli

import (
	"path/filepath"
	"strings"

	"github.com/umairge66/gocode/internal/envutil"
	"github.com/umairge66/gocode/internal/pathguard"
	"github.com/umairge66/gocode/internal/sandbox"
	"github.com/umairge66/gocode/internal/sessionstore"
	"github.com/umairge66/gocode/internal/subagent"
)

// scratchpadDisableEnv turns the session scratchpad off when truthy.
const scratchpadDisableEnv = "GOCODE_DISABLE_SCRATCHPAD"

// sessionScratchpad creates and returns the session's scratchpad directory:
// <temp root>/<escaped cwd>/<session id>/scratchpad, where the temp root is
// the per-user directory the sandbox already lets commands write
// (sandbox.TempRoot: $TMPDIR/gocode-<uid>).
//
// The temp root must be a private directory owned by this user (it lives
// under the shared temp directory, where another user could have created the
// name first); everything below it is created 0700. A diskless session, or
// one with GOCODE_DISABLE_SCRATCHPAD set, has none ("" and a nil error); a
// directory that cannot be used returns "" and the reason.
func sessionScratchpad(getenv func(string) string, cwd, sessionID string, diskless bool) (string, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if diskless || envutil.EnvTruthy(getenv(scratchpadDisableEnv)) || strings.TrimSpace(sessionID) == "" || cwd == "" {
		return "", nil
	}
	if strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return "", nil
	}
	root := sandbox.TempRoot(getenv)
	if err := sandbox.EnsurePrivateDir(root); err != nil {
		return "", err
	}
	dir := filepath.Join(root, sessionstore.EscapeCwd(cwd), sessionID, "scratchpad")
	if err := sandbox.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// scratchpadNotice is the one-line startup notice when the scratchpad
// cannot be used.
func scratchpadNotice(err error) string {
	return "No scratchpad for this session: " + err.Error() + ". Temporary files outside the project will ask for permission."
}

// scratchpadPromptSection tells the model about the session scratchpad.
func scratchpadPromptSection(dir string) string {
	if dir == "" {
		return ""
	}
	return "# Scratchpad\n\n" +
		"For temporary files (throwaway scripts, intermediate output) use `" + dir + "`, not /tmp or the project. " +
		"Writes there need no permission prompt. It is private to this session and temporary; give full paths when naming its files."
}

// withScratchpad sets the session file checks' scratchpad directory.
func withScratchpad(f *pathguard.SessionFiles, dir string) *pathguard.SessionFiles {
	if f != nil {
		f.ScratchpadDir = dir
	}
	return f
}

// withScratchpadSection adds the scratchpad section to what memoryFor gives a
// subagent's prompt, so subagents use the session scratchpad too.
func withScratchpadSection(memoryFor func(subagent.AgentType) string, dir string) func(subagent.AgentType) string {
	sec := scratchpadPromptSection(dir)
	if sec == "" || memoryFor == nil {
		return memoryFor
	}
	return func(a subagent.AgentType) string {
		if m := memoryFor(a); m != "" {
			return m + "\n\n" + sec
		}
		return sec
	}
}
