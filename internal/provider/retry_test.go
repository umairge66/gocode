package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/agent"
)

// fakeSleep records every backoff wait without sleeping; it honors ctx.
type fakeSleep struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeSleep) sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.mu.Unlock()
	return ctx.Err()
}

func (f *fakeSleep) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

type retryCall struct {
	attempt, max int
	delay        time.Duration
	err          error
}

// retrying wires the test seams (fake sleep, zero jitter) and an OnRetry
// recorder onto mc.
func retrying(mc *ModelCaller, max int) (*fakeSleep, func() []retryCall) {
	fs := &fakeSleep{}
	var (
		mu    sync.Mutex
		calls []retryCall
	)
	mc.MaxRetries = max
	mc.Sleep = fs.sleep
	mc.Rand = func() float64 { return 0 }
	mc.OnRetry = func(a, m int, d time.Duration, err error) {
		mu.Lock()
		calls = append(calls, retryCall{a, m, d, err})
		mu.Unlock()
	}
	return fs, func() []retryCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]retryCall(nil), calls...)
	}
}

func orCaller(url string) *ModelCaller {
	return NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: url}, "m")
}

// 429 → 200: one retry, Retry-After honored, OnRetry sees attempt/max/delay.
func TestRetryRateLimitHonorsRetryAfter(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		if n == 0 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`)
			return
		}
		io.WriteString(w, oaiOK)
	})
	mc := orCaller(srv.URL)
	fs, calls := retrying(mc, 10)
	resp, err := mc.CallModel(context.Background(), userReq())
	if err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if n := len(got()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	if d := fs.got(); len(d) != 1 || d[0] != 3*time.Second {
		t.Fatalf("delays = %v, want [3s] (Retry-After)", d)
	}
	c := calls()
	if len(c) != 1 || c[0].attempt != 1 || c[0].max != 10 || c[0].delay != 3*time.Second {
		t.Fatalf("OnRetry calls = %+v", c)
	}
	var ae *APIError
	if !errors.As(c[0].err, &ae) || ae.Category != CategoryRateLimit {
		t.Fatalf("OnRetry err = %v, want rate-limit APIError", c[0].err)
	}
}

// 529 overloaded → success (Anthropic), with the 500ms first backoff.
func TestRetryOverloadedThenSuccess(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		if n == 0 {
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		io.WriteString(w, antOK)
	})
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "m")
	fs, calls := retrying(mc, 10)
	if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	if n := len(got()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	if d := fs.got(); len(d) != 1 || d[0] != 500*time.Millisecond {
		t.Fatalf("delays = %v, want [500ms]", d)
	}
	if c := calls(); len(c) != 1 || !strings.HasPrefix(RetryLabel(c[0].err), "API overloaded") {
		t.Fatalf("calls = %+v", c)
	}
}

// 500 × (max+1) → gives up after exactly max retries with exponential delays.
func TestRetryGivesUpAfterMax(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom","type":"api_error"}}`)
	})
	mc := orCaller(srv.URL)
	fs, _ := retrying(mc, 3)
	_, err := mc.CallModel(context.Background(), userReq())
	if err == nil {
		t.Fatal("want error")
	}
	if n := len(got()); n != 4 {
		t.Fatalf("requests = %d, want 4 (1 + 3 retries)", n)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	d := fs.got()
	if len(d) != 3 || d[0] != want[0] || d[1] != want[1] || d[2] != want[2] {
		t.Fatalf("delays = %v, want %v", d, want)
	}
	if !strings.Contains(err.Error(), "gave up after 3 retries") {
		t.Fatalf("err = %q, want gave-up message", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Category != CategoryServer {
		t.Fatalf("err lost its APIError: %v", err)
	}
	var re *RetriesExhaustedError
	if !errors.As(err, &re) || re.Retries != 3 {
		t.Fatalf("err = %#v, want RetriesExhaustedError{3}", err)
	}
}

// Billing, auth, invalid request and not-found failures are never retried.
func TestRetryNeverForNonTransient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"402-billing", 402, `{"error":{"message":"Your credit balance is too low","type":"billing_error"}}`},
		{"429-billing", 429, `{"error":{"message":"You exceeded your current quota.","code":"insufficient_quota"}}`},
		{"401-auth", 401, `{"error":{"message":"invalid key","type":"authentication_error"}}`},
		{"403-auth", 403, `{"error":{"message":"forbidden","type":"permission_error"}}`},
		{"400-invalid", 400, `{"error":{"message":"bad request","type":"invalid_request_error"}}`},
		{"404-model", 404, `{"error":{"message":"model not found","type":"not_found_error"}}`},
		{"500-should-retry-false", 500, `{"error":{"message":"no"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
				if strings.Contains(tc.name, "should-retry") {
					w.Header().Set("x-should-retry", "false")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			mc := orCaller(srv.URL)
			fs, calls := retrying(mc, 10)
			_, err := mc.CallModel(context.Background(), userReq())
			if err == nil {
				t.Fatal("want error")
			}
			if n := len(got()); n != 1 {
				t.Fatalf("requests = %d, want 1 (no retry)", n)
			}
			if len(fs.got()) != 0 || len(calls()) != 0 {
				t.Fatalf("unexpected backoff/OnRetry")
			}
			if strings.Contains(err.Error(), "gave up") {
				t.Fatalf("err = %q, should not mention retries", err)
			}
		})
	}
}

// effort_unsupported is retried once without effort (not counted), then the
// normal policy applies to a following transient failure.
func TestRetryComposesWithEffortDrop(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		switch n {
		case 0:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support the effort parameter."}}`)
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"unavailable"}}`)
		default:
			io.WriteString(w, antOK)
		}
	})
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "claude-old")
	mc.SetEffort("high")
	fs, calls := retrying(mc, 10)
	if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
		t.Fatalf("CallModel: %v", err)
	}
	reqs := got()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	if _, has := reqs[2].body["output_config"]; has {
		t.Fatalf("effort re-sent after the drop: %s", reqs[2].raw)
	}
	if len(fs.got()) != 1 || len(calls()) != 1 || calls()[0].attempt != 1 {
		t.Fatalf("delays=%v calls=%+v, want exactly one counted retry", fs.got(), calls())
	}
}

// Cancelling the context during a (real) backoff wait returns promptly.
func TestRetryContextCancelDuringBackoff(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})
	mc := orCaller(srv.URL)
	mc.MaxRetries = 10
	mc.Sleep = nil // the real ctx-aware timer
	started := make(chan struct{}, 1)
	mc.OnRetry = func(int, int, time.Duration, error) { started <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	t0 := time.Now()
	_, err := mc.CallModel(ctx, userReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(t0); el > 5*time.Second {
		t.Fatalf("took %v, want prompt return", el)
	}
	if n := len(got()); n != 1 {
		t.Fatalf("requests = %d, want 1", n)
	}
}

// flakyDoer fails the first n requests with err, then delegates.
type flakyDoer struct {
	mu   sync.Mutex
	n    int
	err  error
	next Doer
	hits int
}

func (d *flakyDoer) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.hits++
	fail := d.hits <= d.n
	d.mu.Unlock()
	if fail {
		return nil, d.err
	}
	return d.next.Do(r)
}

// Connection refused, and a server that closes the connection, are retried.
func TestRetryNetworkErrors(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		srv, _ := recordingServer(t, func(w http.ResponseWriter, n int) { io.WriteString(w, oaiOK) })
		fd := &flakyDoer{n: 2, err: syscall.ECONNREFUSED, next: http.DefaultClient}
		mc := NewModelCaller(&OpenRouter{APIKey: "k", BaseURL: srv.URL, HTTP: fd}, "m")
		fs, _ := retrying(mc, 10)
		if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
			t.Fatalf("CallModel: %v", err)
		}
		if len(fs.got()) != 2 {
			t.Fatalf("delays = %v, want 2 retries", fs.got())
		}
	})
	t.Run("closed", func(t *testing.T) {
		srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
			if n == 0 {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("no hijacker")
					return
				}
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
			io.WriteString(w, oaiOK)
		})
		mc := orCaller(srv.URL)
		fs, _ := retrying(mc, 10)
		if _, err := mc.CallModel(context.Background(), userReq()); err != nil {
			t.Fatalf("CallModel: %v", err)
		}
		if len(got()) < 2 || len(fs.got()) < 1 {
			t.Fatalf("requests=%d delays=%v, want a retry", len(got()), fs.got())
		}
	})
}

func streamReq(onText func(string)) *agent.StreamHandler {
	return &agent.StreamHandler{OnText: onText}
}

// A streaming failure BEFORE the first delta is retried; output is not
// duplicated.
func TestRetryStreamBeforeFirstDelta(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
			if n == 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":{"message":"down"}}`)
				return
			}
			io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
			io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		})
		mc := orCaller(srv.URL)
		retrying(mc, 10)
		var deltas []string
		resp, err := mc.CallModelStream(context.Background(), userReq(), streamReq(func(s string) { deltas = append(deltas, s) }))
		if err != nil {
			t.Fatalf("CallModelStream: %v", err)
		}
		if len(got()) != 2 || strings.Join(deltas, "") != "hello" || resp.Message.Content[0].Text != "hello" {
			t.Fatalf("requests=%d deltas=%q resp=%+v", len(got()), deltas, resp)
		}
	})
	t.Run("overloaded-event", func(t *testing.T) {
		srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
			antSSE(w, "message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}`)
			if n == 0 {
				antSSE(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
				return
			}
			antSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
			antSSE(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
			antSSE(w, "message_stop", `{"type":"message_stop"}`)
		})
		mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "m")
		retrying(mc, 10)
		var deltas []string
		if _, err := mc.CallModelStream(context.Background(), userReq(), streamReq(func(s string) { deltas = append(deltas, s) })); err != nil {
			t.Fatalf("CallModelStream: %v", err)
		}
		if len(got()) != 2 || strings.Join(deltas, "") != "hi" {
			t.Fatalf("requests=%d deltas=%q", len(got()), deltas)
		}
	})
}

