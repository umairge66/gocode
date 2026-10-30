package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file is the automatic retry-with-backoff policy the ModelCaller applies
// to every model request (CallModel and CallModelStream). A transient failure —
// a 429 rate limit, a 529 / overloaded_error, a 5xx, a 408 / 409, or a
// connection-level error — is retried with exponential backoff (or the
// provider's Retry-After), instead of failing the whole turn. Billing / auth /
// invalid-request / not-found failures and a user interrupt are never retried.
//
// Policy:
//
//	max retries         10
//	env override        GOCODE_CODE_MAX_RETRIES → GOCODE_MAX_RETRIES
//	env clamp           values > 15 clamp to 15
//	base delay          500ms
//	factor              2
//	cap                 32s
//	jitter              proportional, + [0, 25%] of the delay
//	Retry-After         delay = max(Retry-After, backoff)
//	too-long ceiling    a delay over 60s is not waited out:
//	                    the request gives up instead
//	retryable           408, 409, 429 (non-billing), >= 500 (incl. 529),
//	                    connection errors, x-should-retry: true
//	never retried       x-should-retry: false, billing / quota, 401 / 403,
//	                    400, 404, user interrupt
//
// Besides the integer-seconds Retry-After, `retry-after-ms` and an HTTP-date
// Retry-After are honored too (the official SDK's own parsing).
//
// No telemetry: a retry only calls the local OnRetry hook (status line /
// stderr) and records nothing.

const (
	// DefaultMaxRetries is the default number of retries after the first attempt.
	DefaultMaxRetries = 10
	// MaxRetriesCeiling is the clamp applied to a GOCODE_MAX_RETRIES override.
	MaxRetriesCeiling = 15
	// EnvMaxRetries is the environment override for the retry count ("0"
	// disables retries).
	EnvMaxRetries = "GOCODE_MAX_RETRIES"

	retryBaseDelay   = 500 * time.Millisecond
	retryFactor      = 2.0
	retryMaxBackoff  = 32 * time.Second
	retryJitterRatio = 0.25
	// retryMaxWait is the longest delay the caller will wait out before a
	// retry; a Retry-After beyond it makes the request give up immediately.
	retryMaxWait = 60 * time.Second
)

// MaxRetriesFromEnv resolves the retry count from GOCODE_MAX_RETRIES via
// getenv (nil → os.Getenv): a non-negative integer is used (clamped to
// MaxRetriesCeiling), anything else falls back to DefaultMaxRetries.
func MaxRetriesFromEnv(getenv func(string) string) int {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := strings.TrimSpace(getenv(EnvMaxRetries))
	if raw == "" {
		return DefaultMaxRetries
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return DefaultMaxRetries
	}
	if n > MaxRetriesCeiling {
		return MaxRetriesCeiling
	}
	return n
}

// RetriesExhaustedError is returned when every retry failed. It wraps the last
// failure, so errors.As still finds the *APIError (and its Category) for the
// category-specific suggestion.
type RetriesExhaustedError struct {
	Retries int
	Err     error
}

func (e *RetriesExhaustedError) Error() string {
	prefix := ""
	if isOverloaded(e.Err) {
		prefix = "Repeated 529 Overloaded errors: "
	}
	noun := "retries"
	if e.Retries == 1 {
		noun = "retry"
	}
	return fmt.Sprintf("%s%s (gave up after %d %s)", prefix, e.Err.Error(), e.Retries, noun)
}

func (e *RetriesExhaustedError) Unwrap() error { return e.Err }

// isRetryable reports whether err is a transient failure worth retrying.
func isRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Category {
		case CategoryCredits, CategoryAuth:
			return false
		}
		switch ae.ShouldRetry {
		case "true":
			return true
		case "false":
			return false
		}
		return ae.Retryable
	}
	// A plain (unclassified) error: retry only a connection-level failure — a
	// stream cut before any output (unexpected EOF, reset, broken pipe) or a
	// net.Error timeout.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// isOverloaded reports whether err is an overload (HTTP 529 / overloaded_error).
func isOverloaded(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == 529 {
		return true
	}
	hay := strings.ToLower(ae.Detail)
	if u := ae.Unwrap(); u != nil {
		hay += " " + strings.ToLower(u.Error())
	}
	return strings.Contains(hay, "overloaded")
}

