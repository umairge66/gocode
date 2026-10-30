package mods

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/umairge66/gocode/internal/proctree"
)

// $.process.spawn: a command started by its argument vector (no shell, the
// same rules as $.process.run: the session's working directory by default,
// git with the repository's hooks off) whose output streams piece by piece,
// then how it ended. The loop is the child's life: leaving it, return() on
// the stream, the caller's next.signal aborting or the module unloading
// kills the child (SIGTERM, then SIGKILL after a short grace).
//
// The child's input is taken once, as `input` (written, then the
// child's stdin closes); it has no later writes, and no timeout: a child
// may run for the session's life, bounded by the module's. A reader that
// falls more than 1 MiB behind holds the child's output until it catches up.

// spawnBacklog: the unread output a child may run ahead by.
const spawnBacklog = 1048576

// spawnKillGrace: SIGTERM, then SIGKILL this much later.
const spawnKillGrace = 500 * time.Millisecond

func spawnCheck(a map[string]any) string {
	if a == nil {
		return "takes { argv, cwd?, env?, input? }"
	}
	if m := argvCheck(a["argv"]); m != "" {
		return m
	}
	if c, has := a["cwd"]; has && c != nil {
		if s, ok := c.(string); !ok || s == "" {
			return "cwd is a non-empty path"
		}
	}
	if m := stringMapCheck(a["env"], "env"); m != "" {
		return m
	}
	if in, has := a["input"]; has && in != nil {
		if _, ok := in.(string); !ok {
			return "input is a string"
		}
	}
	return ""
}

// pieceStream is a streaming event's stream from one link down: pieces,
// then a result ({ value } or { deny }).
type pieceStream interface {
	// pull answers the next piece, or (piece nil) the stream's result.
	pull(ctx context.Context) (piece, result map[string]any, err error)
	// close lets the stream go: what runs beneath it stops.
	close()
}

// --- the child -------------------------------------------------------------

type spawnItem struct {
	piece  map[string]any
	ended  map[string]any
	failed error
}

// processStream: the child, started on the first pull.
type processStream struct {
	plugin string
	e      map[string]any
	cwd    string
	logf   func(string, ...any)

	once    sync.Once
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	started time.Time

	mu      sync.Mutex
	queue   []spawnItem
	backlog int
	room    *sync.Cond
	closed  bool
	notify  chan struct{}
	chunks  int
}

func newProcessStream(plugin string, e map[string]any, cwd string, logf func(string, ...any)) *processStream {
	p := &processStream{plugin: plugin, e: e, cwd: cwd, logf: logf, notify: make(chan struct{}, 1)}
	p.room = sync.NewCond(&p.mu)
	return p
}

func (p *processStream) label() string {
	argv, _ := stringList(p.e["argv"])
	return fmt.Sprintf("%s: $.process.spawn(%s)", p.plugin, argv[0])
}

