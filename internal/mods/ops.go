package mods

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/git"
)

// Host is what serves the calls on `$` beneath the hooks: the engine's own
// reach into the file system, the clock, the network, local processes, the
// settings and the environment. Every field is optional.
//
// Nothing here reports anywhere: $.telemetry.* are local no-ops, and
// $.http.fetch fetches only what a module declared and asked for.
type Host struct {
	// Cwd is the session's working directory (relative paths resolve there).
	Cwd func() string
	// ProjectRoot is $.session.root (default Cwd).
	ProjectRoot func() string
	// AdditionalDirs are the session's additional working directories
	// (--add-dir, permissions.additionalDirectories). With Cwd, ProjectRoot
	// and the plugin's own root and data folder they are all a hooks module's
	// $.fs calls may reach (see fsScope).
	AdditionalDirs func() []string
	// SessionID is $.session.id.
	SessionID func() string
	// Model is $.session.model.
	Model func() string
	// Version is $.session.version's version.
	Version string
	// Interactive selects the surface $.session.surface reports.
	Interactive bool
	// Settings serves $.settings.read: source "" is the merge, else one of
	// user, project, local, flag, policy.
	Settings func(source string) (map[string]any, error)
	// PinnedEnv reports a variable a plugin outside policy may not set (the
	// managed settings' env keys); the code-loading and policy-source
	// variables are pinned regardless.
	PinnedEnv func(name string) bool
	// Notify receives $.ui.toast / status / log / notify lines.
	Notify func(plugin, kind, text string)
	// HTTPClient performs $.http.fetch (nil: a client with a
	// 30 s bound).
	HTTPClient *http.Client
	// Getenv / Setenv / Unsetenv default to the process environment.
	Getenv   func(string) (string, bool)
	Setenv   func(string, string) error
	Unsetenv func(string) error
	// Models serves $.model.*: completions through the session's own
	// provider, model and key (never a gocode-hosted service).
	Models ModelHost
	// MCP serves $.mcp.*: the session's MCP host (nil: none). It is a
	// function because the host is built after the modules load.
	MCP func() MCPHost

	storeMu sync.Mutex
}

// Limits of the calls (4 MiB file read/write, request body, process output
// per stream, process timeout, http timeout, redirects).
const (
	fsLimit            = 4194304
	httpBodyLimit      = 4194304
	processOutputLimit = 4194304
	processTimeout     = 30 * time.Second
	processTimeoutMax  = 600 * time.Second
	httpTimeout        = 30 * time.Second
	httpRedirects      = 5
	storeValueLimit    = 4194304
)

// pinnedVariables are the variables no plugin outside policy changes: the
// policy-source and config variables, the process wrapper
// and the code-loading variables.
var pinnedVariables = []string{
	"GOCODE_CODE_MANAGED_SETTINGS_PATH", "GOCODE_CODE_REMOTE_SETTINGS_PATH", "GOCODE_CODE_DISABLE_ADMIN_ENV_UNION",
	"GOCODE_CODE_MOCK_REMOTE_SETTINGS", "GOCODE_CONFIG_DIR", "GOCODE_SECURESTORAGE_CONFIG_DIR", "USE_LOCAL_OAUTH",
	"USE_STAGING_OAUTH", "GOCODE_LOCAL_OAUTH_API_BASE", "GOCODE_CODE_PROCESS_WRAPPER",
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
	"DYLD_FRAMEWORK_PATH", "BASH_ENV", "ENV", "ZDOTDIR", "NODE_OPTIONS",
}

func (h *Host) cwd() string {
	if h != nil && h.Cwd != nil {
		if c := h.Cwd(); c != "" {
			return c
		}
	}
	wd, _ := os.Getwd()
	return wd
}

// fsScope is the reach of plugin s's $.fs calls: the project (cwd,
// $.session.root, the additional working directories) and the plugin's own
// root and data folder.
func (h *Host) fsScope(s *seat) fsScope {
	projects := []string{h.cwd()}
	if h != nil && h.ProjectRoot != nil {
		projects = append(projects, h.ProjectRoot())
	}
	if h != nil && h.AdditionalDirs != nil {
		projects = append(projects, h.AdditionalDirs()...)
	}
	return newFSScope(projects, s.root, s.dataDir)
}

