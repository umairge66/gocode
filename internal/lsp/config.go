package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Defaults for the per-server timing and restart settings.
const (
	DefaultRequestTimeoutMS  = 60000
	DefaultMaxRestarts       = 3
	DefaultShutdownTimeoutMS = 5000
	defaultLanguageID        = "plaintext"
	maxRequestTimeoutMS      = 2147483647
)

// ServerConfig is one language server entry of a `.lsp.json` file or a
// plugin's `lspServers`: how to launch it and which files it handles.
type ServerConfig struct {
	// Command is the executable. It may contain spaces only when it is an
	// absolute path; arguments go in Args.
	Command string `json:"command"`
	// Args are the command-line arguments.
	Args []string `json:"args,omitempty"`
	// ExtensionToLanguage maps a file extension (".ts") to an LSP language
	// ID ("typescript"). It routes files to the server and must not be empty.
	ExtensionToLanguage map[string]string `json:"extensionToLanguage"`
	// Transport is "stdio" (the default) or "socket"; only stdio runs.
	Transport string `json:"transport,omitempty"`
	// Env are extra environment variables for the server process.
	Env map[string]string `json:"env,omitempty"`
	// InitializationOptions is sent in the initialize request.
	InitializationOptions json.RawMessage `json:"initializationOptions,omitempty"`
	// Settings are pushed with workspace/didChangeConfiguration and answer
	// workspace/configuration requests.
	Settings json.RawMessage `json:"settings,omitempty"`
	// WorkspaceFolder overrides the workspace root sent to the server.
	WorkspaceFolder string `json:"workspaceFolder,omitempty"`
	// StartupTimeout bounds initialization, in milliseconds.
	StartupTimeout *int `json:"startupTimeout,omitempty"`
	// ShutdownTimeout bounds the graceful stop, in milliseconds.
	ShutdownTimeout *int `json:"shutdownTimeout,omitempty"`
	// RequestTimeout bounds one request, in milliseconds (default 60000).
	RequestTimeout *int `json:"requestTimeout,omitempty"`
	// RestartOnCrash false keeps a crashed server down.
	RestartOnCrash *bool `json:"restartOnCrash,omitempty"`
	// MaxRestarts caps consecutive crash recoveries (default 3).
	MaxRestarts *int `json:"maxRestarts,omitempty"`
	// Diagnostics false keeps navigation but stops the server's
	// diagnostics from being reported after edits.
	Diagnostics *bool `json:"diagnostics,omitempty"`

	// Plugin is the plugin that declared the server ("" for none).
	Plugin string `json:"-"`
}

// Config maps server names to their configuration: the body of a `.lsp.json`
// file or of an inline `lspServers` object.
type Config map[string]ServerConfig

// RequestTimeoutMS is the effective per-request timeout.
func (c ServerConfig) RequestTimeoutMS() int {
	if c.RequestTimeout != nil && *c.RequestTimeout > 0 {
		return *c.RequestTimeout
	}
	return DefaultRequestTimeoutMS
}

// ShutdownTimeoutMS is the effective graceful-stop bound.
func (c ServerConfig) ShutdownTimeoutMS() int {
	if c.ShutdownTimeout != nil && *c.ShutdownTimeout > 0 {
		return *c.ShutdownTimeout
	}
	return DefaultShutdownTimeoutMS
}

// MaxRestartCount is the effective crash-recovery cap.
func (c ServerConfig) MaxRestartCount() int {
	if c.MaxRestarts != nil {
		return *c.MaxRestarts
	}
	return DefaultMaxRestarts
}

// DiagnosticsEnabled reports whether the server's diagnostics are reported
// after edits (the default).
func (c ServerConfig) DiagnosticsEnabled() bool {
	return c.Diagnostics == nil || *c.Diagnostics
}

// HasSettings reports whether the server has a settings object.
func (c ServerConfig) HasSettings() bool {
	s := bytes.TrimSpace(c.Settings)
	return len(s) > 0 && string(s) != "null"
}

