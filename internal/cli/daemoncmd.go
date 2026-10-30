package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/confighome"
	"github.com/umairge66/gocode/internal/daemon"
)

// -----------------------------------------------------------------------------
// `gocode daemon [run|stop]`.
//
// The `run` supervisor is wired to internal/daemon's control transport: it
// binds ListenControl at the per-install ControlAddress, serves the control
// protocol (Server + Router with DefaultCheckPeer peer auth) until a signal,
// and cleans up the socket. The background-session supervisor sits behind the
// daemonControl seam.
// -----------------------------------------------------------------------------

// daemonInvocation is the parsed `daemon` argv.
type daemonInvocation struct {
	Sub       string
	LogPath   string
	Origin    string
	SpawnedBy string
	Rest      []string
}

// daemonSubcommands is the recognized `daemon` subcommand set.
var daemonSubcommands = map[string]struct{}{
	"run": {}, "stop": {},
}

// parseDaemonArgs peels the `--log-file`/`--origin`/`--spawned-by` options
// (both "--flag value" and "--flag=value" forms) and resolves the subcommand.
// Without a subcommand the default is "run" when stdin is not a TTY and ""
// (print usage) when it is.
func parseDaemonArgs(args []string, stdinTTY bool) daemonInvocation {
	inv := daemonInvocation{}
	consumed := make(map[int]bool)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--log-file" && i+1 < len(args):
			consumed[i] = true
			i++
			consumed[i] = true
			inv.LogPath = args[i]
		case strings.HasPrefix(a, "--log-file="):
			consumed[i] = true
			inv.LogPath = strings.TrimPrefix(a, "--log-file=")
		case a == "--origin" && i+1 < len(args):
			consumed[i] = true
			i++
			consumed[i] = true
			inv.Origin = args[i]
		case strings.HasPrefix(a, "--origin="):
			consumed[i] = true
			inv.Origin = strings.TrimPrefix(a, "--origin=")
		case a == "--spawned-by" && i+1 < len(args):
			consumed[i] = true
			i++
			consumed[i] = true
			inv.SpawnedBy = args[i]
		}
	}

	var rest []string
	for i, a := range args {
		if !consumed[i] {
			rest = append(rest, a)
		}
	}

	posIdx := -1
	for i, a := range rest {
		if !strings.HasPrefix(a, "-") {
			posIdx = i
			break
		}
	}
	if posIdx == -1 {
		if !stdinTTY {
			inv.Sub = "run"
		}
		inv.Rest = rest
		return inv
	}
	tok := rest[posIdx]
	inv.Sub = tok
	if _, ok := daemonSubcommands[tok]; ok {
		// A known subcommand: drop it from rest.
		inv.Rest = append(append([]string{}, rest[:posIdx]...), rest[posIdx+1:]...)
	}
	return inv
}

// daemonUsage is the usage text for `daemon --help`.
const daemonUsage = `Usage: gocode daemon [run|stop]

The background daemon runs on demand and exits when the last client disconnects.

Commands:
  run        serve the control socket in the foreground (default when not a TTY)
  stop       ask the running daemon to shut down`

// runDaemon is the dispatch leaf for `daemon`. t is the args after the literal
// `daemon` token (what daemonArgs returns).
func runDaemon(env *Env, t []string) int {
	if contains(t, "--help") || contains(t, "-h") {
		fmt.Fprintln(env.Stdout, daemonUsage)
		return 0
	}

	inv := parseDaemonArgs(t, env.StdinTTY)

	switch inv.Sub {
	case "run":
		return runDaemonServeOrigin(env, daemonConfigHome(env), inv.Origin, inv.LogPath)
	case "stop":
		return runDaemonStop(env, daemonConfigHome(env), inv.Rest)
	case "":
		fmt.Fprintln(env.Stderr, daemonUsage)
		return 2
	default:
		fmt.Fprintf(env.Stderr, "unknown subcommand: %s\n\n", inv.Sub)
		fmt.Fprintln(env.Stderr, daemonUsage)
		return 1
	}
}

// errDaemonAlreadyRunning is the singleton-guard sentinel.
var errDaemonAlreadyRunning = errors.New("another daemon is already running")

// daemonControl is the injectable control-socket transport seam, so the serve
// loop is exercised over a temp socket without the real /tmp/cc-daemon path.
type daemonControl struct {
	// ping probes an existing daemon for the singleton guard (try-connect).
	ping func(ctx context.Context) bool
	// listen binds the control socket.
	listen func() (net.Listener, error)
	// background, when set, starts the background-session supervisor on the
	// router (bgdaemon.go); cancel stops the daemon (idle exit / shutdown op).
	background func(ctx context.Context, cancel context.CancelFunc, version string) *bgSupervisor
}

// controlForConfigDir wires daemonControl to internal/daemon for configDir.
func controlForConfigDir(configDir string) daemonControl {
	return daemonControl{
		ping: func(ctx context.Context) bool {
			return daemon.NewControlClient(configDir).Ping(ctx).OK
		},
		listen: func() (net.Listener, error) {
			return daemon.ListenControl(configDir)
		},
	}
}

// runDaemonServeOrigin is runDaemonServe with the `--origin` / `--log-file`
// of the invocation (a "transient" daemon exits when idle; see bgdaemon.go).
func runDaemonServeOrigin(env *Env, configDir, origin, logPath string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two-stage signal handling: the first SIGINT/SIGTERM aborts gracefully, a
	// second forces exit(1).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
		<-sigCh
		fmt.Fprintln(env.Stderr, "forced shutdown")
		os.Exit(1)
	}()

	ctrl := controlForConfigDir(configDir)
	ctrl.background = backgroundSupervisorFor(env, configDir, origin, logPath)
	err := serveDaemon(ctx, ctrl, updateCurrentVersion(), env.Stderr)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDaemonAlreadyRunning):
		fmt.Fprintln(env.Stderr, errDaemonAlreadyRunning.Error())
		return 1
	default:
		fmt.Fprintf(env.Stderr, "gocode daemon: %v\n", err)
		return 1
	}
}

// serveDaemon is the testable core: singleton guard, bind, serve-until-cancel,
// socket cleanup (the unix listener unlinks its socket on Close).
func serveDaemon(ctx context.Context, ctrl daemonControl, version string, stderr io.Writer) error {
	// Singleton guard: ping any daemon already serving this address.
	if ctrl.ping != nil {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		alive := ctrl.ping(pingCtx)
		pingCancel()
		if alive {
			return errDaemonAlreadyRunning
		}
	}

	l, err := ctrl.listen()
	if err != nil {
		return err
	}
	defer l.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	router := &daemon.Router{
		Version: version,
		Ready:   func() bool { return true },
	}
	if ctrl.background != nil {
		if bg := ctrl.background(ctx, cancel, version); bg != nil {
			bg.register(router)
			defer bg.close()
		}
	}
	srv := &daemon.Server{Handler: router}

	serveErr := srv.Serve(ctx, l)
	if serveErr == nil || errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, net.ErrClosed) {
		return nil
	}
	return serveErr
}

// daemonConfigHome resolves the gocode root (internal/confighome) the control
// address and background-session state are derived from.
func daemonConfigHome(env *Env) string {
	return confighome.ResolveDefault(envGetenv(env), func() (string, error) {
		if h := trustHome(env); h != "" {
			return h, nil
		}
		return os.UserHomeDir()
	})
}
