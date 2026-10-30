package toolrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/umairge66/gocode/internal/pathguard"
)

func TestWithDefaultSearchPath(t *testing.T) {
	var permPath, runInput string
	a := adapter{
		permission: func(in json.RawMessage) (string, string, pathguard.Operation, error) {
			var v struct {
				Path string `json:"path"`
			}
			_ = json.Unmarshal(in, &v)
			permPath = v.Path
			return v.Path, v.Path, pathguard.OpRead, nil
		},
		run: func(_ context.Context, in json.RawMessage) (string, error) { runInput = string(in); return "", nil },
	}
	w := withDefaultSearchPath(a, "/wt")
	_, _, _, _ = w.permission(json.RawMessage(`{"pattern":"*.go"}`))
	_, _ = w.run(context.Background(), json.RawMessage(`{"pattern":"*.go"}`))
	if permPath != "/wt" || runInput != `{"path":"/wt","pattern":"*.go"}` {
		t.Fatalf("perm=%q run=%q", permPath, runInput)
	}
	_, _, _, _ = w.permission(json.RawMessage(`{"pattern":"x","path":"/elsewhere"}`))
	if permPath != "/elsewhere" {
		t.Fatalf("explicit path overridden: %q", permPath)
	}
	if d := New("default", nil, WithWorkingDir("/wt")); d.WorkDir != "/wt" {
		t.Fatal("WithWorkingDir")
	}
}
