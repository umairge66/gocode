package mods

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Sound: $.audio.play and $.audio.speak. Both use the platform's own tools:
// `afplay` plays a clip and `say` speaks on
// macOS; elsewhere a terminal has no player (a clip is skipped, resolving)
// and no synthesizer (speech rejects). No hosted service is involved: a
// { url } clip is fetched like $.http.fetch, under its rules.

// Limits: the clip's bytes, the gain's ceiling, the plays a plugin has going
// at once, a looping clip's longest run, a clip's fetch timeout, the longest
// text a call carries.
const (
	audioClipLimit   = 26214400
	audioGainMax     = 4
	audioPlaysAtOnce = 4
	audioLoopLimit   = 600 * time.Second
	audioFetchLimit  = 30 * time.Second
	audioTextLimit   = 4096
)

// audioPlatform is the OS the player and speaker go by (a test sets it).
var audioPlatform = runtime.GOOS

// audioRun runs the player or synthesizer (a test replaces it).
var audioRun = func(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			// A stopped child resolves.
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s exited %d", name, ee.ExitCode())
		}
		return fmt.Errorf("%s failed to start: %v", name, err)
	}
	return nil
}

// audioState is the engine's sound state: the plays going per plugin and
// the queue utterances wait in.
type audioState struct {
	mu    sync.Mutex
	plays map[string]int
	speak sync.Mutex
}

func audioPlayCheck(e map[string]any, root string) string {
	clip, ok := e["clip"].(map[string]any)
	if !ok {
		return "$.audio.play: clip must be { asset }, { url } or { base64, mime }"
	}
	var kinds []string
	for _, k := range []string{"asset", "url", "base64"} {
		if v, has := clip[k]; has && v != nil {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		got := "none"
		if len(kinds) > 0 {
			got = strings.Join(kinds, " and ")
		}
		return "$.audio.play: clip must carry exactly one of asset, url, base64 (got " + got + ")"
	}
	kind := kinds[0]
	if s, ok := clip[kind].(string); !ok || s == "" {
		return "$.audio.play: clip." + kind + " must be a non-empty string"
	}
	switch kind {
	case "base64":
		if _, ok := clip["mime"].(string); !ok {
			return "$.audio.play: clip.mime must accompany clip.base64"
		}
		if base64.StdEncoding.DecodedLen(len(clip["base64"].(string))) > audioClipLimit {
			return fmt.Sprintf("$.audio.play: the clip is over %d bytes", audioClipLimit)
		}
	case "asset":
		if root == "" {
			return "$.audio.play: the plugin has no directory"
		}
		if _, err := assetPath(root, clip["asset"].(string)); err != "" {
			return err
		}
	}
	if v, has := e["shouldLoop"]; has && v != nil {
		if _, ok := v.(bool); !ok {
			return "$.audio.play: options.shouldLoop must be a boolean"
		}
	}
	if v, has := e["gain"]; has && v != nil {
		f, ok := v.(float64)
		if !ok || f < 0 || f > audioGainMax {
			return fmt.Sprintf("$.audio.play: options.gain must be a number from 0 to %d", audioGainMax)
		}
	}
	return ""
}

// assetPath: a path inside the plugin's own folder.
func assetPath(root, asset string) (string, string) {
	clean := filepath.Clean(filepath.FromSlash(asset))
	if strings.TrimSpace(asset) == "" || filepath.IsAbs(asset) || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || strings.Contains(asset, "\x00") {
		return "", fmt.Sprintf("$.audio.play: asset must be a path inside the plugin's directory (got %q)", asset)
	}
	for _, seg := range strings.FieldsFunc(asset, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", fmt.Sprintf("$.audio.play: asset must be a path inside the plugin's directory (got %q)", asset)
		}
	}
	return filepath.Join(root, clean), ""
}

func audioSpeakCheck(e map[string]any) string {
	t, ok := e["text"].(string)
	if !ok || strings.TrimSpace(t) == "" {
		return "$.audio.speak: text must be a non-empty string"
	}
	if v, has := e["voice"]; has && v != nil {
		if _, ok := v.(string); !ok {
			return "$.audio.speak: voice must be a string when given"
		}
	}
	return ""
}

// audioPlay: at most four plays a plugin at
// once; afplay on macOS (looping while asked, at most ten minutes); on
// another platform the clip is not played and the call resolves.
func (en *Engine) audioPlay(ctx context.Context, s *seat, e map[string]any) (any, error) {
	if m := audioPlayCheck(e, s.root); m != "" {
		return nil, fmt.Errorf("%s", m)
	}
	clip := e["clip"].(map[string]any)
	if u, ok := clip["url"].(string); ok {
		// The URL obeys $.http.fetch's rules.
		if m := clipURLCheck(s.name, u); m != "" {
			return nil, fmt.Errorf("%s", m)
		}
	}
	a := &en.audio
	a.mu.Lock()
	if a.plays == nil {
		a.plays = map[string]int{}
	}
	if a.plays[s.name] >= audioPlaysAtOnce {
		a.mu.Unlock()
		return nil, fmt.Errorf("%s: $.audio.play: refused: %d plays are going at once", s.name, audioPlaysAtOnce)
	}
	a.plays[s.name]++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.plays[s.name]--
		a.mu.Unlock()
	}()
	if audioPlatform != "darwin" {
		en.logf("$.audio.play (%s): no audio player on %s; not played", s.name, audioPlatformName())
		return nil, nil
	}
	loop, _ := e["shouldLoop"].(bool)
	pctx := ctx
	if loop {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(ctx, audioLoopLimit)
		defer cancel()
	}
	file, cleanup, err := en.clipFile(pctx, s, clip)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var args []string
	if g, ok := e["gain"].(float64); ok {
		args = append(args, "-v", fmt.Sprint(g))
	}
	args = append(args, file)
	for {
		if pctx.Err() != nil {
			return nil, nil
		}
		if err := audioRun(pctx, "afplay", args...); err != nil {
			return nil, fmt.Errorf("$.audio.play: %v", err)
		}
		if !loop {
			return nil, nil
		}
	}
}