// opChecks are the host's argument checks.
var opChecks = map[string]func(map[string]any) string{
	"fs.read": func(a map[string]any) string {
		if m := pathCheck(a); m != "" {
			return m
		}
		if as := a["as"]; as != "text" && as != "bytes" {
			return `as is "text" or "bytes"`
		}
		return ""
	},
	"fs.write": func(a map[string]any) string {
		if m := pathCheck(a); m != "" {
			return m
		}
		if _, ok := a["text"].(string); !ok {
			return "takes the text to write"
		}
		return ""
	},
	"fs.list":   pathCheck,
	"fs.exists": pathCheck,
	"fs.stat": func(a map[string]any) string {
		if m := pathCheck(a); m != "" {
			return m
		}
		if _, ok := a["resolve"].(bool); !ok {
			return "resolve is true or false"
		}
		return ""
	},
	"process.run": func(a map[string]any) string {
		if m := argvCheck(a["argv"]); m != "" {
			return m
		}
		init, has := a["init"]
		if !has || init == nil {
			return ""
		}
		in, ok := init.(map[string]any)
		if !ok {
			return "takes init, { cwd?, env?, stdin?, timeoutMs? }"
		}
		if c, has := in["cwd"]; has && c != nil {
			if s, ok := c.(string); !ok || s == "" {
				return "init.cwd is a non-empty path"
			}
		}
		if m := stringMapCheck(in["env"], "init.env"); m != "" {
			return m
		}
		if s, has := in["stdin"]; has && s != nil {
			if _, ok := s.(string); !ok {
				return "init.stdin is a string"
			}
		}
		if t, has := in["timeoutMs"]; has && t != nil {
			f, ok := t.(float64)
			if !ok || f != float64(int64(f)) || f <= 0 || f > float64(processTimeoutMax.Milliseconds()) {
				return fmt.Sprintf("init.timeoutMs is a whole number of ms, 1 to %d", processTimeoutMax.Milliseconds())
			}
		}
		return ""
	},
	"http.fetch": func(a map[string]any) string {
		if _, ok := a["url"].(string); !ok {
			return "takes a URL"
		}
		return ""
	},
	"env.get":     envCheck,
	"env.set":     envCheck,
	"clock.sleep": msCheck, "clock.after": msCheck, "clock.every": msCheck,
	"process.spawn": spawnCheck,
	"state.get":     stateGetCheck,
	"state.set":     stateSetCheck,
	"model.complete": func(a map[string]any) string {
		return modelCompleteCheck(a)
	},
	"model.classify": func(a map[string]any) string {
		if _, ok := a["text"].(string); !ok {
			return "takes { text, labels }"
		}
		if _, ok := a["labels"].([]any); !ok {
			return "takes { text, labels }"
		}
		return ""
	},
	"model.fork": func(a map[string]any) string {
		if p, ok := a["prompt"].(string); !ok || strings.TrimSpace(p) == "" {
			return "takes { prompt } (a non-empty prompt)"
		}
		return ""
	},
	"mcp.call":     mcpCallCheck,
	"mcp.connect":  mcpConnectCheck,
	"store.get":    keyCheck,
	"store.delete": keyCheck,
	"store.set": func(a map[string]any) string {
		if m := keyCheck(a); m != "" {
			return m
		}
		b, err := json.Marshal(a["value"])
		if err != nil {
			return "value is not JSON data: " + err.Error()
		}
		if len(b) > storeValueLimit {
			return fmt.Sprintf("the value is %d characters, over the %d limit", len(b), storeValueLimit)
		}
		return ""
	},
	"settings.read": func(a map[string]any) string {
		if s, has := a["source"]; has && s != nil {
			if !slices.Contains([]string{"user", "project", "local", "flag", "policy"}, fmt.Sprint(s)) {
				return "source is one of user, project, local, flag, policy"
			}
		}
		return ""
	},
	"ui.toast":  textCheck,
	"ui.log":    textCheck,
	"ui.notify": textCheck,
}

func pathCheck(a map[string]any) string {
	if p, ok := a["path"].(string); !ok || p == "" {
		return "takes a path"
	}
	return ""
}

