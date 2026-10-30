package lsp

import (
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/umairge66/gocode/internal/capbuf"
	"github.com/umairge66/gocode/internal/envsec"
	"github.com/umairge66/gocode/internal/proctree"
)

// Bounds on what a server's stderr may keep in memory: the first and the last
// few kilobytes, enough to explain a crash.
const (
	stderrHeadCap = 4 << 10
	stderrTailCap = 16 << 10
)

// exitGrace is how long a server that was sent "exit" is given before its
// process group is killed.
const exitGrace = 500 * time.Millisecond

// StdioConfig configures a StdioTransport launch.
type StdioConfig struct {
	Command string
	Args    []string
	// Env is applied on top of the inherited environment, which never carries
	// gocode's own provider API keys.
	Env map[string]string
	// Dir is the working directory ("" inherits).
	Dir string
}

// StdioTransport runs a language server as a child process (in its own
// process group) and speaks the framed base protocol over its stdin/stdout.
// Its stderr is kept, bounded, for error reports.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *FrameReader
	stderr *capbuf.Buffer

	exited    chan struct{}
	closeOnce sync.Once
}

// NewStdioTransport spawns the server process.
func NewStdioTransport(sc StdioConfig) (*StdioTransport, error) {
	cmd := exec.Command(sc.Command, sc.Args...)
	cmd.Dir = sc.Dir
	cmd.Env = serverEnv(sc.Env)
	proctree.Prepare(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		reader: NewFrameReader(stdout),
		stderr: capbuf.New(stderrHeadCap, stderrTailCap),
		exited: make(chan struct{}),
	}
	cmd.Stderr = t.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		close(t.exited)
	}()
	return t, nil
}

// serverEnv is the inherited environment without provider credentials, with
// the server's own variables applied on top (in a stable order).
func serverEnv(extra map[string]string) []string {
	env := envsec.ChildEnv(nil)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// Send frames and writes one payload to the server's stdin.
func (t *StdioTransport) Send(payload []byte) error {
	return WriteMessage(t.stdin, payload)
}

// Receive reads the next framed message from the server's stdout.
func (t *StdioTransport) Receive() ([]byte, error) {
	return t.reader.ReadMessage()
}

// Stderr returns the kept stderr output, trimmed.
func (t *StdioTransport) Stderr() string {
	return strings.TrimSpace(t.stderr.String())
}

// Exited is closed once the server process has exited.
func (t *StdioTransport) Exited() <-chan struct{} { return t.exited }

// Close closes stdin, gives the process a moment to exit on its own, then
// kills its process group.
func (t *StdioTransport) Close() error {
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		select {
		case <-t.exited:
		case <-time.After(exitGrace):
		}
		if t.cmd.Process != nil {
			_ = proctree.KillGroup(t.cmd.Process.Pid)
			_ = t.cmd.Process.Kill()
		}
	})
	return nil
}
