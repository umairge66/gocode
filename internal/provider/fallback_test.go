package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func fallbackCall(t *testing.T, status int, body string, fallback string) (error, int) {
	t.Helper()
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	mc := orCaller(srv.URL)
	retrying(mc, 10)
	mc.SetFallbackModel(fallback)
	_, err := mc.CallModel(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}})
	return err, len(got())
}

func TestFallbackOverloadedAfterThree(t *testing.T) {
	err, n := fallbackCall(t, 529, `{"error":{"type":"overloaded_error","message":"Overloaded"}}`, "backup")
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want FallbackTriggeredError", err)
	}
	if fe.Reason != FallbackOverloaded || fe.OriginalModel != "m" || fe.FallbackModel != "backup" {
		t.Fatalf("fallback = %+v", fe)
	}
	if n != 3 {
		t.Fatalf("requests = %d, want 3 (fallback on the 3rd consecutive overload)", n)
	}
	if fe.Error() != "Model fallback triggered: m -> backup" {
		t.Fatalf("Error() = %q", fe.Error())
	}
}

func TestFallbackServerErrorImmediate(t *testing.T) {
	err, n := fallbackCall(t, 500, `{"error":{"type":"api_error","message":"boom"}}`, "backup")
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackServerError || n != 1 {
		t.Fatalf("err=%v requests=%d, want immediate server_error fallback", err, n)
	}
}