// A streaming failure AFTER a delta reached the handler is NOT retried.
func TestRetryStreamAfterDeltaNotRetried(t *testing.T) {
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		antSSE(w, "message_start", `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}`)
		antSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		antSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)
		antSSE(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})
	mc := NewModelCaller(&Anthropic{APIKey: "k", BaseURL: srv.URL}, "m")
	fs, _ := retrying(mc, 10)
	var deltas []string
	_, err := mc.CallModelStream(context.Background(), userReq(), streamReq(func(s string) { deltas = append(deltas, s) }))
	if err == nil {
		t.Fatal("want error")
	}
	if len(got()) != 1 || len(fs.got()) != 0 || strings.Join(deltas, "") != "partial" {
		t.Fatalf("requests=%d delays=%v deltas=%q, want no retry", len(got()), fs.got(), deltas)
	}
}

// GOCODE_MAX_RETRIES=0 disables retries; other values are parsed and clamped.
func TestMaxRetriesEnv(t *testing.T) {
	t.Setenv(EnvMaxRetries, "0")
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	mc := orCaller(srv.URL)
	if mc.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0 from env", mc.MaxRetries)
	}
	_, err := mc.CallModel(context.Background(), userReq())
	if err == nil || len(got()) != 1 || strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err=%v requests=%d, want a single attempt", err, len(got()))
	}
	for in, want := range map[string]int{"": 10, "3": 3, "99": 15, "-1": 10, "x": 10} {
		if g := MaxRetriesFromEnv(func(string) string { return in }); g != want {
			t.Errorf("MaxRetriesFromEnv(%q) = %d, want %d", in, g, want)
		}
	}
}

