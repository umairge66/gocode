package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/umairge66/gocode/internal/daemon"
)

// -----------------------------------------------------------------------------
// Internal process modes of background sessions:
//
//	gocode --bg-pty-host <sock> <cols> <rows> -- <file> [args...]
//	gocode --bg-spare <claim.sock>

// runBgPtyHost is the dispatch leaf for `--bg-pty-host`.
func runBgPtyHost(env *Env, t []string) int {
	return daemon.RunPTYHost(daemon.PTYHostConfig{
		Args:      t[1:],
		ConfigDir: daemonConfigHome(env),
		Version:   updateCurrentVersion(),
		Stderr:    env.Stderr,
	})
}

// sparePPIDPoll is how often a waiting spare checks that its pty host still
// exists.
const sparePPIDPoll = 2 * time.Second

// runBgSpare waits for a claim on the claim socket, applies it (working
// directory, environment, argv) and then runs as that background session.
func runBgSpare(env *Env, t []string) int {
	if len(t) < 2 || t[1] == "" {
		fmt.Fprintln(env.Stderr, "[bg-spare] missing claim sock path")
		return 2
	}
	claimSock := t[1]
	auth := env.Getenv(daemon.EnvBgClaimAuth)
	_ = os.Unsetenv(daemon.EnvBgClaimAuth)
	if tp := env.Getenv(daemon.EnvBgTokensPath); tp != "" {
		_ = os.Unsetenv(daemon.EnvBgTokensPath)
		tok := daemon.ReadSocketTokens(tp)
		_ = os.Remove(tp)
		if tok != nil && tok.ClaimAuth != "" {
			auth = tok.ClaimAuth
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	ppid := os.Getppid()
	exitQuietly := make(chan struct{}, 1)
	go func() {
		tk := time.NewTicker(sparePPIDPoll)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				exitQuietly <- struct{}{}
				cancel()
				return
			case <-tk.C:
				if os.Getppid() != ppid {
					exitQuietly <- struct{}{}
					cancel()
					return
				}
			}
		}
	}()
	claim, err := daemon.ReceiveClaim(ctx, claimSock, auth)
	signal.Stop(sigCh)
	select {
	case <-exitQuietly:
		return 0
	default:
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "[bg-spare] claim recv failed: %v\n", err)
		return 1
	}
	cancel()
	if claim.Cwd != "" {
		if err := os.Chdir(claim.Cwd); err != nil {
			fmt.Fprintf(env.Stderr, "[bg-spare] post-claim init failed: %v\n", err)
			return 1
		}
	}
	for k, v := range claim.Env {
		if k == "" || containsNUL(k) || containsNUL(v) {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return Run(DefaultEnv(), append([]string{progNameOr("gocode")}, claim.Argv...))
}

func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

func progNameOr(def string) string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return os.Args[0]
	}
	return def
}
