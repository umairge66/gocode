package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/auth"
	"github.com/umairge66/gocode/internal/daemon"
	"github.com/umairge66/gocode/internal/envsec"
)

// Provider keys and background sessions.
//
// A background session never receives the dispatching shell's provider key
// (bgcmd.go bgDispatchEnv): it resolves its key the normal way — the daemon's
// own environment, auth.json written by /login, a settings key — and, when it
// finds none at start, stops at once with bgNoKeyMessage instead of sitting
// idle on a placeholder caller. `gocode --bg` warns up front when the caller's
// key exists only in its own environment (bgEnvOnlyKeyWarning).

// bgNoKeyMessage is the fail-fast text of a background session with no key.
func bgNoKeyMessage(provider string) string {
	return fmt.Sprintf("No API key for %s in the background session — run /login (saves to auth.json) or set it in the environment that starts the daemon", provider)
}

// isBgWorker reports whether this process is a daemon-backed background
// session (GOCODE_CODE_SESSION_KIND=bg).
func isBgWorker(env *Env) bool {
	return envGetenv(env)("GOCODE_CODE_SESSION_KIND") == "bg"
}

// bgWorkerMissingKey returns the fail-fast message when this process is a
// background session whose provider key could not be resolved, else "".
func bgWorkerMissingKey(env *Env, s *printSession) string {
	if s == nil || !isBgWorker(env) || s.APIKey != "" {
		return ""
	}
	return bgNoKeyMessage(sessionProviderName(s))
}

// bgEnvOnlyKeyWarning returns the up-front `gocode --bg` warning when the
// caller has a key for the session's provider only in its environment — no
// auth.json (or settings) key the background session could use — else "".
// The key's value is never printed. flags are the forwarded session flags (a
// --provider among them selects the provider).
func bgEnvOnlyKeyWarning(env *Env, flags []string) string {
	getenv := envGetenv(env)
	d, _ := parseDispatchDefaults(flags)
	p, err := resolveProvider(env, d)
	if err != nil {
		return ""
	}
	envName := ""
	for _, n := range auth.ProviderEnvVars(auth.Provider(p)) {
		if strings.TrimSpace(getenv(n)) != "" {
			envName = n
			break
		}
	}
	if envName == "" {
		return ""
	}
	// Resolve again with every provider-key variable hidden: what the
	// background session can find without the caller's environment.
	r := newKeyResolver(env)
	r.Getenv = func(k string) string {
		if envsec.IsProviderKeyEnvName(k) || envsec.IsSecretEnvName(k) {
			return ""
		}
		return getenv(k)
	}
	if _, _, ok := r.ResolveProviderKey(auth.Provider(p)); ok {
		return ""
	}
	return fmt.Sprintf("warning: %s is set only in this shell and is not passed to background sessions; auth.json has no %s key. "+
		"Unless the daemon's own environment has it, the session will stop with \"No API key for %s\" — run /login (saves to auth.json) or set it in the environment that starts the daemon.",
		envName, p, p)
}

// failStart settles the background job as failed before the session ran
// (state "failed", the message as detail) and records it in the timeline.
// A nil receiver does nothing.
func (j *jobStatus) failStart(msg string) {
	if j == nil {
		return
	}
	detail := daemon.TruncateDetail(collapse(msg))
	var at string
	j.update(func(st *daemon.JobState) bool {
		at = j.stamp()
		st.State, st.Detail, st.Tempo, st.Needs, st.InFlight = "failed", detail, "idle", "", nil
		st.Output = json.RawMessage("null")
		st.UpdatedAt = at
		if st.FirstTerminalAt == nil {
			st.FirstTerminalAt = &at
		}
		return true
	})
	if at != "" {
		j.timeline(at, "failed", detail, detail)
	}
}
