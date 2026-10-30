package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/envsec"
)

// HTTPDoer is the minimal HTTP client an http hook needs. *http.Client
// satisfies it; tests inject a fake. No default client is created implicitly —
// the caller must supply one, so an offline or telemetry-free build simply
// leaves it nil and http hooks report a non-blocking error.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// HTTPResult is the raw outcome of an http hook POST.
type HTTPResult struct {
	StatusCode int
	Body       string
	// OK is true for a 2xx response.
	OK bool
	// Aborted is true when the context deadline/cancel fired.
	Aborted bool
	// Duration is the wall-clock time of the request.
	Duration time.Duration
}

// HTTPRunner executes http hooks: it POSTs the event payload as JSON to the
// hook's URL and interprets the JSON response body as a decision, exactly as a
// command hook's stdout is interpreted. It sends only to the user-configured
// URL and emits no telemetry (no sandbox-proxy, diskless or cloud-session
// paths).
type HTTPRunner struct {
	// Doer performs the request. Nil makes every http hook a non-blocking error
	// ("no HTTP client"), keeping offline builds safe.
	Doer HTTPDoer
	// DefaultTimeout applies when the hook sets no timeout. Zero means no
	// timeout beyond the caller's context.
	DefaultTimeout time.Duration
	// EnvLookup resolves a variable name for header interpolation. Nil disables
	// interpolation.
	EnvLookup func(name string) (string, bool)
}

// Run POSTs payload to the http hook's URL and returns the interpreted
// Decision. The error is non-nil only for a payload-marshal failure; transport
// and status failures are reported through the Decision (ErrKind) and
// HTTPResult; the runner never fails out on them.
func (r *HTTPRunner) Run(ctx context.Context, hook CommandHook, payload Payload, opts DecisionOptions) (Decision, HTTPResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Decision{}, HTTPResult{}, err
	}
	if r.Doer == nil {
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrSpawnFailed, Err: "no HTTP client configured for http hook",
		}, HTTPResult{}, nil
	}

	timeout := r.DefaultTimeout
	if hook.TimeoutSeconds > 0 {
		timeout = time.Duration(hook.TimeoutSeconds) * time.Second
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrSpawnFailed, Err: err.Error(),
		}, HTTPResult{}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	allowed := toSet(hook.AllowedEnvVars)
	for k, v := range hook.Headers {
		req.Header.Set(k, r.interpolate(v, allowed))
	}

	start := time.Now()
	resp, err := r.Doer.Do(req)
	dur := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			return Decision{
				Behavior: BehaviorNone, Continue: true,
				ErrKind: ErrTimeout, Err: "http hook timed out",
			}, HTTPResult{Aborted: true, Duration: dur}, nil
		}
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrExit, Err: err.Error(),
		}, HTTPResult{Duration: dur}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res := HTTPResult{
		StatusCode: resp.StatusCode,
		Body:       string(raw),
		OK:         resp.StatusCode >= 200 && resp.StatusCode < 300,
		Duration:   dur,
	}
	if !res.OK {
		return Decision{
			Behavior: BehaviorNone, Continue: true,
			ErrKind: ErrExit,
			Err:     fmt.Sprintf("HTTP %d from %s", res.StatusCode, hook.URL),
		}, res, nil
	}
	return BodyDecision(res.Body, opts), res, nil
}

// interpolate replaces ${VAR} in a header value with the environment value when
// VAR is whitelisted. Unknown or
// non-whitelisted variables are left blank.
func (r *HTTPRunner) interpolate(value string, allowed map[string]bool) string {
	if !strings.Contains(value, "${") || r.EnvLookup == nil {
		return value
	}
	var b strings.Builder
	for {
		i := strings.Index(value, "${")
		if i < 0 {
			b.WriteString(value)
			break
		}
		j := strings.IndexByte(value[i:], '}')
		if j < 0 {
			b.WriteString(value)
			break
		}
		b.WriteString(value[:i])
		name := value[i+2 : i+j]
		if allowed[name] && !credentialHeaderVar(name) {
			if v, ok := r.EnvLookup(name); ok {
				b.WriteString(v)
			}
		}
		value = value[i+j+1:]
	}
	// Header values never carry CR/LF/NUL.
	return strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' || c == 0 {
			return -1
		}
		return c
	}, b.String())
}

// credentialHeaderVar reports whether an env var is a credential that is never
// interpolated into an HTTP hook header, even when the hook lists it in
// allowedEnvVars: a hook (which a cloned repository's settings can declare)
// could otherwise post the user's provider API key to any URL.
func credentialHeaderVar(name string) bool { return envsec.IsSecretEnvName(name) }
