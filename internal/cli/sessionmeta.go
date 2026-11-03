package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/umairge66/gocode/internal/conversation"
)

// This file implements /rename and /tag: both append a meta line to the
// session transcript (sessionstore meta.go), so they work the same in the
// interactive host and in -p.

// sessionMetaRecorder is the meta seam of the conversation's session store
// (sessionstore.Session).
type sessionMetaRecorder interface {
	ID() string
	Rename(title string) error
	CustomTitle() string
	Tags() []string
	SetTags(tags []string, latest string) error
}

// sessionMetaOf returns conv's session store when it records meta lines.
func sessionMetaOf(conv *conversation.Conversation) (sessionMetaRecorder, bool) {
	if conv == nil || conv.Store == nil {
		return nil, false
	}
	rec, ok := conv.Store.(sessionMetaRecorder)
	return rec, ok && rec != nil
}

// sessionMetaUnavailable is the reply when the session is not persisted.
const sessionMetaUnavailable = "Session persistence is disabled; this conversation is not saved, so it cannot be renamed or tagged."

// renameCommand runs "/rename [title]": with a title it names the session;
// bare, it reports the current name.
func renameCommand(conv *conversation.Conversation, args string) string {
	rec, ok := sessionMetaOf(conv)
	if !ok {
		return sessionMetaUnavailable
	}
	title := strings.Join(strings.Fields(args), " ")
	if title == "" {
		if cur := rec.CustomTitle(); cur != "" {
			return fmt.Sprintf("This conversation is named %q. Use /rename <title> to change it.", cur)
		}
		return "Usage: /rename <title>"
	}
	if err := rec.Rename(title); err != nil {
		return "Could not rename the conversation: " + err.Error()
	}
	return fmt.Sprintf("Renamed the conversation to %q.", title)
}

// tagCommand runs "/tag": bare lists the tags, "<name>" toggles one,
// "--remove <name>" removes one and "--clear" removes them all.
func tagCommand(conv *conversation.Conversation, args string) string {
	rec, ok := sessionMetaOf(conv)
	if !ok {
		return sessionMetaUnavailable
	}
	tags := rec.Tags()
	fields := strings.Fields(args)
	if len(fields) == 0 {
		if len(tags) == 0 {
			return "This conversation has no tags. Use /tag <name> to add one."
		}
		return "Tags: " + strings.Join(tags, ", ")
	}
	set := func(next []string, latest, done string) string {
		if err := rec.SetTags(next, latest); err != nil {
			return "Could not update the tags: " + err.Error()
		}
		return done
	}
	switch fields[0] {
	case "--clear":
		if len(tags) == 0 {
			return "This conversation has no tags."
		}
		return set(nil, "", "Removed all tags.")
	case "--remove", "-r":
		if len(fields) < 2 {
			return "Usage: /tag --remove <name>"
		}
		name := strings.Join(fields[1:], " ")
		if !slices.Contains(tags, name) {
			return fmt.Sprintf("This conversation has no tag %q.", name)
		}
		return set(slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == name }), "", fmt.Sprintf("Removed tag %q.", name))
	}
	name := strings.Join(fields, " ")
	if slices.Contains(tags, name) {
		return set(slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == name }), "", fmt.Sprintf("Removed tag %q.", name))
	}
	return set(append(slices.Clone(tags), name), name, fmt.Sprintf("Tagged the conversation %q.", name))
}