func (p *processStream) push(it spawnItem) {
	p.mu.Lock()
	p.queue = append(p.queue, it)
	if it.piece != nil {
		p.backlog += len(it.piece["text"].(string))
	}
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (p *processStream) start() error {
	argv, _ := stringList(p.e["argv"])
	dir := p.cwd
	if c, ok := p.e["cwd"].(string); ok && c != "" {
		if filepath.IsAbs(c) {
			dir = c
		} else {
			dir = filepath.Join(p.cwd, c)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	env, _ := p.e["env"].(map[string]any)
	cmd := hostCommand(ctx, argv[0], argv[1:], dir, env)
	// SIGTERM to the child's tree first, SIGKILL after the grace.
	// The child leads a process group of its own, so what it started goes
	// with it.
	proctree.Prepare(cmd)
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		err := proctree.SignalGraceful(pid)
		time.AfterFunc(spawnKillGrace, func() {
			if proctree.Alive(pid) {
				_ = proctree.KillGroup(pid)
			}
		})
		if err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = spawnKillGrace + 2*time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	if in, ok := p.e["input"].(string); ok {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			return err
		}
		go func() {
			_, _ = io.WriteString(stdin, in)
			_ = stdin.Close()
		}()
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	p.cmd = cmd
	p.started = time.Now()
	p.logf("%s: %s with %d args in %s (pid %d)", p.label(), argv[0], len(argv)-1, dir, cmd.Process.Pid)
	var readers sync.WaitGroup
	readers.Add(2)
	go p.read(stdout, "stdout", &readers)
	go p.read(stderr, "stderr", &readers)
	go func() {
		readers.Wait()
		err := cmd.Wait()
		ended := map[string]any{"code": nil, "signal": nil}
		if st := cmd.ProcessState; st != nil {
			if sig := exitSignal(st); sig != "" {
				ended["signal"] = sig
			} else {
				ended["code"] = float64(st.ExitCode())
			}
		} else if err != nil {
			p.push(spawnItem{failed: fmt.Errorf("%s failed: %v", p.label(), err)})
			return
		}
		p.push(spawnItem{ended: ended})
	}()
	return nil
}

// read streams one pipe as text pieces, holding back an incomplete UTF-8
// sequence for the next read and pausing while
// the reader is more than spawnBacklog behind.
func (p *processStream) read(r io.Reader, stream string, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			cut := len(data)
			for back := 1; back <= 3 && back <= len(data); back++ {
				if utf8.RuneStart(data[len(data)-back]) {
					if !utf8.FullRune(data[len(data)-back:]) {
						cut = len(data) - back
					}
					break
				}
			}
			carry = append([]byte(nil), data[cut:]...)
			if cut > 0 {
				p.push(spawnItem{piece: map[string]any{"stream": stream, "text": string(data[:cut])}})
			}
			p.mu.Lock()
			for p.backlog > spawnBacklog && !p.closed {
				p.room.Wait()
			}
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
		}
		if err != nil {
			if len(carry) > 0 {
				p.push(spawnItem{piece: map[string]any{"stream": stream, "text": string(carry)}})
			}
			return
		}
	}
}

func (p *processStream) pull(ctx context.Context) (map[string]any, map[string]any, error) {
	var startErr error
	p.once.Do(func() {
		if err := p.start(); err != nil {
			startErr = fmt.Errorf("%s failed to start: %v", p.label(), err)
			p.mu.Lock()
			p.closed = true
			p.mu.Unlock()
		}
	})
	if startErr != nil {
		return nil, nil, startErr
	}
	for {
		p.mu.Lock()
		if p.cmd == nil {
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("%s aborted", p.label())
		}
		if len(p.queue) > 0 {
			it := p.queue[0]
			p.queue = p.queue[1:]
			if it.piece != nil {
				p.backlog -= len(it.piece["text"].(string))
				p.chunks++
				p.room.Broadcast()
			}
			p.mu.Unlock()
			switch {
			case it.failed != nil:
				return nil, nil, it.failed
			case it.ended != nil:
				p.logf("%s: ended (%v) in %dms, %d chunks", p.label(), firstNonNil(it.ended["code"], it.ended["signal"]), time.Since(p.started).Milliseconds(), p.chunks)
				p.finish()
				return nil, map[string]any{"value": it.ended}, nil
			}
			return it.piece, nil, nil
		}
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, nil, fmt.Errorf("%s aborted", p.label())
		}
		select {
		case <-p.notify:
		case <-ctx.Done():
			p.close()
			return nil, nil, fmt.Errorf("%s aborted", p.label())
		}
	}
}

func firstNonNil(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func (p *processStream) finish() {
	p.mu.Lock()
	p.closed = true
	p.room.Broadcast()
	p.mu.Unlock()
}

// close kills the child if it is still running.
func (p *processStream) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.room.Broadcast()
	running := p.cmd != nil
	p.mu.Unlock()
	if running && p.cancel != nil {
		p.logf("%s: stopped after %dms: the stream was let go", p.label(), time.Since(p.started).Milliseconds())
		p.cancel()
	}
}

// --- the chain --------------------------------------------------------------

// streamChain is one dispatch of a streaming event: the
// hooks that select it, outermost first, each an async generator reading
// next(e)'s stream and yielding pieces up, and the bottom.
type streamChain struct {
	en     *Engine
	event  string
	links  []*registration
	origin Origin
	bottom func(e map[string]any) (pieceStream, error)
}

// at is the stream from link i down.
func (c *streamChain) at(i int, e map[string]any) pieceStream {
	if i >= len(c.links) {
		return &lazyStream{open: func() (pieceStream, error) { return c.bottom(e) }}
	}
	return &hookStream{c: c, i: i, e: e}
}