func textCheck(a map[string]any) string {
	if _, ok := a["text"].(string); !ok {
		return "takes a string text"
	}
	return ""
}

func keyCheck(a map[string]any) string {
	if _, ok := a["key"].(string); !ok {
		return "takes a key"
	}
	return ""
}

func msCheck(a map[string]any) string {
	if f, ok := a["ms"].(float64); !ok || f < 0 || f > 2147483647 {
		return "takes { ms }, a duration in milliseconds"
	}
	return ""
}

func argvCheck(v any) string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return "takes argv, a non-empty list of strings naming the command first"
	}
	for i, x := range arr {
		s, ok := x.(string)
		if !ok || (i == 0 && s == "") {
			return "takes argv, a non-empty list of strings naming the command first"
		}
	}
	return ""
}

func stringMapCheck(v any, what string) string {
	if v == nil {
		return ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return what + " is an object of strings"
	}
	for _, x := range m {
		if _, ok := x.(string); !ok {
			return what + " is an object of strings"
		}
	}
	return ""
}

func envCheck(a map[string]any) string {
	n, _ := a["name"].(string)
	if n == "" || strings.Contains(n, "=") || strings.Contains(n, "\x00") {
		got, _ := json.Marshal(a["name"])
		return "takes a variable name on a plain object (got " + string(got) + ")"
	}
	if v, has := a["value"]; has && v != nil {
		s, ok := v.(string)
		if !ok {
			return fmt.Sprintf("takes a string value, or undefined to unset (got %T)", v)
		}
		if strings.Contains(s, "\x00") {
			return "takes a value with no NUL byte in it"
		}
	}
	return ""
}

// raise makes an fs call's path absolute against the working directory
// before the hooks see it.
func (en *Engine) raise(name string, args map[string]any) map[string]any {
	if !strings.HasPrefix(name, "fs.") {
		return args
	}
	p, ok := args["path"].(string)
	if !ok || p == "" || filepath.IsAbs(p) {
		return args
	}
	out := cloneMap(args)
	out["path"] = filepath.Join(en.opts.Host.cwd(), p)
	return out
}

// stamp is the ops table's stamp: state.set's event carries the value it
// replaces as `previous`.
func (en *Engine) stamp(name string, args map[string]any) map[string]any {
	if name != "state.set" {
		return args
	}
	out := cloneMap(args)
	out["previous"] = en.state.previous(refOf(args))
	return out
}

// newCallID is a random id for a call the engine makes on a plugin's behalf.
func newCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// eventCore is the core of an engine event a module raises through `$`.
// tool.call, command.run and the other engine events are raised only
// from the session itself, so a module's own raise reaches no core here
// (under `gocode plugin test` the test's hooks answer it).
func (en *Engine) eventCore(s *seat, name string) CoreFunc {
	if en.opts.testMode {
		return nil
	}
	return func(context.Context, map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("%s: $.%s: this gocode build does not let a hooks module raise %s", s.name, name, name)
	}
}

