package agent

import (
	"fmt"
	"sort"
	"strings"
)

// Deferred tool loading.
//
// A tool schema marked Deferred is not sent as a callable definition until
// the model loads it: the ToolSearch tool answers with the matched names
// (ContentBlock.LoadedTools on its tool_result), and from the next request on
// those tools are sent in full, appended after the always-loaded tools in the
// order they were first loaded, so the head of the tools array never changes.
// The set of loaded tools is derived from the conversation itself, so it
// survives turns, resume and (through the compact summary's LoadedTools)
// compaction without any side state.

// ToolSearchName is the name of the tool that loads deferred tools.
const ToolSearchName = "ToolSearch"

// LoadedTools returns the deferred tool names the conversation has loaded,
// unique, in the order they were first loaded.
func LoadedTools(messages []Message) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range messages {
		for _, b := range m.Content {
			for _, n := range b.LoadedTools {
				if n != "" && !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// SelectTools splits the tool schemas for one request.
//
// send is what a provider offers as callable definitions: every non-deferred
// tool in catalog order, followed by the deferred tools the conversation
// has loaded, in load order. deferred is the remaining (not loaded) deferred
// tools, in catalog order; their names are announced to the model and
// ToolSearch loads them.
//
// When no tool is deferred, ToolSearch has nothing to search and is left out.
// When tools are deferred but ToolSearch is not among the schemas (a subagent
// whose tool list does not include it), deferral is off for the request and
// every tool is sent in full.
func SelectTools(all []ToolSchema, messages []Message) (send, deferred []ToolSchema) {
	hasDeferred, hasSearch := false, false
	for _, t := range all {
		if t.Deferred {
			hasDeferred = true
		}
		if t.Name == ToolSearchName && !t.Deferred {
			hasSearch = true
		}
	}
	if !hasDeferred {
		if !hasSearch {
			return all, nil
		}
		send = make([]ToolSchema, 0, len(all))
		for _, t := range all {
			if t.Name != ToolSearchName {
				send = append(send, t)
			}
		}
		return send, nil
	}
	if !hasSearch {
		send = make([]ToolSchema, len(all))
		for i, t := range all {
			t.Deferred = false
			send[i] = t
		}
		return send, nil
	}
	byName := map[string]int{}
	for i, t := range all {
		if t.Deferred {
			if _, dup := byName[t.Name]; !dup {
				byName[t.Name] = i
			}
		}
	}
	loaded := map[string]bool{}
	send = make([]ToolSchema, 0, len(all))
	for _, t := range all {
		if !t.Deferred {
			send = append(send, t)
		}
	}
	for _, n := range LoadedTools(messages) {
		if i, ok := byName[n]; ok && !loaded[n] {
			loaded[n] = true
			send = append(send, all[i])
		}
	}
	for _, t := range all {
		if t.Deferred && !loaded[t.Name] {
			deferred = append(deferred, t)
		}
	}
	return send, deferred
}

// loadedNowNote follows the error of a deferred tool called before it was
// loaded: the call ran without the model having seen the tool's definition,
// which is sent from the next request on.
func loadedNowNote(name string) string {
	return fmt.Sprintf("(%s was called before it was loaded. Its full definition is loaded now and comes with your next request: "+
		"check its parameters there before calling it again.)", name)
}

// Deferred-tool reminder texts.
const (
	deferredAddedLead = "The following deferred tools are now available via " + ToolSearchName +
		". Their schemas are NOT loaded yet: use " +
		ToolSearchName + " with query \"select:<name>[,<name>...]\" to load the schemas of the ones you need before calling them:"
	deferredGone = "Do not search for them — " + ToolSearchName + " will return no match"
	// deferredGroupLimit is how many disconnected MCP tools are listed one per
	// line before they are summarized per server.
	deferredGroupLimit = 30
)

// AnnounceDeferredTools tells the model which deferred tools exist. It
// compares the deferred tools among tools with the set the conversation last
// announced and, when they differ, appends a
// system-reminder naming the added and removed tools to the trailing user
// turn. The reminder's text block records the new set (AnnouncedTools), so
// each name is announced once; a tool stays announced after it is loaded.
// messages is returned unchanged when nothing changed.
func AnnounceDeferredTools(messages []Message, tools []ToolSchema) []Message {
	if !hasDeferredSearch(tools) {
		// No deferral (feature off, nothing deferred, or no ToolSearch).
		return messages
	}
	current := map[string]bool{}
	for _, t := range tools {
		if t.Deferred {
			current[t.Name] = true
		}
	}
	prev, _ := lastAnnounced(messages)
	var added, removed []string
	for n := range current {
		if !prev[n] {
			added = append(added, n)
		}
	}
	for n := range prev {
		if !current[n] {
			removed = append(removed, n)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return messages
	}
	sort.Strings(added)
	sort.Strings(removed)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, deferredAddedLead+"\n"+strings.Join(added, "\n"))
	}
	var mcpGone, otherGone []string
	for _, n := range removed {
		if strings.HasPrefix(n, "mcp__") {
			mcpGone = append(mcpGone, n)
		} else {
			otherGone = append(otherGone, n)
		}
	}
	if len(mcpGone) > deferredGroupLimit {
		parts = append(parts, fmt.Sprintf("%d deferred tools are no longer available (MCP server disconnected): %s. %s.",
			len(mcpGone), groupMCPNames(mcpGone), deferredGone))
	} else if len(mcpGone) > 0 {
		parts = append(parts, "The following deferred tools are no longer available (their MCP server disconnected). "+
			deferredGone+":\n"+strings.Join(mcpGone, "\n"))
	}
	if len(otherGone) > 0 {
		parts = append(parts, "The following deferred tools are no longer available in this session. "+
			deferredGone+":\n"+strings.Join(otherGone, "\n"))
	}
	snapshot := make([]string, 0, len(current))
	for n := range current {
		snapshot = append(snapshot, n)
	}
	sort.Strings(snapshot)
	block := TextBlock("<system-reminder>\n" + strings.Join(parts, "\n\n") + "\n</system-reminder>")
	block.AnnouncedTools = snapshot
	return appendToUserTurn(messages, []ContentBlock{block})
}

// hasDeferredSearch reports whether tools carry deferred tools and ToolSearch.
func hasDeferredSearch(tools []ToolSchema) bool {
	d, s := false, false
	for _, t := range tools {
		d = d || t.Deferred
		s = s || (t.Name == ToolSearchName && !t.Deferred)
	}
	return d && s
}

// lastAnnounced returns the deferred tool set recorded by the latest
// reminder in messages.
func lastAnnounced(messages []Message) (map[string]bool, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		c := messages[i].Content
		for j := len(c) - 1; j >= 0; j-- {
			if c[j].AnnouncedTools != nil {
				set := map[string]bool{}
				for _, n := range c[j].AnnouncedTools {
					set[n] = true
				}
				return set, true
			}
		}
	}
	return map[string]bool{}, false
}

// groupMCPNames summarizes MCP tool names per server ("mcp__srv__* (12)"),
// sorted by server.
func groupMCPNames(names []string) string {
	counts := map[string]int{}
	for _, n := range names {
		key := n
		if parts := strings.SplitN(n, "__", 3); len(parts) == 3 {
			key = parts[0] + "__" + parts[1] + "__*"
		}
		counts[key]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		if counts[k] > 1 {
			out[i] = fmt.Sprintf("%s (%d)", k, counts[k])
		} else {
			out[i] = k
		}
	}
	return strings.Join(out, ", ")
}
