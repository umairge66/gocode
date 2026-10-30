package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
)

// LoopGuardConfig sets when the loop reacts to the model repeating the exact
// same tool call (same tool name, same input) several times in a row. The
// same thresholds drive the no-progress detector (see noprogress.go), which
// notes from WarnAfter equivalent calls with an unchanged result and stops at
// twice StopAfter.
//
// WarnAfter is the streak length at which a note is appended to the tool
// result asking the model to stop or change approach; the note repeats, with
// the updated count, on every further repeat. StopAfter is the streak length
// at which the call is not executed and the run ends with
// ReasonRepeatedToolCalls. Zero disables that stage; both zero disables the
// guard.
type LoopGuardConfig struct {
	WarnAfter int
	StopAfter int
}

// DefaultLoopGuard is the guard a Loop uses when its LoopGuard field is nil.
var DefaultLoopGuard = LoopGuardConfig{WarnAfter: 3, StopAfter: 8}

// LoopGuardEnv names the environment variable that turns the guard off
// ("off", "0", "false", "disabled").
const LoopGuardEnv = "GOCODE_LOOP_GUARD"

// pollingFactor multiplies both thresholds for tools whose repetition with an
// identical input is a normal way to wait for something (see pollingTools).
const pollingFactor = 4

// pollingTools are the tools that legitimately get called with the same input
// over and over while waiting on a background job, an agent or a page: their
// repeats are still counted, but against thresholds pollingFactor times
// higher.
var pollingTools = map[string]bool{
	"TaskOutput":    true,
	"BashOutput":    true,
	"TaskGet":       true,
	"TaskList":      true,
	"Monitor":       true,
	"Sleep":         true,
	"ListAgents":    true,
	"screenshot":    true,
	"read_page":     true,
	"get_page_text": true,
	"tabs_context":  true,
	"computer":      true,
}

// Validate reports a configuration whose stop stage would fire before (or
// together with) its warn stage, or that uses negative values.
func (c LoopGuardConfig) Validate() error {
	if c.WarnAfter < 0 || c.StopAfter < 0 {
		return fmt.Errorf("warnAfter and stopAfter must be 0 (off) or a positive number")
	}
	if c.WarnAfter > 0 && c.StopAfter > 0 && c.StopAfter <= c.WarnAfter {
		return fmt.Errorf("stopAfter (%d) must be greater than warnAfter (%d)", c.StopAfter, c.WarnAfter)
	}
	return nil
}

// thresholds returns the warn and stop streak lengths for a tool.
func (c LoopGuardConfig) thresholds(tool string) (warn, stop int) {
	warn, stop = c.WarnAfter, c.StopAfter
	if pollingTools[tool] {
		warn, stop = warn*pollingFactor, stop*pollingFactor
	}
	return warn, stop
}

// loopGuard tracks the current streak of identical tool calls of a Loop.
type loopGuard struct {
	sig   string
	count int
}

// reset ends the current streak (a new user message arrived).
func (g *loopGuard) reset() { g.sig, g.count = "", 0 }

// next records a call and returns the length of the streak it extends (1 for
// a call that differs from the previous one).
func (g *loopGuard) next(name string, input json.RawMessage) int {
	sig := CallSignature(name, input)
	if sig == g.sig {
		g.count++
	} else {
		g.sig, g.count = sig, 1
	}
	return g.count
}

// CallSignature identifies a tool call by its tool name and the canonical JSON
// of its input: object keys sorted, insignificant whitespace dropped and
// numbers normalised so that 5000, 5000.0 and 5e3 are the same value. An input
// that is not valid JSON is compared byte for byte.
func CallSignature(name string, input json.RawMessage) string {
	return name + "\x00" + canonicalJSON(input)
}

func canonicalJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return string(trimmed)
	}
	out, err := json.Marshal(normalizeNumbers(v))
	if err != nil {
		return string(trimmed)
	}
	return string(out)
}

// normalizeNumbers rewrites every number in a decoded JSON value to its
// canonical form: integers exactly, other values as their shortest float64
// spelling (json.Marshal already sorts object keys).
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumbers(e)
		}
		return t
	case json.Number:
		r, ok := new(big.Rat).SetString(string(t))
		if !ok {
			return t
		}
		if r.IsInt() {
			return json.Number(r.Num().String())
		}
		f, _ := r.Float64()
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	}
	return v
}

// RepeatedCallNote is the note appended to the result of a call that repeats
// the previous one for the count-th time in a row.
func RepeatedCallNote(tool string, count int) string {
	return fmt.Sprintf("Note: you have now run this exact %s call %d times in a row with the same input. "+
		"If the task is complete, stop and give your final answer; otherwise try a different approach.", tool, count)
}

// RepeatedCallsStopMessage is the reason a run ends with when a call repeats
// count times in a row.
func RepeatedCallsStopMessage(tool string, count int) string {
	return fmt.Sprintf("Stopped: the same %s call was repeated %d times in a row", tool, count)
}

// repeatedCallNotRun answers the call that hit the stop threshold.
func repeatedCallNotRun(tool string, count int) string {
	return fmt.Sprintf("Not run: this exact %s call was repeated %d times in a row with the same input, so the turn was stopped.", tool, count)
}

// skippedAfterRepeatStop answers the calls of the same assistant turn that
// follow the stopped one.
const skippedAfterRepeatStop = "Not run: the turn was stopped because of repeated identical tool calls."

// guardVerdict is the guard's decision on one assistant turn's tool calls.
type guardVerdict struct {
	// notes maps a call index to the note appended to its result.
	notes map[int]string
	// stopAt is the index of the call that hit the stop threshold, or -1.
	stopAt    int
	stopTool  string
	stopCount int
}

// guardConfig returns the effective guard configuration.
func (l *Loop) guardConfig() LoopGuardConfig {
	if l.LoopGuard != nil {
		return *l.LoopGuard
	}
	return DefaultLoopGuard
}

// checkRepeats feeds a turn's tool calls, in order, to the streak tracker and
// decides which results get a note and whether a call must not run.
func (l *Loop) checkRepeats(calls []ContentBlock) guardVerdict {
	v := guardVerdict{stopAt: -1}
	cfg := l.guardConfig()
	if cfg.WarnAfter <= 0 && cfg.StopAfter <= 0 {
		return v
	}
	if l.guard == nil {
		l.guard = &loopGuard{}
	}
	for i, c := range calls {
		count := l.guard.next(c.Name, c.Input)
		warn, stop := cfg.thresholds(c.Name)
		if stop > 0 && count >= stop {
			v.stopAt, v.stopTool, v.stopCount = i, c.Name, count
			// The run ends here; the streak does not carry into a later
			// run on this Loop.
			l.guard.reset()
			return v
		}
		if warn > 0 && count >= warn {
			if v.notes == nil {
				v.notes = map[int]string{}
			}
			v.notes[i] = RepeatedCallNote(c.Name, count)
		}
	}
	return v
}

// resetRepeats ends the current streak and forgets the no-progress state (a
// new run started or a new user message reached the model).
func (l *Loop) resetRepeats() {
	if l.guard != nil {
		l.guard.reset()
	}
	if l.progress != nil {
		l.progress.reset()
	}
}

// appendNote adds a guard note to a tool_result block's text.
func appendNote(b ContentBlock, note string) ContentBlock {
	if b.Content == "" {
		b.Content = note
	} else {
		b.Content += "\n\n" + note
	}
	return b
}
