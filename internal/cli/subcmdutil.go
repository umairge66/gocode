package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Shared plumbing for the local subcommands in doctorcmd.go,
// agentscmd.go, automodecmd.go, sandboxcmd.go, purgecmd.go,
// importconvcmd.go and edithookcmd.go: picking a subcommand's own arguments
// out of the raw argv, a small option scanner, and the y/N
// confirmation prompt used for destructive verbs.

// argsAfter returns the tokens that follow the subcommand path in t. path is
// the verb chain ("purge", or "auto-mode","reset"); each element is matched as
// the next non-flag token. Option tokens that appear before the verb (global
// flags) are dropped. When the path is not found, nil is returned.
func argsAfter(t []string, path ...string) []string {
	i := 0
	for _, want := range path {
		found := false
		for ; i < len(t); i++ {
			if strings.HasPrefix(t[i], "-") {
				continue
			}
			if t[i] == want {
				i++
				found = true
				break
			}
			return nil
		}
		if !found {
			return nil
		}
	}
	return t[i:]
}

// cmdOpts is the result of scanning a subcommand's arguments.
type cmdOpts struct {
	bools  map[string]bool
	values map[string]string
	pos    []string
	// unknown lists option tokens that were not declared.
	unknown []string
}

func (o cmdOpts) has(name string) bool { return o.bools[name] }

func (o cmdOpts) value(name string) (string, bool) {
	v, ok := o.values[name]
	return v, ok
}

// scanOpts scans args for the declared options. boolSpec maps every accepted
// spelling ("-y", "--yes") to the canonical name of a boolean option;
// valueSpec does the same for options that take a value ("--cwd <dir>"), which
// may be given inline (--cwd=dir) or as the next token. A bare "--" ends option
// parsing. Undeclared options are collected in unknown (callers decide whether
// they are an error; global flags such as --debug pass through harmlessly).
func scanOpts(args []string, boolSpec, valueSpec map[string]string) (cmdOpts, error) {
	o := cmdOpts{bools: map[string]bool{}, values: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.pos = append(o.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.pos = append(o.pos, a)
			continue
		}
		key, val, inline := a, "", false
		if eq := strings.Index(a, "="); eq > 0 {
			key, val, inline = a[:eq], a[eq+1:], true
		}
		if name, ok := valueSpec[key]; ok {
			if !inline {
				if i+1 >= len(args) {
					return o, fmt.Errorf("error: option '%s' argument missing", key)
				}
				i++
				val = args[i]
			}
			o.values[name] = val
			continue
		}
		if name, ok := boolSpec[key]; ok && !inline {
			o.bools[name] = true
			continue
		}
		o.unknown = append(o.unknown, a)
	}
	return o, nil
}

// confirmYesNo writes "<question> [y/N] " to stdout and
// reads one line from stdin; only "y" / "yes" (any case) confirm. EOF, a closed
// or missing stdin, or anything else declines.
func confirmYesNo(env *Env, question string) bool {
	return confirmYesNoFrom(env, newStdinReader(env), question)
}

// confirmYesNoFrom is confirmYesNo over a shared stdin reader, for commands
// that prompt more than once (a fresh bufio.Reader per prompt would swallow
// buffered answers meant for the next one).
func confirmYesNoFrom(env *Env, rd *bufio.Reader, question string) bool {
	fmt.Fprintf(env.Stdout, "%s [y/N] ", question)
	line, ok := readAnswer(rd)
	if !ok {
		fmt.Fprintln(env.Stdout)
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// newStdinReader wraps env.Stdin (nil when there is none).
func newStdinReader(env *Env) *bufio.Reader {
	if env.Stdin == nil {
		return nil
	}
	return bufio.NewReader(env.Stdin)
}

// readAnswer reads one line; ok=false on a missing stdin or EOF with no input.
func readAnswer(rd *bufio.Reader) (string, bool) {
	if rd == nil {
		return "", false
	}
	line, err := rd.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// writeJSONLine marshals v compactly and writes it with a trailing newline
// .
func writeJSONLine(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // JSON.stringify does not escape <, > and &
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(w, "{\"error\":%q}\n", err.Error())
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// writeCommandUsage prints a command group's help to stderr when a group (auto-mode, sandbox, project) is run without a subcommand.
func writeCommandUsage(env *Env, name string) {
	cmd := findSubcommand(name)
	if cmd == nil {
		return
	}
	errEnv := *env
	errEnv.Stdout = env.Stderr
	printHelp(&errEnv, cmd, "gocode "+cmd.Name)
}

// pluralize returns the word for a count.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