// lazyStream opens its stream on the first pull (a refusal rejects it).
type lazyStream struct {
	open   func() (pieceStream, error)
	mu     sync.Mutex
	s      pieceStream
	err    error
	opened bool
	closed bool
}

func (l *lazyStream) pull(ctx context.Context) (map[string]any, map[string]any, error) {
	l.mu.Lock()
	if !l.opened {
		l.opened = true
		if !l.closed {
			l.s, l.err = l.open()
		}
	}
	s, err, closed := l.s, l.err, l.closed
	l.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if closed || s == nil {
		return nil, nil, errors.New("the stream was let go")
	}
	return s.pull(ctx)
}

func (l *lazyStream) close() {
	l.mu.Lock()
	l.closed = true
	s := l.s
	l.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// openStream starts a streaming call on `$`: the scan must list
// it and the host check pass; then it runs as its event through every hook
// but the caller, the host's stream at the bottom (re-checked there, on what
// the hooks passed down). The returned stream answers the result's value,
// or rejects with its { deny }.
func (en *Engine) openStream(ctx context.Context, s *seat, reg *registration, name string, args map[string]any) pieceStream {
	refuse := func(err error) pieceStream {
		return &lazyStream{open: func() (pieceStream, error) { return nil, err }}
	}
	if name != "process.spawn" {
		return refuse(fmt.Errorf("%s: $.%s: no such stream", s.name, name))
	}
	if !s.engineLevel {
		if msg := s.scan.allowsCall(name); msg != "" {
			return refuse(fmt.Errorf("%s: $.%s refused: %s", s.name, name, msg))
		}
	}
	if msg := spawnCheck(args); msg != "" {
		return refuse(fmt.Errorf("%s: %s: %s (host check)", s.name, name, msg))
	}
	if en.opts.testMode {
		// The test kit has no process: what no test hook answers rejects.
		bottom := func(map[string]any) (pieceStream, error) {
			return nil, fmt.Errorf("no implementation for %s", name)
		}
		return en.streamTop(ctx, s, reg, name, args, bottom)
	}
	cwd := en.opts.Host.cwd()
	bottom := func(e map[string]any) (pieceStream, error) {
		if msg := spawnCheck(e); msg != "" {
			return nil, fmt.Errorf("%s: %s (host check)", name, msg)
		}
		return newProcessStream(s.name, e, cwd, en.logf), nil
	}
	return en.streamTop(ctx, s, reg, name, args, bottom)
}

func (en *Engine) streamTop(ctx context.Context, s *seat, reg *registration, name string, args map[string]any, bottom func(map[string]any) (pieceStream, error)) pieceStream {
	c := &streamChain{en: en, event: name, origin: Origin{Plugin: s.name, Tier: s.tier}, bottom: bottom}
	if s.engineLevel {
		c.origin = EngineOrigin
	}
	en.mu.RLock()
	seats := append([]*seat(nil), en.seats...)
	beneath := en.beneath
	en.mu.RUnlock()
	for _, st := range seats {
		for _, r := range st.regs {
			if r != reg && r.pattern.selects(name, st.tier == TierBuiltin) {
				c.links = append(c.links, r)
			}
		}
	}
	if beneath != nil {
		for _, r := range beneath.regs {
			if r != reg && r.pattern.selects(name, true) {
				c.links = append(c.links, r)
			}
		}
	}
	return &topStream{plugin: s.name, event: name, s: c.at(0, args)}
}

// topStream is what the caller reads: the pieces, then the result's value;
// a { deny } rejects.
type topStream struct {
	plugin, event string
	s             pieceStream
}

func (t *topStream) pull(ctx context.Context) (map[string]any, map[string]any, error) {
	piece, res, err := t.s.pull(ctx)
	if err != nil || piece != nil {
		return piece, nil, err
	}
	if d, ok := res["deny"].(string); ok {
		return nil, nil, fmt.Errorf("%s: $.%s: %s", t.plugin, t.event, d)
	}
	v, _ := res["value"].(map[string]any)
	if v == nil {
		v = map[string]any{}
	}
	return nil, v, nil
}

func (t *topStream) close() { t.s.close() }