// clipURLCheck holds a clip's URL to $.http.fetch's rules.
func clipURLCheck(plugin, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Sprintf("%s: $.audio.play: %s refused: not a URL", plugin, raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Sprintf("%s: $.audio.play: %s refused: http or https only", plugin, raw)
	}
	if u.User != nil {
		return fmt.Sprintf("%s: $.audio.play: %s refused: credentials in the URL", plugin, u.Host)
	}
	if isTelemetryPath(u.EscapedPath()) {
		return fmt.Sprintf("%s: $.audio.play: %s refused: this gocode sends no telemetry", plugin, raw)
	}
	return ""
}

// clipFile: the clip as a file the player reads (the asset
// itself, else the bytes written to a private temporary folder).
func (en *Engine) clipFile(ctx context.Context, s *seat, clip map[string]any) (string, func(), error) {
	if asset, ok := clip["asset"].(string); ok {
		p, _ := assetPath(s.root, asset)
		if !withinReal(s.root, p) {
			return "", nil, fmt.Errorf("$.audio.play: could not read %s in %s's directory: no such file", asset, s.name)
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return "", nil, fmt.Errorf("$.audio.play: could not read %s in %s's directory: no such file", asset, s.name)
		}
		if st.Size() > audioClipLimit {
			return "", nil, fmt.Errorf("$.audio.play: could not read %s in %s's directory: the clip is over %d bytes", asset, s.name, audioClipLimit)
		}
		return p, func() {}, nil
	}
	var data []byte
	mime, _ := clip["mime"].(string)
	if b64, ok := clip["base64"].(string); ok {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", nil, fmt.Errorf("$.audio.play: the clip's base64 does not decode: %v", err)
		}
		data = b
	} else {
		u, _ := clip["url"].(string)
		fctx, cancel := context.WithTimeout(ctx, audioFetchLimit)
		defer cancel()
		client := http.Client{Timeout: audioFetchLimit}
		if h := en.opts.Host; h != nil && h.HTTPClient != nil {
			client = *h.HTTPClient
		}
		req, err := http.NewRequestWithContext(fctx, http.MethodGet, u, nil)
		if err != nil {
			return "", nil, fmt.Errorf("$.audio.play: could not fetch the clip: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("$.audio.play: could not fetch the clip: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", nil, fmt.Errorf("$.audio.play: could not fetch the clip: HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, audioClipLimit+1))
		if err != nil {
			return "", nil, fmt.Errorf("$.audio.play: could not fetch the clip: %v", err)
		}
		if len(b) > audioClipLimit {
			return "", nil, fmt.Errorf("$.audio.play: could not fetch the clip: the clip is over %d bytes", audioClipLimit)
		}
		data = b
		mime = resp.Header.Get("Content-Type")
		if mime == "" {
			mime = "audio/mpeg"
		}
	}
	dir, err := os.MkdirTemp("", "gocode-audio-")
	if err != nil {
		return "", nil, fmt.Errorf("$.audio.play: %v", err)
	}
	file := filepath.Join(dir, "clip."+clipExtension(mime))
	if err := os.WriteFile(file, data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("$.audio.play: %v", err)
	}
	return file, func() { _ = os.RemoveAll(dir) }, nil
}

func clipExtension(mime string) string {
	m, _, _ := strings.Cut(mime, ";")
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/aac":
		return "aac"
	case "audio/ogg":
		return "ogg"
	}
	return "audio"
}

// audioSpeak: utterances wait their turn; `say` speaks
// on macOS; another platform has no synthesizer and the call rejects.
func (en *Engine) audioSpeak(ctx context.Context, s *seat, e map[string]any) (any, error) {
	if m := audioSpeakCheck(e); m != "" {
		return nil, fmt.Errorf("%s", m)
	}
	text := e["text"].(string)
	if len([]rune(text)) > audioTextLimit {
		en.logf("$.audio.speak (%s): a text of %d characters is spoken up to its first %d", s.name, len([]rune(text)), audioTextLimit)
		text = string([]rune(text)[:audioTextLimit])
	}
	en.audio.speak.Lock()
	defer en.audio.speak.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if audioPlatform != "darwin" {
		en.logf("$.audio.speak (%s): no speech synthesizer on %s", s.name, audioPlatformName())
		return nil, fmt.Errorf("$.audio.speak: no speech synthesizer on %s", audioPlatformName())
	}
	var args []string
	if v, ok := e["voice"].(string); ok {
		args = append(args, "-v", v)
	}
	args = append(args, "--", text)
	if err := audioRun(ctx, "say", args...); err != nil {
		return nil, fmt.Errorf("$.audio.speak: %v", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return map[string]any{"via": "system"}, nil
}

// audioPlatformName is the platform's display name.
func audioPlatformName() string {
	switch audioPlatform {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	}
	return audioPlatform
}
