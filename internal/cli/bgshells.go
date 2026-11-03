package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
	"github.com/umairge66/gocode/internal/ui"
)

// wireBackgroundShells connects the session's background shell commands to
// the user: when a main-session command finishes, its outcome is shown as a
// system line and, in an interactive session, a continuation turn is started
// so the model receives the queued task-notification even when the session
// is idle. A turn already in flight takes the notification between its tool
// calls instead, and the continuation then finds nothing left to send.
func (r *sessionRuntime) wireBackgroundShells() {
	if r.conv == nil || r.conv.BackgroundTasks == nil {
		return
	}
	r.conv.BackgroundTasks.SetOnComplete(func(info bgtask.TaskInfo, notified bool) {
		if f := r.bgObserver.Load(); f != nil {
			(*f)(info)
		}
		if !notified {
			return
		}
		r.emit(info.Summary())
		r.triggerResume()
	})
}

// setBackgroundObserver installs f, told about every background command
// that ended (print mode's task_notification records).
func (r *sessionRuntime) setBackgroundObserver(f func(bgtask.TaskInfo)) {
	r.bgObserver.Store(&f)
}

// printBgWaitCeilingEnv bounds how long a -p run waits for its background
// commands after the last turn (milliseconds; 0 waits indefinitely).
const printBgWaitCeilingEnv = "GOCODE_PRINT_BG_WAIT_CEILING_MS"

// defaultPrintBgWaitCeiling is the -p wait bound when the variable is unset.
const defaultPrintBgWaitCeiling = 10 * time.Minute

// printBgWaitCeiling reads the -p background wait bound (0 = no bound).
func printBgWaitCeiling(env *Env) time.Duration {
	v := strings.TrimSpace(envGetenv(env)(printBgWaitCeilingEnv))
	if v == "" {
		return defaultPrintBgWaitCeiling
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return defaultPrintBgWaitCeiling
	}
	return time.Duration(n) * time.Millisecond
}

// awaitBackgroundShells keeps a -p run alive while main-session background
// commands are still running after its last turn. Whenever a completion
// notification is queued, turn runs one more model turn to deliver it
// (returning false ends the wait). The wait ends when nothing is running and
// nothing is queued, when ctx ends, or when the commands are still running
// the ceiling after the last turn: they are then reported on stderr and left
// for session shutdown to stop.
func (r *sessionRuntime) awaitBackgroundShells(ctx context.Context, env *Env, turn func(context.Context) bool) {
	if r == nil || r.conv == nil || r.conv.BackgroundTasks == nil {
		return
	}
	m := r.conv.BackgroundTasks
	q := m.Notifications()
	ceiling := printBgWaitCeiling(env)
	last := time.Now()
	for ctx.Err() == nil {
		if q.HasPending() {
			if !turn(ctx) {
				return
			}
			last = time.Now()
			continue
		}
		changed := m.Changed()
		running := m.Running(true)
		if len(running) == 0 {
			if q.HasPending() {
				continue
			}
			return
		}
		var timeout <-chan time.Time
		var timer *time.Timer
		if ceiling > 0 {
			remaining := ceiling - time.Since(last)
			if remaining <= 0 {
				reportBackgroundCeiling(env, running, time.Since(last))
				return
			}
			timer = time.NewTimer(remaining)
			timeout = timer.C
		}
		select {
		case <-changed:
		case <-ctx.Done():
		case <-timeout:
			reportBackgroundCeiling(env, m.Running(true), time.Since(last))
			return
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// reportBackgroundCeiling tells the user the -p run stops waiting for its
// background commands.
func reportBackgroundCeiling(env *Env, running []bgtask.TaskInfo, waited time.Duration) {
	if len(running) == 0 {
		return
	}
	labels := make([]string, 0, len(running))
	for _, t := range running {
		labels = append(labels, t.ID+" "+strconv.Quote(t.Label()))
	}
	sort.Strings(labels)
	fmt.Fprintf(env.Stderr, "gocode -p: Background tasks still running %s after the last turn (%s); stopping them. Set %s=0 to wait indefinitely.\n",
		waited.Round(time.Second), strings.Join(labels, ", "), printBgWaitCeilingEnv)
}

// tasksCommand runs /tasks (alias /bashes): with no argument it lists the
// session's background shell commands; "stop <id>" (or "kill <id>") stops
// one.
func tasksCommand(m *bgtask.Manager, args string) string {
	if m == nil {
		return "No background tasks."
	}
	fields := strings.Fields(args)
	if len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "stop", "kill":
			if len(fields) < 2 {
				return "Usage: /tasks stop <id>"
			}
			id := fields[1]
			info, err := m.Stop(id)
			switch {
			case errors.Is(err, bgtask.ErrTaskNotFound):
				return "No background task with ID " + id + "."
			case errors.Is(err, bgtask.ErrTaskNotRunning):
				return "Task " + id + " is not running (" + string(info.Status) + ")."
			case err != nil:
				return "Could not stop " + id + ": " + err.Error()
			}
			return "Stopped " + id + " (" + info.Label() + ")."
		case "list", "ls":
		default:
			return "Usage: /tasks [list | stop <id>]"
		}
	}
	return formatTasks(m.List(), time.Now())
}

// formatTasks renders the background task list, oldest first.
func formatTasks(list []bgtask.TaskInfo, now time.Time) string {
	if len(list) == 0 {
		return "No background tasks."
	}
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].StartedAt.Equal(list[j].StartedAt) {
			return list[i].StartedAt.Before(list[j].StartedAt)
		}
		return list[i].ID < list[j].ID
	})
	running := 0
	var b strings.Builder
	for _, t := range list {
		state := string(t.Status)
		switch t.Status {
		case bgtask.StatusRunning:
			running++
			state = "running " + now.Sub(t.StartedAt).Round(time.Second).String()
		case bgtask.StatusCompleted, bgtask.StatusFailed:
			state += " (exit code " + strconv.Itoa(t.ExitCode) + ")"
		}
		label := t.Label()
		if len([]rune(label)) > 80 {
			label = string([]rune(label)[:79]) + "…"
		}
		fmt.Fprintf(&b, "  %s  %s  %s\n", t.ID, state, label)
		if t.OutputFile != "" {
			fmt.Fprintf(&b, "      output: %s\n", t.OutputFile)
		}
	}
	head := fmt.Sprintf("Background tasks (%d running, %d total)\n", running, len(list))
	tail := ""
	if running > 0 {
		tail = "Stop one with /tasks stop <id>."
	}
	return strings.TrimRight(head+b.String()+tail, "\n")
}

// refreshShells pushes the number of running background shell commands to
// the footer pill when it changed.
func (d *turnDriver) refreshShells() {
	if d.conv == nil || d.conv.BackgroundTasks == nil || d.prog == nil {
		return
	}
	n := len(d.conv.BackgroundTasks.Running(false))
	d.agentMu.Lock()
	changed := n != d.shellsShown
	d.shellsShown = n
	d.agentMu.Unlock()
	if changed {
		d.prog.Send(ui.SetBackgroundShellsMsg{Count: n})
	}
}
