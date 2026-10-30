package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/mcp"
)

// MethodProgressNotification is the MCP progress notification a server sends
// while a long request runs, when the client asked for progress by putting a
// progressToken in the request's _meta.
const MethodProgressNotification = "notifications/progress"

// defaultProgressInterval is the heartbeat period of a tools/call that carries a
// progressToken.
const defaultProgressInterval = 30 * time.Second

// WithProgressInterval overrides the progress heartbeat period (default 30s).
// A non-positive value keeps the default. It exists so tests need not wait 30s.
func WithProgressInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.progressInterval = d
		}
	}
}

// progressParams is the params object of a notifications/progress message.
type progressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Message       string          `json:"message,omitempty"`
}

// progressTokenFromMeta returns the raw progressToken of a request's _meta, or
// nil when there is none. MCP allows a string or an integer token; anything
// else (null, object, array, bool) is treated as absent (the schema allows
// only string | number).
func progressTokenFromMeta(meta json.RawMessage) json.RawMessage {
	if len(meta) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(meta, &m); err != nil {
		return nil
	}
	raw, ok := m["progressToken"]
	if !ok {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	switch v.(type) {
	case string, float64:
		return raw
	default:
		return nil
	}
}

// progressHeartbeat is the running heartbeat of one tools/call.
type progressHeartbeat struct {
	stop func()
}

// startProgressHeartbeat starts the heartbeat of one tools/call:
//
//   - with no token it does nothing;
//   - otherwise every interval it sends notifications/progress with
//     progress = elapsed seconds and message "Running for <s>s";
//   - progress is strictly increasing: a value that would not exceed the last
//     one sent becomes last+0.001;
//   - a send failure is logged (via the server's error hook) and ignored.
//
// stop clears the ticker; it must be called when the tool call settles.
func (s *Server) startProgressHeartbeat(token json.RawMessage, send func(progressParams) error, interval time.Duration) progressHeartbeat {
	if token == nil || send == nil {
		return progressHeartbeat{stop: func() {}}
	}
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	var (
		mu   sync.Mutex
		last float64
	)
	emit := func(w float64, msg string) error {
		mu.Lock()
		if w > last {
			last = w
		} else {
			last += 0.001
		}
		p := progressParams{ProgressToken: token, Progress: last, Message: msg}
		mu.Unlock()
		return send(p)
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticks := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ticks++
				w := float64(ticks) * interval.Seconds()
				if err := emit(w, fmt.Sprintf("Running for %ss", formatSeconds(w))); err != nil {
					s.reportProgressError(err)
				}
			}
		}
	}()
	return progressHeartbeat{
		stop: func() {
			once.Do(func() {
				ticker.Stop()
				close(done)
			})
		},
	}
}

// formatSeconds renders a seconds count like JavaScript number-to-string
// conversion: integral values without a fraction ("30"), others in the
// shortest round-trip form ("0.05").
func formatSeconds(w float64) string {
	return strconv.FormatFloat(w, 'f', -1, 64)
}

// reportProgressError logs "MCP server: progress heartbeat not sent: <err>": a
// failed heartbeat never fails the call.
func (s *Server) reportProgressError(err error) {
	s.mu.RLock()
	h := s.onError
	s.mu.RUnlock()
	if h != nil {
		h(fmt.Errorf("MCP server: progress heartbeat not sent: %w", err))
	}
}

// progressSender returns the notification sender of the connection serving ctx,
// or nil when the call is not on a live connection (a direct Handle in tests).
func progressSender(ctx context.Context) func(progressParams) error {
	peer := PeerFromContext(ctx)
	if peer == nil {
		return nil
	}
	return func(p progressParams) error {
		return peer.Notify(context.Background(), MethodProgressNotification, p)
	}
}

// withProgress runs call under a progress heartbeat for the given tools/call
// params.
func (s *Server) withProgress(ctx context.Context, params mcp.CallToolParams, call func(context.Context) (mcp.CallToolResult, error)) (mcp.CallToolResult, error) {
	s.mu.RLock()
	interval := s.progressInterval
	s.mu.RUnlock()
	hb := s.startProgressHeartbeat(progressTokenFromMeta(params.Meta), progressSender(ctx), interval)
	defer hb.stop()
	return call(ctx)
}
