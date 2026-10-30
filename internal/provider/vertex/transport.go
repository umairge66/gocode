package vertex

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/umairge66/gocode/internal/provider"
)

// This file is the Vertex-specific HTTP layer under the shared OpenAI
// chat-completions client (provider.OpenAI). It
//
//   - attaches the OAuth bearer token (and x-goog-user-project) — only to the
//     configured Vertex host;
//   - rewrites the outgoing body: replays Gemini thought signatures on
//     assistant tool calls (Gemini 3 rejects a function-calling history without
//     them, HTTP 400) and strips "$schema" from tool parameter schemas;
//   - rewrites the incoming body: gives every tool call a stable id and stream
//     index (Gemini emits whole calls per chunk, not always with an index) and
//     records each call's extra_content.google.thought_signature for replay.
//
// The neutral provider.ChatToolCall has no place for a signature, so the
// provider keeps an in-memory id → signature map for the session.

// skipSignature is the documented last-resort sentinel for a function call the
// API did not generate (e.g. history from another model or a resumed session).
const skipSignature = "skip_thought_signature_validator"

// signatureStore maps tool-call ids to Gemini thought signatures.
type signatureStore struct {
	mu sync.Mutex
	m  map[string]string
}

// maxSignatures bounds the store; on overflow it is reset (older calls then
// fall back to the sentinel).
const maxSignatures = 20000

func (s *signatureStore) put(id, sig string) {
	if id == "" || sig == "" {
		return
	}
	s.mu.Lock()
	if s.m == nil || len(s.m) >= maxSignatures {
		s.m = map[string]string{}
	}
	s.m[id] = sig
	s.mu.Unlock()
}

func (s *signatureStore) get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	return v, ok
}

// geminiVersionRe extracts the major version from a Gemini model id.
var geminiVersionRe = regexp.MustCompile(`gemini-(\d+)`)

// needsSignatures reports whether model enforces thought-signature validation
// on function calls (Gemini 3 and later).
func needsSignatures(model string) bool {
	m := geminiVersionRe.FindStringSubmatch(strings.ToLower(model))
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	return n >= 3
}

// transport is the provider.Doer handed to provider.OpenAI for one call.
type transport struct {
	base         provider.Doer
	host         string // only requests to this host get the token
	token        string
	quotaProject string
	model        string
	sigs         *signatureStore
	tokens       TokenSource // invalidated on 401
}