// retry-after-ms and HTTP-date Retry-After are honored; a wait beyond the 60s
// ceiling gives up immediately.
func TestRetryAfterVariants(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		hdr     map[string]string
		want    time.Duration
		retried bool
	}{
		{"ms", map[string]string{"retry-after-ms": "1500"}, 1500 * time.Millisecond, true},
		{"date", map[string]string{"Retry-After": now.Add(7 * time.Second).Format(http.TimeFormat)}, 7 * time.Second, true},
		{"too-long", map[string]string{"Retry-After": "120"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
				if n == 0 {
					for k, v := range tc.hdr {
						w.Header().Set(k, v)
					}
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error":{"message":"slow"}}`)
					return
				}
				io.WriteString(w, oaiOK)
			})
			mc := orCaller(srv.URL)
			fs, _ := retrying(mc, 10)
			mc.Now = func() time.Time { return now }
			_, err := mc.CallModel(context.Background(), userReq())
			if !tc.retried {
				if err == nil || len(got()) != 1 {
					t.Fatalf("err=%v requests=%d, want give-up without retry", err, len(got()))
				}
				return
			}
			if err != nil {
				t.Fatalf("CallModel: %v", err)
			}
			if d := fs.got(); len(d) != 1 || d[0] != tc.want {
				t.Fatalf("delays = %v, want [%v]", d, tc.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		r       float64
		want    time.Duration
	}{
		{1, 0, 500 * time.Millisecond},
		{2, 0, time.Second},
		{4, 0, 4 * time.Second},
		{7, 0, 32 * time.Second},
		{10, 0, 32 * time.Second},
		{1, 1, 625 * time.Millisecond},
		{7, 1, 40 * time.Second},
	} {
		if g := backoffDelay(tc.attempt, tc.r); g != tc.want {
			t.Errorf("backoffDelay(%d,%v) = %v, want %v", tc.attempt, tc.r, g, tc.want)
		}
	}
}

func TestRetryStatusText(t *testing.T) {
	over := &APIError{Status: 529, Category: CategoryServer, Detail: "Overloaded"}
	if g := RetryStatusText(1, 10, 500*time.Millisecond, over); g != "API error · Retrying in 1s · attempt 1/10" {
		t.Errorf("early = %q", g)
	}
	if g := RetryStatusText(3, 10, 4*time.Second, over); g != "API overloaded · Retrying in 4s · attempt 3/10" {
		t.Errorf("later = %q", g)
	}
	rl := &APIError{Status: 429, Category: CategoryRateLimit}
	if g := RetryStatusText(1, 10, 12*time.Second, rl); g != "Rate limited · Retrying in 12s · attempt 1/10" {
		t.Errorf("rate = %q", g)
	}
	if g := FormatRetryDelay(65 * time.Second); g != "1m 5s" {
		t.Errorf("FormatRetryDelay = %q", g)
	}
}