func TestFallbackModelNotFound(t *testing.T) {
	err, n := fallbackCall(t, 404, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`, "backup")
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackModelNotFound || n != 1 {
		t.Fatalf("err=%v requests=%d, want model_not_found fallback", err, n)
	}
}

func TestFallbackLastResortAndExemptions(t *testing.T) {
	err, _ := fallbackCall(t, 410, `{"error":{"message":"gone"}}`, "backup")
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackLastResort {
		t.Fatalf("err=%v, want last_resort fallback", err)
	}
	// 400 is our own payload: it would fail on the fallback too.
	err, n := fallbackCall(t, 400, `{"error":{"message":"bad thing"}}`, "backup")
	if errors.As(err, &fe) || n != 1 {
		t.Fatalf("400 err=%v requests=%d, want the plain error", err, n)
	}
	// 401 is exempt: an auth failure would fail on the fallback too.
	err, _ = fallbackCall(t, 401, `{"error":{"message":"bad key"}}`, "backup")
	if errors.As(err, &fe) {
		t.Fatalf("401 triggered a fallback: %v", err)
	}
}

func TestNoFallbackWhenUnarmed(t *testing.T) {
	err, n := fallbackCall(t, 500, `{"error":{"message":"boom"}}`, "")
	var fe *FallbackTriggeredError
	if errors.As(err, &fe) || n != 11 {
		t.Fatalf("err=%v requests=%d, want plain retries (1+10) with no fallback", err, n)
	}
	// A fallback equal to the current model is ignored.
	err, _ = fallbackCall(t, 500, `{"error":{"message":"boom"}}`, "m")
	if errors.As(err, &fe) {
		t.Fatal("fallback to the same model triggered")
	}
}

func TestFallbackNotice(t *testing.T) {
	cases := []struct {
		reason string
		err    error
		want   string
	}{
		{FallbackOverloaded, nil, "Switched to b due to high demand for a"},
		{FallbackServerError, nil, "Switched to b due to high demand for a"},
		{FallbackPermissionDenied, errors.New("x"), "Switched to b because a is not available"},
		{FallbackModelNotFound, errors.New("weird"), "Switched to b because a is not available (weird)"},
		{FallbackLastResort, errors.New("bad request"), "Switched to b because a returned an error that could not be retried (bad request)"},
	}
	for _, tc := range cases {
		if got := FallbackNotice(tc.reason, "a", "b", tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.reason, got, tc.want)
		}
	}
	long := errors.New(strings.Repeat("x", 400))
	if got := FallbackNotice(FallbackLastResort, "a", "b", long); !strings.HasSuffix(got, "…)") {
		t.Errorf("long detail not truncated: %q", got)
	}
}

func TestParseFallbackModels(t *testing.T) {
	got := ParseFallbackModels(" a, b ,a", "", "c,d")
	want := []string{"a", "b", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ParseFallbackModels = %v, want %v", got, want)
	}
	if ParseFallbackModels("", " , ") != nil {
		t.Fatal("empty input should give nil")
	}
}

// fallbackSequence serves the given (status, body, retry-after) responses in
// order (the last one repeats) and returns the error and the request count.
func fallbackSequence(t *testing.T, max int, fallback string, resp ...[3]string) (error, int) {
	t.Helper()
	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		r := resp[min(n, len(resp)-1)]
		if r[2] != "" {
			w.Header().Set("Retry-After", r[2])
		}
		status := 200
		fmt.Sscan(r[0], &status)
		w.WriteHeader(status)
		io.WriteString(w, r[1])
	})
	mc := orCaller(srv.URL)
	retrying(mc, max)
	mc.SetFallbackModel(fallback)
	_, err := mc.CallModel(context.Background(), agent.ModelRequest{Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))}})
	return err, len(got())
}

const (
	orFreeRateLimit = `{"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429}}`
	orUpstreamLimit = `{"error":{"message":"cohere/north-mini-code:free is temporarily rate-limited upstream. Please retry shortly.","code":429}}`
	orFreePerDay    = `{"error":{"message":"Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day","code":429}}`
)

func TestFallbackRateLimitAfterThree(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{{orFreeRateLimit, 3}, {orUpstreamLimit, 3}, {orFreePerDay, 1}} {
		body := tc.body
		err, n := fallbackSequence(t, 10, "backup", [3]string{"429", body, ""})
		var fe *FallbackTriggeredError
		if !errors.As(err, &fe) || fe.Reason != FallbackRateLimited || n != tc.want {
			t.Fatalf("%s: err=%v requests=%d, want rate_limited fallback after %d", body, err, n, tc.want)
		}
		if got := FallbackNotice(fe.Reason, fe.OriginalModel, fe.FallbackModel, fe.Err); got != "Switched to backup due to rate limits on m" {
			t.Fatalf("notice = %q", got)
		}
	}
	// A 429 that recovers on the retry never switches.
	err, n := fallbackSequence(t, 10, "backup", [3]string{"429", orFreeRateLimit, ""}, [3]string{"200", oaiOK, ""})
	if err != nil || n != 2 {
		t.Fatalf("err=%v requests=%d, want success on the retry", err, n)
	}
	// Without a fallback the 429 keeps its normal retries.
	err, n = fallbackSequence(t, 4, "", [3]string{"429", orFreeRateLimit, ""})
	var fe *FallbackTriggeredError
	if errors.As(err, &fe) || n != 5 {
		t.Fatalf("unarmed err=%v requests=%d, want 1+4 plain retries", err, n)
	}
}

func TestFallbackRateLimitLongRetryAfterImmediate(t *testing.T) {
	err, n := fallbackSequence(t, 10, "backup", [3]string{"429", orFreeRateLimit, "120"})
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackRateLimited || n != 1 {
		t.Fatalf("err=%v requests=%d, want an immediate rate_limited fallback", err, n)
	}
	// Unarmed, the >60s wait still fails at once (no fallback).
	err, n = fallbackSequence(t, 10, "", [3]string{"429", orFreeRateLimit, "120"})
	if errors.As(err, &fe) || n != 1 {
		t.Fatalf("unarmed err=%v requests=%d, want the plain error at once", err, n)
	}
}

func TestFallbackRetriesExhausted(t *testing.T) {
	// Retries disabled: the first 429 falls back instead of failing.
	err, n := fallbackSequence(t, 0, "backup", [3]string{"429", orFreeRateLimit, ""})
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackRateLimited || n != 1 {
		t.Fatalf("err=%v requests=%d, want rate_limited fallback at exhaustion", err, n)
	}
}

func TestFallbackServerAndUpstreamErrors(t *testing.T) {
	cases := []struct {
		status, body, reason, notice string
	}{
		{"503", `{"error":{"message":"Service Unavailable","code":503}}`, FallbackServerError, "Switched to backup due to high demand for m"},
		{"502", `{"error":{"message":"Provider returned error","code":502}}`, FallbackUpstreamError, "Switched to backup due to errors from m"},
		{"500", `{"error":{"message":"upstream connect error"}}`, FallbackUpstreamError, "Switched to backup due to errors from m"},
		{"503", `{"error":{"message":"No instances available","code":503}}`, FallbackModelUnavailable, "Switched to backup because m is not available"},
		{"404", `{"error":{"message":"No endpoints found for cohere/north-mini-code:free.","code":404}}`, FallbackModelNotFound, "Switched to backup because m is not available"},
		{"400", `{"error":{"message":"cohere/nope is not a valid model ID","code":400}}`, FallbackModelNotFound, "Switched to backup because m is not available"},
	}
	for _, tc := range cases {
		err, n := fallbackSequence(t, 10, "backup", [3]string{tc.status, tc.body, ""})
		var fe *FallbackTriggeredError
		if !errors.As(err, &fe) || fe.Reason != tc.reason || n != 1 {
			t.Fatalf("%s %s: err=%v requests=%d, want immediate %s", tc.status, tc.body, err, n, tc.reason)
		}
		if got := FallbackNotice(fe.Reason, fe.OriginalModel, fe.FallbackModel, fe.Err); got != tc.notice {
			t.Fatalf("%s: notice = %q, want %q", tc.body, got, tc.notice)
		}
	}
}

func TestNoFallbackOnKeyPayloadOrContextErrors(t *testing.T) {
	cases := []struct{ status, body string }{
		{"401", `{"error":{"message":"No auth credentials found","code":401}}`},
		{"403", `{"error":{"message":"Key is disabled","code":403}}`},
		{"402", `{"error":{"message":"Insufficient credits","code":402}}`},
		{"400", `{"error":{"message":"tools.0.name: invalid","code":400}}`},
		{"400", `{"error":{"message":"This endpoint's maximum context length is 32768 tokens. However, you requested about 40000 tokens","code":400}}`},
		{"413", `{"error":{"message":"Prompt is too long: 250000 tokens > 200000"}}`},
	}
	for _, tc := range cases {
		err, n := fallbackSequence(t, 10, "backup", [3]string{tc.status, tc.body, ""})
		var fe *FallbackTriggeredError
		if errors.As(err, &fe) {
			t.Fatalf("%s %s: fell back (%v), want the error shown", tc.status, tc.body, err)
		}
		if n != 1 {
			t.Fatalf("%s: requests = %d, want 1", tc.body, n)
		}
	}
}

// OpenRouter reports an upstream failure inside a 200 stream as an error
// chunk; it is the same typed error as the failed request would be, so it is
// retried and falls back the same way instead of ending the turn quietly.
func TestStreamErrorChunkFallsBack(t *testing.T) {
	sse := "data: {\"id\":\"g1\",\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n" +
		"data: {\"error\":{\"code\":502,\"message\":\"Provider returned error\"},\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := accumulateOAIStream("openrouter", strings.NewReader(sse), nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 502 || !ae.Retryable || ae.Detail != "Provider returned error" {
		t.Fatalf("error chunk = %#v", err)
	}

	srv, got := recordingServer(t, func(w http.ResponseWriter, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	})
	mc := orCaller(srv.URL)
	retrying(mc, 10)
	mc.SetFallbackModel("backup")
	_, err = mc.CallModelStream(context.Background(), agent.ModelRequest{
		Messages: []agent.Message{agent.UserMessage(agent.TextBlock("hi"))},
	}, &agent.StreamHandler{OnText: func(string) {}})
	var fe *FallbackTriggeredError
	if !errors.As(err, &fe) || fe.Reason != FallbackUpstreamError || len(got()) != 1 {
		t.Fatalf("stream error chunk: err=%v requests=%d, want immediate upstream fallback", err, len(got()))
	}

	// A stream with no error chunk still parses as before.
	if _, err := accumulateOAIStream("openrouter", strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil, nil); err != nil {
		t.Fatal(err)
	}
}
