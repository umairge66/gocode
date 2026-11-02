package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/term"

	"github.com/umairge66/gocode/internal/auth"
)

// maxPipedKeyBytes bounds what login reads from a piped stdin.
const maxPipedKeyBytes = 64 << 10

// authProviderArg resolves the provider for login / logout: the one named in
// args, or the active provider. More than one argument is refused, so a key
// pasted on the command line is never accepted (or echoed back).
func authProviderArg(env *Env, d *DispatchDefaults, verb string, args []string) (string, bool) {
	switch len(args) {
	case 0:
		p, err := resolveProvider(env, d)
		if err != nil {
			fmt.Fprintf(env.Stderr, "gocode auth %s: %v\n", verb, err)
			return "", false
		}
		return p, true
	case 1:
		for _, p := range validProviders() {
			if strings.EqualFold(args[0], p) {
				return p, true
			}
		}
		// The argument is not echoed: it may be a key typed in the wrong place.
		fmt.Fprintf(env.Stderr, "gocode auth %s: unknown provider. Available: %s.\n", verb, strings.Join(validProviders(), ", "))
		if verb == "login" {
			fmt.Fprintln(env.Stderr, "The key is never given as an argument: gocode asks for it, or reads it from stdin.")
		}
		return "", false
	}
	fmt.Fprintf(env.Stderr, "gocode auth %s: too many arguments. Usage: gocode auth %s [provider]\n", verb, verb)
	if verb == "login" {
		fmt.Fprintln(env.Stderr, "The key is never given as an argument (it would end up in your shell history): gocode asks for it, or reads it from stdin.")
	}
	return "", false
}

// runAuthLogin saves a provider key to auth.json.
func runAuthLogin(env *Env, d *DispatchDefaults, args []string) int {
	prov, ok := authProviderArg(env, d, "login", args)
	if !ok {
		return 1
	}
	key, err := readAuthKey(env, prov)
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode auth login: %v\n", err)
		return 1
	}
	path := authFilePath(env)
	if err := auth.SaveProviderKey(path, auth.Provider(prov), key); err != nil {
		fmt.Fprintf(env.Stderr, "gocode auth login: could not save the key: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "Saved the %s key to %s\n", prov, displayHomePath(env, path))
	if v := setProviderEnvVar(env, prov); v != "" {
		fmt.Fprintf(env.Stdout, "Note: %s is set in your environment and wins over the saved key. Unset it (and remove it from your shell profile) to use the saved key.\n", v)
	}
	return 0
}

// runAuthLogout removes a provider key from auth.json.
func runAuthLogout(env *Env, d *DispatchDefaults, args []string) int {
	prov, ok := authProviderArg(env, d, "logout", args)
	if !ok {
		return 1
	}
	path := authFilePath(env)
	removed, err := auth.RemoveProviderKey(path, auth.Provider(prov))
	if err != nil {
		fmt.Fprintf(env.Stderr, "gocode auth logout: could not update %s: %v\n", displayHomePath(env, path), err)
		return 1
	}
	if removed {
		fmt.Fprintf(env.Stdout, "Removed the saved %s key from %s\n", prov, displayHomePath(env, path))
	} else {
		fmt.Fprintf(env.Stdout, "No %s key is saved in %s\n", prov, displayHomePath(env, path))
	}
	if v := setProviderEnvVar(env, prov); v != "" {
		fmt.Fprintf(env.Stdout, "Note: %s is still set in your environment, so gocode keeps using that key. Remove it where it is set (for example your shell profile).\n", v)
	}
	return 0
}

// setProviderEnvVar is the first of the provider's key variables that is set.
func setProviderEnvVar(env *Env, prov string) string {
	for _, v := range auth.ProviderEnvVars(auth.Provider(prov)) {
		if strings.TrimSpace(env.Getenv(v)) != "" {
			return v
		}
	}
	return ""
}

// readAuthKey reads the key from a hidden prompt when stdin is a terminal,
// otherwise from stdin (one line).
func readAuthKey(env *Env, prov string) (string, error) {
	if env.Stdin == nil {
		return "", errors.New("no input to read the key from")
	}
	var raw string
	if f, ok := env.Stdin.(*os.File); ok && env.StdinTTY {
		fmt.Fprintf(env.Stderr, "Paste your %s API key (input is hidden): ", prov)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("could not read the key: %w", err)
		}
		raw = string(b)
	} else {
		b, err := io.ReadAll(io.LimitReader(env.Stdin, maxPipedKeyBytes+1))
		if err != nil {
			return "", fmt.Errorf("could not read the key from stdin: %w", err)
		}
		if len(b) > maxPipedKeyBytes {
			return "", errors.New("stdin holds more than a key")
		}
		var lines []string
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		sc.Buffer(make([]byte, 0, 4096), maxPipedKeyBytes)
		for sc.Scan() {
			if t := strings.TrimSpace(sc.Text()); t != "" {
				lines = append(lines, t)
			}
		}
		if len(lines) > 1 {
			return "", errors.New("stdin holds more than one line; pipe in just the key")
		}
		if len(lines) == 1 {
			raw = lines[0]
		}
	}
	return checkAuthKey(raw)
}

// checkAuthKey trims the key and refuses an empty one or one with spaces or
// control characters (a paste that caught more than the key).
func checkAuthKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errors.New("no key entered")
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("the key contains spaces or control characters; check what was pasted")
		}
	}
	return key, nil
}

// displayHomePath shows path with the home directory as "~".
func displayHomePath(env *Env, path string) string {
	home := settingsContext(env).HomeDir
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}