func (t *transport) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.host {
		req.Header.Set("Authorization", "Bearer "+t.token)
		if t.quotaProject != "" {
			req.Header.Set("x-goog-user-project", t.quotaProject)
		}
	}
	if req.Body != nil && req.Method == http.MethodPost {
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if out, ok := rewriteRequest(raw, t.model, t.sigs); ok {
			raw = out
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	resp, err := t.base.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if inv, ok := t.tokens.(invalidator); ok {
			inv.Invalidate()
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || req.Header.Get("Accept") == "text/event-stream" {
		resp.Body = newStreamRewriter(resp.Body, t.sigs)
		return resp, nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if out, ok := rewriteResponse(raw, t.sigs); ok {
		raw = out
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	return resp, nil
}

// decodeJSON decodes into generic values, keeping numbers exact.
func decodeJSON(raw []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return nil, false
	}
	return m, true
}

// rewriteRequest adds thought signatures and cleans tool schemas. ok=false
// leaves the body untouched.
func rewriteRequest(raw []byte, model string, sigs *signatureStore) ([]byte, bool) {
	body, ok := decodeJSON(raw)
	if !ok {
		return nil, false
	}
	changed := false
	if msgs, _ := body["messages"].([]any); msgs != nil {
		enforce := needsSignatures(model)
		for _, mv := range msgs {
			m, _ := mv.(map[string]any)
			if m == nil || m["role"] != "assistant" {
				continue
			}
			calls, _ := m["tool_calls"].([]any)
			if len(calls) == 0 {
				continue
			}
			known := false
			for _, cv := range calls {
				c, _ := cv.(map[string]any)
				if c == nil {
					continue
				}
				id, _ := c["id"].(string)
				if sig, ok := sigs.get(id); ok {
					c["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": sig}}
					known = true
					changed = true
				}
			}
			if !known && enforce {
				if c, _ := calls[0].(map[string]any); c != nil {
					c["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": skipSignature}}
					changed = true
				}
			}
		}
	}
	if tools, _ := body["tools"].([]any); tools != nil {
		for _, tv := range tools {
			tm, _ := tv.(map[string]any)
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if p, ok := fn["parameters"]; ok && stripSchemaKeys(p) {
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return out, true
}

// stripSchemaKeys removes "$schema" keys (which Vertex's schema translation
// rejects) from a JSON-Schema value, recursively. Reports whether it changed.
func stripSchemaKeys(v any) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["$schema"]; ok {
			delete(x, "$schema")
			changed = true
		}
		for _, c := range x {
			if stripSchemaKeys(c) {
				changed = true
			}
		}
	case []any:
		for _, c := range x {
			if stripSchemaKeys(c) {
				changed = true
			}
		}
	}
	return changed
}

// newCallID generates an OpenAI-style call id for a tool call that has none.
func newCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// signatureOf returns extra_content.google.thought_signature of a tool call.
func signatureOf(c map[string]any) string {
	ec, _ := c["extra_content"].(map[string]any)
	g, _ := ec["google"].(map[string]any)
	s, _ := g["thought_signature"].(string)
	return s
}

// rewriteResponse fills missing tool-call ids and records signatures in a
// non-streaming response.
func rewriteResponse(raw []byte, sigs *signatureStore) ([]byte, bool) {
	body, ok := decodeJSON(raw)
	if !ok {
		return nil, false
	}
	changed := false
	choices, _ := body["choices"].([]any)
	for _, chv := range choices {
		ch, _ := chv.(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		for _, cv := range calls {
			c, _ := cv.(map[string]any)
			if c == nil {
				continue
			}
			id, _ := c["id"].(string)
			if id == "" {
				id = newCallID()
				c["id"] = id
				changed = true
			}
			sigs.put(id, signatureOf(c))
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return out, true
}

// streamRewriter rewrites SSE `data:` lines that carry tool calls, assigning
// each distinct call its own index and id and recording signatures. Lines
// without tool calls pass through byte-for-byte.
type streamRewriter struct {
	src  io.ReadCloser
	r    *bufio.Reader
	buf  bytes.Buffer
	err  error
	sigs *signatureStore

	next int            // next free index
	cur  int            // index of the call being continued (-1: none)
	seen map[string]int // call id -> index
	orig map[int]int    // upstream index -> our index (for continuations)
}

func newStreamRewriter(src io.ReadCloser, sigs *signatureStore) *streamRewriter {
	return &streamRewriter{src: src, r: bufio.NewReaderSize(src, 64*1024), sigs: sigs, cur: -1, seen: map[string]int{}, orig: map[int]int{}}
}

func (s *streamRewriter) Read(p []byte) (int, error) {
	for s.buf.Len() == 0 {
		if s.err != nil {
			return 0, s.err
		}
		line, err := s.r.ReadString('\n')
		if line != "" {
			s.buf.WriteString(s.rewriteLine(line))
		}
		if err != nil {
			s.err = err
		}
	}
	return s.buf.Read(p)
}

func (s *streamRewriter) Close() error { return s.src.Close() }

func (s *streamRewriter) rewriteLine(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") || !strings.Contains(trimmed, "tool_calls") {
		return line
	}
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	chunk, ok := decodeJSON([]byte(data))
	if !ok {
		return line
	}
	changed := false
	choices, _ := chunk["choices"].([]any)
	for _, chv := range choices {
		ch, _ := chv.(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, cv := range calls {
			c, _ := cv.(map[string]any)
			if c == nil {
				continue
			}
			changed = true
			id, _ := c["id"].(string)
			fn, _ := c["function"].(map[string]any)
			name, _ := fn["name"].(string)
			upstream, hasUpstream := -1, false
			if n, ok := c["index"].(json.Number); ok {
				if v, err := n.Int64(); err == nil {
					upstream, hasUpstream = int(v), true
				}
			}
			switch {
			case id != "":
				idx, ok := s.seen[id]
				if !ok {
					idx = s.next
					s.next++
					s.seen[id] = idx
				}
				s.cur = idx
			case name != "":
				// A new call without an id.
				id = newCallID()
				c["id"] = id
				s.seen[id] = s.next
				s.cur = s.next
				s.next++
			default:
				// A continuation fragment (arguments only).
				if hasUpstream {
					if idx, ok := s.orig[upstream]; ok {
						s.cur = idx
						break
					}
				}
				if s.cur < 0 {
					s.cur = s.next
					s.next++
				}
			}
			if hasUpstream && (id != "" || name != "") {
				s.orig[upstream] = s.cur
			}
			c["index"] = s.cur
			if sig := signatureOf(c); sig != "" {
				if id == "" {
					for k, v := range s.seen {
						if v == s.cur {
							id = k
						}
					}
				}
				s.sigs.put(id, sig)
			}
		}
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return line
	}
	return "data: " + string(out) + "\n"
}
