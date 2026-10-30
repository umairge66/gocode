package mcp

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// OpenBrowser best-effort opens rawURL in the user's browser. It honours
// $BROWSER, then uses the platform opener (open / rundll32 / xdg-open). It
// never blocks on the browser and only accepts http(s) URLs, so a hostile
// authorization endpoint cannot smuggle a shell command or a file: URL.
// Callers must ALWAYS also print the URL: over SSH or without a desktop the
// open silently does nothing.
func OpenBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("mcp: refusing to open a non-http(s) URL")
	}
	var cmd *exec.Cmd
	if b := strings.TrimSpace(os.Getenv("BROWSER")); b != "" {
		cmd = exec.Command(b, rawURL)
	} else {
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", rawURL)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
		default:
			cmd = exec.Command("xdg-open", rawURL)
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
