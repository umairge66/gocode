package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The no-progress detector complements the exact-repeat streak: it catches
// the model re-running calls that are equivalent but not byte-identical
// (a re-ordered `&&` chain, a different echoed marker, another timeout) and
// that keep returning the same result while nothing else changes.
//
// Each executed call gets a similarity key (SimilarityKey) and its result an
// output fingerprint (OutputFingerprint). Per key the detector remembers the
// last fingerprint and how many times in a row (counting only that key's own
// calls, so A,B,A,B,A counts each key separately) the key returned it. A
// different fingerprint restarts that key's count; a file-changing call
// (progressTools) or a new user message forgets every key.
//
// Because it judges results, the detector acts after a call has run: from
// LoopGuardConfig.WarnAfter equivalent results on, the result gets a note; at
// noProgressStopFactor × StopAfter the run ends with ReasonRepeatedToolCalls
// once the turn's results are recorded. Polling tools use thresholds
// pollingFactor times higher, as for the exact-repeat streak.

// noProgressStopFactor multiplies StopAfter for the no-progress stop: with
// the defaults a run ends after 16 equivalent calls with unchanged results.
const noProgressStopFactor = 2

// progressTools change files or the task checklist; a successful call to one
// of them counts as progress and forgets every tracked key.
var progressTools = map[string]bool{
	"Write":        true,
	"Edit":         true,
	"MultiEdit":    true,
	"NotebookEdit": true,
	"TodoWrite":    true,
}

// incidentalFields are top-level input fields that do not change what a call
// does, only how it is run or labelled; similarity keys ignore them.
var incidentalFields = []string{"timeout", "description", "run_in_background", "dangerouslyDisableSandbox"}

// noProgressGuard holds the per-key state of the no-progress detector.
type noProgressGuard struct {
	keys map[string]*noProgressKey
}

type noProgressKey struct {
	fingerprint string
	count       int
}

// reset forgets every key (progress was made or a new user message arrived).
func (g *noProgressGuard) reset() { g.keys = nil }

// record notes one executed call and its result and returns how many times in
// a row that call's key has now produced the same result (1 for a new key or
// a changed result). It returns 0 for a call that counts as progress.
func (g *noProgressGuard) record(name string, input json.RawMessage, output string, isError bool) int {
	if progressTools[name] && !isError {
		g.reset()
		return 0
	}
	key, markers := similarity(name, input)
	fp := fingerprint(output, markers)
	if g.keys == nil {
		g.keys = map[string]*noProgressKey{}
	}
	k := g.keys[key]
	if k == nil || k.fingerprint != fp {
		g.keys[key] = &noProgressKey{fingerprint: fp, count: 1}
		return 1
	}
	k.count++
	return k.count
}

// similarity returns the similarity key of a call and the marker texts
// printed by the echo segments its command normalisation dropped.
func similarity(name string, input json.RawMessage) (string, []string) {
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil || obj == nil {
		return name + "\x00" + canonicalJSON(input), nil
	}
	for _, f := range incidentalFields {
		delete(obj, f)
	}
	var markers []string
	if cmd, ok := obj["command"].(string); ok && name == "Bash" {
		obj["command"], markers = normalizeCommand(cmd)
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return name + "\x00" + canonicalJSON(input), nil
	}
	return name + "\x00" + canonicalJSON(raw), markers
}

func normalizeCommand(cmd string) (string, []string) {
	var segs []string
	if strings.Contains(cmd, "<<") {
		segs = []string{cmd}
	} else {
		segs = splitTopLevel(cmd)
	}
	var kept, markers []string
	for _, s := range segs {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			continue
		}
		if marker, ok := markerSegment(s); ok {
			if marker != "" {
				markers = append(markers, marker)
			}
			continue
		}
		kept = append(kept, s)
	}
	sort.Strings(kept)
	return strings.Join(kept, "\x1f"), markers
}