// opCore is the implementation beneath the hooks of a call on `$`. Under
// the test kit there is none: the environment has no fs, network or process,
// and what nothing beneath the plugins answers rejects.
func (en *Engine) opCore(s *seat, reg *registration, name string) CoreFunc {
	if en.opts.testMode {
		return nil
	}
	h := en.opts.Host
	value := func(v any) (map[string]any, error) { return map[string]any{"value": v}, nil }
	switch name {
	case "fs.read", "fs.write", "fs.list", "fs.exists", "fs.stat":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			v, err := fsOp(s.name, strings.TrimPrefix(name, "fs."), e, h.cwd(), h.fsScope(s))
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "clock.now":
		return func(context.Context, map[string]any) (map[string]any, error) {
			return value(float64(time.Now().UnixMilli()))
		}
	case "clock.sleep", "clock.after", "clock.every":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			ms, _ := e["ms"].(float64)
			t := time.NewTimer(time.Duration(ms * float64(time.Millisecond)))
			defer t.Stop()
			select {
			case <-t.C:
				return value(nil)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	case "process.run":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			v, err := processRun(ctx, s.name, e, h.cwd())
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "http.fetch":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			v, err := httpFetch(ctx, s.name, e, h)
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "settings.read":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			if h == nil || h.Settings == nil {
				return value(map[string]any{})
			}
			src, _ := e["source"].(string)
			st, err := h.Settings(src)
			if err != nil {
				return nil, fmt.Errorf("%s: $.settings.read: %v", s.name, err)
			}
			if st == nil {
				st = map[string]any{}
			}
			if k, ok := e["key"].(string); ok {
				return value(st[k])
			}
			return value(st)
		}
	case "env.get":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			n, _ := e["name"].(string)
			if n == "USER_TYPE" {
				return value("external")
			}
			if v, ok := h.getenv(n); ok {
				return value(v)
			}
			return value(nil)
		}
	case "env.set":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			n, _ := e["name"].(string)
			if s.tier != TierPrepend && s.tier != TierAppend && h.pinned(n) {
				return value(map[string]any{"deny": fmt.Sprintf("env (managed): %s is not for plugins outside policy to change", n)})
			}
			var err error
			if v, ok := e["value"].(string); ok {
				err = h.setenv(n, v)
			} else {
				err = h.unsetenv(n)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: $.env.set: %v", s.name, err)
			}
			return value(map[string]any{})
		}
	case "session.cwd":
		return func(context.Context, map[string]any) (map[string]any, error) { return value(h.cwd()) }
	case "session.root":
		return func(context.Context, map[string]any) (map[string]any, error) {
			if h != nil && h.ProjectRoot != nil {
				return value(h.ProjectRoot())
			}
			return value(h.cwd())
		}
	case "session.id":
		return func(context.Context, map[string]any) (map[string]any, error) {
			if h != nil && h.SessionID != nil {
				return value(h.SessionID())
			}
			return value("")
		}
	case "session.model":
		return func(context.Context, map[string]any) (map[string]any, error) {
			if h != nil && h.Model != nil {
				return value(h.Model())
			}
			return value("")
		}
	case "session.version":
		return func(context.Context, map[string]any) (map[string]any, error) {
			v := ""
			if h != nil {
				v = h.Version
			}
			return value(map[string]any{"version": v})
		}
	case "session.surface":
		return func(context.Context, map[string]any) (map[string]any, error) {
			if h != nil && h.Interactive {
				return value("terminal")
			}
			return value(nil)
		}
	case "session.surfaces":
		return func(context.Context, map[string]any) (map[string]any, error) {
			if h != nil && h.Interactive {
				return value([]any{"terminal"})
			}
			return value([]any{})
		}
	case "ui.toast", "ui.status", "ui.log", "ui.notify":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			text, _ := e["text"].(string)
			kind := strings.TrimPrefix(name, "ui.")
			if h != nil && h.Notify != nil {
				h.Notify(s.name, kind, text)
			} else {
				en.logf("[%s] $.ui.%s: %s", s.name, kind, text)
			}
			if name == "ui.notify" {
				return value(map[string]any{})
			}
			return value(nil)
		}
	case "state.get":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			v, ver := en.state.get(refOf(e))
			return value(map[string]any{"value": v, "version": ver})
		}
	case "state.set":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			var iv *float64
			if f, ok := e["ifVersion"].(float64); ok {
				iv = &f
			}
			isSet, ver, err := en.state.set(refOf(e), e["value"], iv)
			if err != nil {
				return nil, fmt.Errorf("%s: $.state.set: %v", s.name, err)
			}
			return value(map[string]any{"isSet": isSet, "version": ver})
		}
	case "model.complete", "model.classify", "model.fork":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			var v any
			var err error
			switch name {
			case "model.complete":
				v, err = en.modelComplete(ctx, s.name, e)
			case "model.classify":
				v, err = en.modelClassify(ctx, s.name, e)
			default:
				v, err = en.modelFork(ctx, s.name, e)
			}
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "mcp.call":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			v, err := en.mcpCall(ctx, s, reg, e)
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "mcp.connect":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			v, err := en.mcpConnect(ctx, s, e)
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "audio.play", "audio.speak":
		return func(ctx context.Context, e map[string]any) (map[string]any, error) {
			var v any
			var err error
			if name == "audio.play" {
				v, err = en.audioPlay(ctx, s, e)
			} else {
				v, err = en.audioSpeak(ctx, s, e)
			}
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	case "store.get", "store.set", "store.delete", "store.keys":
		return func(_ context.Context, e map[string]any) (map[string]any, error) {
			v, err := h.store(s, strings.TrimPrefix(name, "store."), e)
			if err != nil {
				return nil, err
			}
			return value(v)
		}
	}
	return func(context.Context, map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("%s: $.%s: this gocode build has no implementation of it", s.name, name)
	}
}

