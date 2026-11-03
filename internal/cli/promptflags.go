package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/umairge66/gocode/internal/agentsdef"
)

// The session prompt flags:
//
//	--system-prompt <prompt>             replace the default system prompt
//	--system-prompt-file <file>          the same, read from a file
//	--append-system-prompt <prompt>      append to the system prompt
//	--append-system-prompt-file <file>   the same, read from a file
//	--agents <json>                      custom subagents, as a JSON object
//	                                     {"name": {"description", "prompt",
//	                                     "tools", "model", …}}
//
// A replacing prompt stands in for the built-in identity, guidance and
// environment block only: the gocode.md memory, the sandbox and scratchpad
// sections and the skills listing are still added after it, and an appended
// prompt goes last. A prompt and its -file form are mutually exclusive.
// --agents definitions load at the flag tier (above user and project agents,
// below managed ones).

// promptFlagNames are the value-taking flags extractPromptFlags consumes.
var promptFlagNames = map[string]bool{
	"--system-prompt": true, "--system-prompt-file": true,
	"--append-system-prompt": true, "--append-system-prompt-file": true,
	"--agents": true,
}

// extractPromptFlags removes the prompt flags (before a "--" terminator) from
// args and returns the remaining arguments and the raw values by flag name.
func extractPromptFlags(args []string) ([]string, map[string]string) {
	vals := map[string]string{}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		key, val, inline := a, "", false
		if eq := strings.Index(a, "="); eq > 0 {
			key, val, inline = a[:eq], a[eq+1:], true
		}
		if !promptFlagNames[key] {
			rest = append(rest, a)
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				rest = append(rest, a)
				continue
			}
			i++
			val = args[i]
		}
		vals[key] = val
	}
	return rest, vals
}

// applyPromptFlags resolves the prompt flags into d: the -file forms are read,
// a flag given with its -file form is refused, and --agents must be a JSON
// object. The error is the message for stderr.
func applyPromptFlags(d *DispatchDefaults, vals map[string]string) error {
	pick := func(flag string) (string, error) {
		v, hasV := vals[flag]
		file, hasF := vals[flag+"-file"]
		if hasV && hasF {
			return "", fmt.Errorf("Error: Cannot use both %s and %s-file. Please use only one.", flag, flag)
		}
		if !hasF {
			return v, nil
		}
		data, err := os.ReadFile(file)
		if err != nil {
			kind := "System prompt"
			if strings.HasPrefix(flag, "--append") {
				kind = "Append system prompt"
			}
			if os.IsNotExist(err) {
				return "", fmt.Errorf("Error: %s file not found: %s", kind, file)
			}
			return "", fmt.Errorf("Error: Failed to read %s file: %v", strings.ToLower(kind), err)
		}
		return string(data), nil
	}
	var err error
	if d.SystemPrompt, err = pick("--system-prompt"); err != nil {
		return err
	}
	if d.AppendSystemPrompt, err = pick("--append-system-prompt"); err != nil {
		return err
	}
	if raw, ok := vals["--agents"]; ok {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return fmt.Errorf("Error: Invalid JSON provided to --agents: %v", err)
		}
		d.AgentsJSON = raw
	}
	return nil
}

// agentsFlagJSON is the --agents object for agentsdef.LoadOptions.AgentsJSON
// (nil when none was given).
func agentsFlagJSON(s *printSession) []byte {
	if s == nil || strings.TrimSpace(s.AgentsJSON) == "" {
		return nil
	}
	return []byte(s.AgentsJSON)
}

// agentsFlagProblems lists the --agents entries that were skipped (a bad name,
// a missing description), as startup notices.
func agentsFlagProblems(s *printSession) []string {
	data := agentsFlagJSON(s)
	if data == nil {
		return nil
	}
	_, problems := agentsdef.ParseAgentsJSON(data)
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		out = append(out, "--agents: "+p)
	}
	return out
}

// replaceSystemPrompt returns the base system prompt a session starts from:
// the --system-prompt text when one was given, else the built-in prompt.
func replaceSystemPrompt(s *printSession, builtin string) string {
	if s != nil && strings.TrimSpace(s.SystemPrompt) != "" {
		return strings.TrimRight(s.SystemPrompt, "\n")
	}
	return builtin
}

// appendSystemPrompt adds the --append-system-prompt text to the end of the
// session's system prompt.
func appendSystemPrompt(s *printSession, system string) string {
	if s == nil || strings.TrimSpace(s.AppendSystemPrompt) == "" {
		return system
	}
	add := strings.TrimRight(s.AppendSystemPrompt, "\n")
	if system == "" {
		return add
	}
	return strings.TrimRight(system, "\n") + "\n\n" + add
}