// splitTopLevel splits a command at `&&`, `||`, `;` and newlines that are not
// inside quotes or parentheses.
func splitTopLevel(cmd string) []string {
	var (
		out    []string
		cur    strings.Builder
		quote  byte
		depth  int
		escape bool
	)
	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escape {
			escape = false
			cur.WriteByte(c)
			continue
		}
		if c == '\\' && quote != '\'' {
			escape = true
			cur.WriteByte(c)
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0 && (c == ';' || c == '\n'):
			flush()
			continue
		case depth == 0 && (c == '&' || c == '|') && i+1 < len(cmd) && cmd[i+1] == c:
			flush()
			i++
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// markerSegment reports whether a (whitespace-collapsed) segment only prints
// a literal marker, and returns the text it prints ("" for true and :).
func markerSegment(s string) (string, bool) {
	if strings.ContainsAny(s, "$`<>|&(){}") {
		return "", false
	}
	words := shellWords(s)
	if len(words) == 0 {
		return "", false
	}
	switch words[0] {
	case "true", ":":
		return "", true
	case "echo":
		args := words[1:]
		for len(args) > 0 && (args[0] == "-n" || args[0] == "-e" || args[0] == "-E") {
			args = args[1:]
		}
		return strings.TrimSpace(strings.Join(args, " ")), true
	case "printf":
		text := strings.Join(words[1:], " ")
		text = strings.ReplaceAll(text, `\n`, "\n")
		return strings.TrimSpace(text), true
	}
	return "", false
}

// shellWords splits a segment into words, removing single and double quotes.
func shellWords(s string) []string {
	var (
		words []string
		cur   strings.Builder
		quote byte
		inTok bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inTok = c, true
		case c == ' ':
			if inTok {
				words = append(words, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if inTok {
		words = append(words, cur.String())
	}
	return words
}

var (
	reUUID     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reISODate  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	reMonthDay = regexp.MustCompile(`\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? +\d{1,2}\b`)
	reClock    = regexp.MustCompile(`\b([01]?\d|2[0-3]):[0-5]\d(:[0-5]\d)?(\.\d+)?\b`)
	reDuration = regexp.MustCompile(`\b\d+(\.\d+)?(ns|us|µs|ms|s)\b|\b\d+m\d+(\.\d+)?s\b`)
	reHexID    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
)

func fingerprint(output string, markers []string) string {
	drop := map[string]bool{}
	for _, m := range markers {
		for _, line := range strings.Split(m, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				drop[line] = true
			}
		}
	}
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if drop[strings.TrimSpace(line)] {
			continue
		}
		kept = append(kept, line)
	}
	s := strings.TrimSpace(strings.Join(kept, "\n"))
	s = reUUID.ReplaceAllString(s, "<id>")
	s = reISODate.ReplaceAllString(s, "<date>")
	s = reMonthDay.ReplaceAllString(s, "<date>")
	s = maskClock(s)
	s = reDuration.ReplaceAllString(s, "<dur>")
	s = reHexID.ReplaceAllStringFunc(s, func(h string) string {
		if strings.ContainsAny(h, "0123456789") && strings.ContainsAny(h, "abcdefABCDEF") {
			return "<id>"
		}
		return h
	})
	return s
}

// maskClock replaces clock times with <time>, leaving colon-separated
// numbers that are part of a longer chain alone (file.go:12:34: is a source
// position, not a time).
func maskClock(s string) string {
	locs := reClock.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if (start > 0 && s[start-1] == ':') || (end < len(s) && s[end] == ':') {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString("<time>")
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// NoProgressNote is the note appended to the result of a call equivalent to
// earlier ones that returned the same result count times with nothing
// changed in between.
func NoProgressNote(tool string, count int) string {
	return fmt.Sprintf("Note: you have now run %s calls equivalent to this one %d times with the same result and no changes in between. "+
		"If the task is complete, stop and give your final answer; otherwise try a different approach.", tool, count)
}

// NoProgressStopMessage is the reason a run ends with when equivalent calls
// returned the same result count times.
func NoProgressStopMessage(tool string, count int) string {
	return fmt.Sprintf("Stopped: equivalent %s calls were repeated %d times with no progress", tool, count)
}

// progressVerdict is the no-progress detector's decision on one turn's
// results.
type progressVerdict struct {
	notes     map[int]string
	stop      bool
	stopTool  string
	stopCount int
}

// checkProgress feeds a turn's executed calls and their results to the
// no-progress detector. calls and results are index-aligned; skip holds the
// indexes that already carry an exact-repeat note (they get no second note).
func (l *Loop) checkProgress(calls, results []ContentBlock, skip map[int]string) progressVerdict {
	var v progressVerdict
	cfg := l.guardConfig()
	if cfg.WarnAfter <= 0 && cfg.StopAfter <= 0 {
		return v
	}
	if l.progress == nil {
		l.progress = &noProgressGuard{}
	}
	for i, c := range calls {
		if i >= len(results) {
			break
		}
		count := l.progress.record(c.Name, c.Input, results[i].Content, results[i].IsError)
		if count == 0 {
			continue
		}
		warn, stop := cfg.thresholds(c.Name)
		stop *= noProgressStopFactor
		if warn > 0 && count >= warn {
			if _, noted := skip[i]; !noted {
				if v.notes == nil {
					v.notes = map[int]string{}
				}
				v.notes[i] = NoProgressNote(c.Name, count)
			}
		}
		if stop > 0 && count >= stop && !v.stop {
			v.stop, v.stopTool, v.stopCount = true, c.Name, count
		}
	}
	if v.stop {
		l.progress.reset()
	}
	return v
}