func (h *Host) getenv(n string) (string, bool) {
	if h != nil && h.Getenv != nil {
		return h.Getenv(n)
	}
	return os.LookupEnv(n)
}

func (h *Host) setenv(n, v string) error {
	if h != nil && h.Setenv != nil {
		return h.Setenv(n, v)
	}
	return os.Setenv(n, v)
}

func (h *Host) unsetenv(n string) error {
	if h != nil && h.Unsetenv != nil {
		return h.Unsetenv(n)
	}
	return os.Unsetenv(n)
}

// pinned: a variable a plugin outside policy may not change.
func (h *Host) pinned(n string) bool {
	for _, p := range pinnedVariables {
		if strings.EqualFold(p, n) {
			return true
		}
	}
	return h != nil && h.PinnedEnv != nil && h.PinnedEnv(n)
}

// --- fs ---------------------------------------------------------------------

// isNetworkLocation reports a UNC or `//host` spelling. The
// implementation beneath the hooks never reaches a network location.
func isNetworkLocation(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) || strings.HasPrefix(p, `\/`)
}

// fsOp: the file system as the engine's own process reaches it.
func fsOp(plugin, event string, e map[string]any, cwd string, scope fsScope) (any, error) {
	raw, _ := e["path"].(string)
	p := raw
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	refused := func(why string) error {
		return fmt.Errorf("%s: $.fs.%s: %s refused: %s", plugin, event, raw, why)
	}
	if isNetworkLocation(raw) || isNetworkLocation(p) {
		return nil, refused("a network location is not reached from here")
	}
	// Confined to the project and the plugin's own folders (fsScope).
	real, why := scope.check(p, event == "write")
	if why != "" {
		return nil, refused(why)
	}
	if event != "stat" {
		p = real // stat reports on the link itself; the rest use the resolved path
	}
	failed := func(err error) error {
		return fmt.Errorf("%s: $.fs.%s(%s) failed: %s", plugin, event, raw, errnoOf(err))
	}
	switch event {
	case "read":
		f, err := openNonBlocking(p)
		if err != nil {
			return nil, failed(err)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, fsLimit+1))
		if err != nil {
			return nil, failed(err)
		}
		if len(data) > fsLimit {
			return nil, refused(fmt.Sprintf("the file is over the %d-byte limit", fsLimit))
		}
		if e["as"] == "bytes" {
			return map[string]any{"base64": base64.StdEncoding.EncodeToString(data)}, nil
		}
		return string(data), nil
	case "write":
		text, _ := e["text"].(string)
		if len(text) > fsLimit {
			return nil, refused(fmt.Sprintf("%d bytes is over the %d-byte limit", len(text), fsLimit))
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, failed(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			return nil, failed(err)
		}
		return nil, nil
	case "exists":
		_, err := os.Stat(p)
		return err == nil, nil
	case "stat":
		lst, err := os.Lstat(p)
		if err != nil {
			return nil, failed(err)
		}
		isLink := lst.Mode()&fs.ModeSymlink != 0
		st := lst
		var target fs.FileInfo
		if isLink {
			if t, err := os.Stat(p); err == nil {
				target, st = t, t
			}
		} else {
			target = lst
		}
		out := map[string]any{"kind": kindOf(st), "size": float64(st.Size()), "mtimeMs": float64(st.ModTime().UnixMilli()), "isLink": isLink}
		if r, _ := e["resolve"].(bool); r && target != nil {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				if abs, err := filepath.Abs(real); err == nil {
					out["realPath"] = abs
				}
			}
		}
		return out, nil
	case "list":
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, failed(err)
		}
		out := make([]any, 0, len(ents))
		for _, d := range ents {
			ent := map[string]any{"name": d.Name(), "kind": "other", "size": float64(0), "mtimeMs": float64(0), "isLink": d.Type()&fs.ModeSymlink != 0}
			switch {
			case d.Type()&fs.ModeSymlink != 0:
			case d.IsDir():
				ent["kind"] = "dir"
			case d.Type().IsRegular():
				ent["kind"] = "file"
				if info, err := d.Info(); err == nil {
					ent["size"] = float64(info.Size())
					ent["mtimeMs"] = float64(info.ModTime().UnixMilli())
				}
			}
			out = append(out, ent)
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
		})
		return out, nil
	}
	return nil, fmt.Errorf("%s: $.fs.%s: no such call", plugin, event)
}

