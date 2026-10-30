//go:build unix

package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umairge66/gocode/internal/bgtask"
)

const keyProbe = `echo "or=${OPENROUTER_API_KEY:+set} gc=${GROQ_API_KEY:+set} gh=${GITHUB_TOKEN:+set}"`

func setFakeKeys(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-fake-test-key")
	t.Setenv("GROQ_API_KEY", "fake-groq-key")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
}

// The unsandboxed Bash tool never passes gocode's provider keys to the
// command; the user's other credentials stay.
func TestBashUnsandboxedStripsProviderKeys(t *testing.T) {
	setFakeKeys(t)
	res, err := BashTool{}.Execute(context.Background(), BashInput{Command: keyProbe})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "or= gc= gh=set\n" {
		t.Fatalf("bash env = %q", res.Stdout)
	}
}

// A detached background Bash command (no task manager) is stripped too.
func TestBashBackgroundStripsProviderKeys(t *testing.T) {
	setFakeKeys(t)
	out := filepath.Join(t.TempDir(), "env.txt")
	if _, err := (BashTool{}).Execute(context.Background(), BashInput{Command: keyProbe + ` > '` + out + `'`, RunInBackground: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && strings.HasSuffix(string(b), "\n") {
			if string(b) != "or= gc= gh=set\n" {
				t.Fatalf("background env = %q", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background command never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Background tasks and monitors run through bgtask.OSRunner.
func TestBgTaskRunnerStripsProviderKeys(t *testing.T) {
	setFakeKeys(t)
	var buf strings.Builder
	w := &syncWriter{b: &buf}
	p, err := bgtask.OSRunner{}.Start(context.Background(), keyProbe, w, w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	got := buf.String()
	w.mu.Unlock()
	if got != "or= gc= gh=set\n" {
		t.Fatalf("bg task env = %q", got)
	}
}

type syncWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