// LanguageID returns the language ID configured for a file extension
// (matched exactly, then lower-cased), or "plaintext".
func (c ServerConfig) LanguageID(ext string) string {
	if id, ok := c.ExtensionToLanguage[ext]; ok {
		return id
	}
	lower := strings.ToLower(ext)
	for k, id := range c.ExtensionToLanguage {
		if strings.ToLower(k) == lower {
			return id
		}
	}
	return defaultLanguageID
}

// Validate checks one server entry: a command (no spaces unless absolute), a
// non-empty extensionToLanguage, a known transport, and in-range numbers.
func (c ServerConfig) Validate(name string) error {
	if c.Command == "" {
		return fmt.Errorf("Server %s missing required 'command' field", name)
	}
	if strings.Contains(c.Command, " ") && !isAbsCommand(c.Command) {
		return fmt.Errorf("Server %s: Command should not contain spaces. Use args array for arguments.", name)
	}
	if len(c.ExtensionToLanguage) == 0 {
		return fmt.Errorf("Server %s missing required 'extensionToLanguage' field", name)
	}
	switch c.Transport {
	case "", "stdio", "socket":
	default:
		return fmt.Errorf("Server %s: transport must be \"stdio\" or \"socket\"", name)
	}
	positive := func(field string, v *int, max int) error {
		if v != nil && (*v <= 0 || *v > max) {
			return fmt.Errorf("Server %s: %s must be a positive integer", name, field)
		}
		return nil
	}
	if err := positive("startupTimeout", c.StartupTimeout, maxRequestTimeoutMS); err != nil {
		return err
	}
	if err := positive("shutdownTimeout", c.ShutdownTimeout, maxRequestTimeoutMS); err != nil {
		return err
	}
	if err := positive("requestTimeout", c.RequestTimeout, maxRequestTimeoutMS); err != nil {
		return err
	}
	if c.MaxRestarts != nil && *c.MaxRestarts < 0 {
		return fmt.Errorf("Server %s: maxRestarts must be a non-negative integer", name)
	}
	return nil
}

// isAbsCommand reports whether a command is an absolute path (a leading
// slash, or a Windows drive path).
func isAbsCommand(cmd string) bool {
	if strings.HasPrefix(cmd, "/") {
		return true
	}
	return filepath.IsAbs(cmd)
}

// ParseServer decodes and validates one server entry. Unknown fields are
// rejected.
func ParseServer(name string, data []byte) (ServerConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var sc ServerConfig
	if err := dec.Decode(&sc); err != nil {
		return ServerConfig{}, fmt.Errorf("Server %s: %v", name, err)
	}
	if err := sc.Validate(name); err != nil {
		return ServerConfig{}, err
	}
	return sc, nil
}

// ParseConfig decodes and validates a `.lsp.json` body: an object mapping
// server names to entries. One invalid entry makes the whole file invalid.
func ParseConfig(data []byte) (Config, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("Failed to parse JSON: %v", err)
	}
	cfg := make(Config, len(raw))
	for _, name := range sortedKeys(raw) {
		sc, err := ParseServer(name, raw[name])
		if err != nil {
			return nil, err
		}
		cfg[name] = sc
	}
	return cfg, nil
}

// Routing resolves the extension -> server table. Extensions are compared
// lower-cased; when two servers claim an extension the first (by name order)
// keeps it and the conflict is reported as a warning.
func (c Config) Routing() (route map[string]string, warnings []string) {
	route = make(map[string]string)
	for _, name := range sortedKeys(c) {
		for ext := range c[name].ExtensionToLanguage {
			lower := strings.ToLower(ext)
			if owner, taken := route[lower]; taken {
				if owner != name {
					warnings = append(warnings, fmt.Sprintf("LSP: extension %s already handled by %q; %q will not be used for %s files", lower, owner, name, lower))
				}
				continue
			}
			route[lower] = name
		}
	}
	sort.Strings(warnings)
	return route, warnings
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