func kindOf(st fs.FileInfo) string {
	switch {
	case st.Mode().IsRegular():
		return "file"
	case st.IsDir():
		return "dir"
	}
	return "other"
}

func errnoOf(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// --- process ------------------------------------------------------------------

var gitCommand = regexp.MustCompile(`^git(\.exe|\.cmd|\.bat|\.com)?$`)

// processRun: a command by its argument vector, no shell, the session's
// working directory by default, bounded in time and output. Git runs with the
// repository's hooks and fsmonitor off.
func processRun(ctx context.Context, plugin string, e map[string]any, cwd string) (any, error) {
	argv, _ := stringList(e["argv"])
	init, _ := e["init"].(map[string]any)
	cmdName, args := argv[0], argv[1:]
	dir := cwd
	if c, ok := init["cwd"].(string); ok && c != "" {
		if filepath.IsAbs(c) {
			dir = c
		} else {
			dir = filepath.Join(cwd, c)
		}
	}
	timeout := processTimeout
	if t, ok := init["timeoutMs"].(float64); ok && t > 0 {
		timeout = min(time.Duration(t)*time.Millisecond, processTimeoutMax)
	}
	label := fmt.Sprintf("%s: $.process.run(%s)", plugin, cmdName)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env, _ := init["env"].(map[string]any)
	cmd := hostCommand(rctx, cmdName, args, dir, env)
	if s, ok := init["stdin"].(string); ok {
		cmd.Stdin = strings.NewReader(s)
	}
	stdout := &cappedBuffer{limit: processOutputLimit}
	stderr := &cappedBuffer{limit: processOutputLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if rctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return nil, fmt.Errorf("%s aborted: still running after %dms", label, timeout.Milliseconds())
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s aborted", label)
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		if code < 0 {
			code = 1
		}
	} else if err != nil {
		return nil, fmt.Errorf("%s failed to start: %v", label, err)
	}
	return map[string]any{
		"exitCode": float64(code), "stdout": stdout.String(), "stderr": stderr.String(),
		"isStdoutTruncated": stdout.truncated, "isStderrTruncated": stderr.truncated,
	}, nil
}

// hostCommand: a command by its argument vector, never through
// a shell, in dir, with the session's environment and env over it. Git runs
// with the repository's hooks and fsmonitor off. $.process.run
// and $.process.spawn both start their child here.
func hostCommand(ctx context.Context, cmdName string, args []string, dir string, env map[string]any) *exec.Cmd {
	if gitCommand.MatchString(strings.ToLower(filepath.Base(cmdName))) {
		args = append(slices.Clone(git.BaseConfigArgs), args...)
	}
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = dir
	merged := os.Environ()
	for _, k := range sortedKeysAny(env) {
		merged = append(merged, k+"="+fmt.Sprint(env[k]))
	}
	cmd.Env = merged
	return cmd
}

// cappedBuffer keeps the first limit bytes written.
type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.Len()
	if room <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if len(p) > room {
		b.truncated = true
		b.Buffer.Write(p[:room])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// --- http ---------------------------------------------------------------------

// telemetryBatchPath is the event-logging endpoint. No telemetry is sent, so
// such a request is refused outright.
const telemetryBatchPath = "/api/event_logging/v2/batch"

// httpFetch: http or https only, no credentials in the URL, a bounded
// body, at most five redirects, the response read whole (bounded).
func httpFetch(ctx context.Context, plugin string, e map[string]any, h *Host) (any, error) {
	const lb = "$.http.fetch"
	raw, _ := e["url"].(string)
	init, _ := e["init"].(map[string]any)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s: %s: %s refused: not a URL", plugin, lb, raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("%s: %s: %s refused: http or https only", plugin, lb, u.String())
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s: %s: %s refused: credentials in the URL", plugin, lb, u.Host)
	}
	if isTelemetryPath(u.EscapedPath()) {
		return nil, fmt.Errorf("%s: %s: %s refused: this gocode sends no telemetry", plugin, lb, u.String())
	}
	if a, has := init["auth"]; has && a != nil {
		return nil, fmt.Errorf("%s: %s: unknown auth handle; $.session.authorize() mints one", plugin, lb)
	}
	body, _ := init["body"].(string)
	if len(body) > httpBodyLimit {
		return nil, fmt.Errorf("%s: %s: %s refused: a request body of %d characters is over the %d-character limit", plugin, lb, u.String(), len(body), httpBodyLimit)
	}
	method := "GET"
	if m, ok := init["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}
	client := http.Client{Timeout: httpTimeout}
	if h != nil && h.HTTPClient != nil {
		client = *h.HTTPClient
	}
	if sock, ok := init["socketPath"].(string); ok {
		if sock == "" || !filepath.IsAbs(sock) {
			return nil, fmt.Errorf("%s: %s: socketPath %s refused: takes an absolute path", plugin, lb, sock)
		}
		if isNetworkLocation(sock) {
			return nil, fmt.Errorf("%s: %s: socketPath %s refused: a network location is not reached from here", plugin, lb, sock)
		}
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > httpRedirects {
			return fmt.Errorf("%s: %s: more than %d redirects", plugin, lb, httpRedirects)
		}
		if isTelemetryPath(req.URL.EscapedPath()) {
			return fmt.Errorf("%s: %s: a redirect to %s refused: this gocode sends no telemetry", plugin, lb, req.URL.String())
		}
		return nil
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %v", plugin, lb, err)
	}
	if hm, ok := init["headers"].(map[string]any); ok {
		for k, v := range hm {
			if strings.EqualFold(k, "host") {
				continue
			}
			req.Header.Set(k, fmt.Sprint(v))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %s failed: %v", plugin, lb, u.String(), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %s failed reading the body: %v", plugin, lb, u.String(), err)
	}
	headers := map[string]any{}
	for k, vs := range resp.Header {
		headers[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return map[string]any{
		"status": float64(resp.StatusCode), "ok": resp.StatusCode >= 200 && resp.StatusCode < 300,
		"headers": headers, "text": string(data),
	}, nil
}

// isTelemetryPath: the event-logging path, however encoded.
func isTelemetryPath(p string) bool {
	for i := 0; i < 4; i++ {
		d, err := url.PathUnescape(p)
		if err != nil {
			return true
		}
		norm := strings.TrimRight(strings.ToLower(strings.ReplaceAll(d, `\`, "/")), "/")
		for strings.Contains(norm, "//") {
			norm = strings.ReplaceAll(norm, "//", "/")
		}
		if norm == telemetryBatchPath {
			return true
		}
		if d == p {
			return false
		}
		p = d
	}
	return false
}

// --- store ----------------------------------------------------------------------

// store is the plugin's own key/value store, a JSON file in its data folder.
func (h *Host) store(s *seat, op string, e map[string]any) (any, error) {
	if s.dataDir == "" {
		return nil, fmt.Errorf("%s: $.store.%s: the plugin has no data folder here", s.name, op)
	}
	var mu *sync.Mutex
	if h != nil {
		mu = &h.storeMu
	} else {
		mu = &sync.Mutex{}
	}
	mu.Lock()
	defer mu.Unlock()
	file := filepath.Join(s.dataDir, "store.json")
	data := map[string]any{}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &data)
	}
	key, _ := e["key"].(string)
	switch op {
	case "get":
		return data[key], nil
	case "keys":
		keys := make([]any, 0, len(data))
		for _, k := range sortedKeysAny(data) {
			keys = append(keys, k)
		}
		return keys, nil
	case "set":
		data[key] = e["value"]
	case "delete":
		delete(data, key)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return nil, err
	}
	return nil, os.WriteFile(file, b, 0o600)
}

func sortedKeysAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
