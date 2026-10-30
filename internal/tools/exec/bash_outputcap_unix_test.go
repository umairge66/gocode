//go:build unix

package exec

import (
	"context"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/capbuf"
)

// A command that prints far more than fits is kept to the head and tail, so
// gocode's memory does not grow with the command's output.
func TestBashOutputIsBounded(t *testing.T) {
	res, err := BashTool{}.Execute(context.Background(), BashInput{
		Command: "head -c 300000000 /dev/zero | tr '\\0' x; echo; echo END-OF-OUTPUT",
		Timeout: 120000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if max := capbuf.DefaultHeadCap + capbuf.DefaultTailCap + 512; len(res.Stdout) > max {
		t.Fatalf("stdout kept %d bytes, want at most %d", len(res.Stdout), max)
	}
	if !strings.Contains(res.Stdout, "of output omitted: the command printed 286.1 MB in total") {
		t.Fatalf("missing omission note")
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Stdout), "END-OF-OUTPUT") {
		t.Fatalf("tail not kept: %q", res.Stdout[len(res.Stdout)-80:])
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
}