// serverRetryDelay returns the delay the provider asked for (retry-after-ms,
// then Retry-After as seconds or an HTTP-date), or ok=false when none.
func serverRetryDelay(err error, now time.Time) (time.Duration, bool) {
	var ae *APIError
	if !errors.As(err, &ae) {
		return 0, false
	}
	if ms := strings.TrimSpace(ae.RetryAfterMs); ms != "" {
		if f, perr := strconv.ParseFloat(ms, 64); perr == nil && f >= 0 && !math.IsInf(f, 0) {
			return time.Duration(f * float64(time.Millisecond)), true
		}
	}
	ra := strings.TrimSpace(ae.RetryAfter)
	if ra == "" {
		return 0, false
	}
	if f, perr := strconv.ParseFloat(ra, 64); perr == nil && f >= 0 && !math.IsInf(f, 0) {
		return time.Duration(f * float64(time.Second)), true
	}
	if t, perr := http.ParseTime(ra); perr == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// backoffDelay is the exponential backoff for retry number attempt
// (1-based): min(cap, base·2^(attempt-1)) plus a proportional jitter of up to
// 25% of that (r is a uniform [0,1) sample).
func backoffDelay(attempt int, r float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(retryBaseDelay) * math.Pow(retryFactor, float64(attempt-1))
	if d > float64(retryMaxBackoff) {
		d = float64(retryMaxBackoff)
	}
	return time.Duration(d + r*retryJitterRatio*d)
}

// retryDelay decides the wait before retry number attempt: the larger of the
// server-requested delay and the backoff. ok=false means the requested delay
// exceeds retryMaxWait, so the caller should give up rather than wait.
func retryDelay(err error, attempt int, r float64, now time.Time) (time.Duration, bool) {
	d := backoffDelay(attempt, r)
	if sd, has := serverRetryDelay(err, now); has && sd > d {
		d = sd
	}
	if d > retryMaxWait {
		return d, false
	}
	return d, true
}

// sleepCtx waits d or until ctx is done, returning ctx.Err() on cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// withRetry runs attempt until it succeeds, fails non-retryably, or the retry
// budget is spent. attempt reports (noRetry, err): when noRetry is
// true the failure is returned as-is regardless of its class (a stream that
// already delivered output). The effort-drop retry happens inside attempt and
// is not counted here.
func (m *ModelCaller) withRetry(ctx context.Context, attempt func() (noRetry bool, err error)) error {
	max := m.MaxRetries
	if max < 0 {
		max = 0
	}
	sleep := m.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	rnd := m.Rand
	if rnd == nil {
		rnd = rand.Float64
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}
	fb := &fallbackState{model: m.CurrentModel(), fallback: m.FallbackModel()}
	for n := 0; ; n++ {
		noRetry, err := attempt()
		if err == nil {
			return nil
		}
		// An armed fallback model short-circuits the retries of an
		// overloaded or unavailable model (fallback.go). A stream that
		// already showed output is never re-issued.
		if !noRetry && ctx.Err() == nil {
			if ferr := fb.check(err); ferr != nil {
				return ferr
			}
		}
		if noRetry || ctx.Err() != nil || !isRetryable(err) {
			if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
				return ctx.Err()
			}
			return err
		}
		if n >= max {
			// Out of retries: an armed fallback takes over (fallback.go).
			if ferr := fb.exhausted(err); ferr != nil {
				return ferr
			}
			if n == 0 {
				return err // retries disabled: the plain error
			}
			return &RetriesExhaustedError{Retries: n, Err: err}
		}
		retry := n + 1
		delay, ok := retryDelay(err, retry, rnd(), now())
		if !ok {
			// The provider asked for a wait beyond the ceiling: give up now
			// rather than block the turn (retry_after_too_long), or move to
			// an armed fallback model at once.
			if ferr := fb.exhausted(err); ferr != nil {
				return ferr
			}
			return err
		}
		if m.OnRetry != nil {
			m.OnRetry(retry, max, delay, err)
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.OnRetryStart != nil {
			m.OnRetryStart(retry, max)
		}
	}
}

// RetryLabel is a short, user-facing description of a retryable failure, used
// in the "Retrying in…" status line and the -p stderr note.
func RetryLabel(err error) string {
	if isOverloaded(err) {
		return "API overloaded"
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Category == CategoryRateLimit:
			return "Rate limited"
		case ae.Category == CategoryNetwork:
			return "Connection error"
		case ae.Status == http.StatusRequestTimeout:
			return "Request timed out"
		case ae.Status >= 500:
			return fmt.Sprintf("Server error (HTTP %d)", ae.Status)
		}
		return "API error"
	}
	return "Connection error"
}

// RetryStatusText renders the interactive status-line message shown while a
// retry waits. The format is:
//
//	"<label> · Retrying in <delay> · attempt <n>/<max>"
//
// where label is the generic "API error" for the first attempts, and the
// specific failure once attempt >= min(3, max) or for a connection / rate-limit
// failure.
func RetryStatusText(attempt, max int, delay time.Duration, err error) string {
	label := "API error"
	specific := attempt >= min(3, max)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Category == CategoryNetwork || ae.Category == CategoryRateLimit {
		specific = true
	}
	if specific {
		label = RetryLabel(err)
	}
	return fmt.Sprintf("%s · Retrying in %s · attempt %d/%d", label, FormatRetryDelay(delay), attempt, max)
}

// FormatRetryDelay renders a delay for the retry line: whole seconds rounded
// up ("4s"), minutes past 60s
// ("1m 5s").
func FormatRetryDelay(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m, r := s/60, s%60
	if r == 0 {
		return strconv.Itoa(m) + "m"
	}
	return fmt.Sprintf("%dm %ds", m, r)
}
