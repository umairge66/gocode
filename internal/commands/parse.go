package commands

import "strings"

// ParsedInput is the result of splitting a raw `/command args...` line.
type ParsedInput struct {
	// Name is the command token: everything after the leading '/' up to the
	// first space. It may be namespaced (contain ':'), e.g. "project:foo".
	Name string
	// Args is the remainder of the line after the first space (empty when the
	// line is just `/name`). It is NOT trimmed.
	Args string
}

// IsSlashCommand reports whether s structurally looks like a slash command:
// it begins with '/' and has a non-empty name token.
func IsSlashCommand(s string) bool {
	if !strings.HasPrefix(s, "/") {
		return false
	}
	var name string
	if i := strings.IndexByte(s, ' '); i == -1 {
		name = s[1:]
	} else {
		name = s[1:i]
	}
	return name != ""
}

// ParseInput splits a raw line into a command name and argument string. It
// returns ok=false when the line is not a slash command.
//
// The split is purely positional: the name is the run of characters after '/'
// up to the first ASCII space, and the args are everything after that space.
// A line with no space (`/clear`) yields empty args.
func ParseInput(s string) (ParsedInput, bool) {
	if !IsSlashCommand(s) {
		return ParsedInput{}, false
	}
	if i := strings.IndexByte(s, ' '); i == -1 {
		return ParsedInput{Name: s[1:], Args: ""}, true
	} else {
		return ParsedInput{Name: s[1:i], Args: s[i+1:]}, true
	}
}
