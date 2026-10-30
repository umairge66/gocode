package exec

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/bgtask"
)

type recSandbox struct {
	on       bool
	released int
	shell    string
}

func (r *recSandbox) ShouldSandbox(cmd string, dds bool) bool { return r.on && !dds }
func (r *recSandbox) WrapCommand(_ context.Context, cmd, shell, dir string) ([]string, func(), error) {
	r.shell = shell
	return []string{shell, "-c", "echo WRAPPED; " + cmd}, func() { r.released++ }, nil
}

func TestBashToolRunsThroughSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sandbox on Windows")
	}
	sb := &recSandbox{on: true}
	res, err := BashTool{Sandbox: sb}.Execute(context.Background(), BashInput{Command: "echo inner"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "WRAPPED") || !strings.Contains(res.Stdout, "inner") || sb.released != 1 {
		t.Fatalf("stdout %q released %d", res.Stdout, sb.released)
	}
	if sb.shell == "" {
		t.Error("the resolved shell is handed to the wrapper")
	}
	res, err = BashTool{Sandbox: sb}.Execute(context.Background(), BashInput{Command: "echo plain", DangerouslyDisableSandbox: true})
	if err != nil || strings.Contains(res.Stdout, "WRAPPED") {
		t.Fatalf("unsandboxed run wrapped: %q %v", res.Stdout, err)
	}
}

func TestFlexBool(t *testing.T) {
	for in, want := range map[string]bool{`true`: true, `"true"`: true, `"True"`: true, `"TRUE"`: true, `1`: true, `"1"`: true, `false`: false, `"false"`: false, `"False"`: false, `0`: false, `null`: false} {
		var in2 BashInput
		if err := json.Unmarshal([]byte(`{"command":"x","dangerouslyDisableSandbox":`+in+`}`), &in2); err != nil || bool(in2.DangerouslyDisableSandbox) != want {
			t.Errorf("%s → %v (%v)", in, in2.DangerouslyDisableSandbox, err)
		}
	}
	for _, v := range []string{`"yes"`, `2`, `{}`, `[]`, `"tru"`} {
		var bad BashInput
		if err := json.Unmarshal([]byte(`{"command":"x","dangerouslyDisableSandbox":`+v+`}`), &bad); err == nil {
			t.Errorf("non-boolean %s accepted", v)
		}
	}
}

func TestBashToolBackgroundRunsThroughSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sandbox on Windows")
	}
	m := bgtask.NewManager()
	defer m.Close()
	sb := &recSandbox{on: true}
	res, err := BashTool{Sandbox: sb, BackgroundTasks: m}.Execute(context.Background(), BashInput{Command: "echo bg", RunInBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), res.BackgroundTaskID); err != nil {
		t.Fatal(err)
	}
	out, err := m.Output(res.BackgroundTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "WRAPPED") || !strings.Contains(out.Output, "bg") {
		t.Fatalf("background output %q", out.Output)
	}
}
