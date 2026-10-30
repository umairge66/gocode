package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/umairge66/gocode/internal/agent"
)

func TestWithNotice_AppendsToSuccessOnly(t *testing.T) {
	shown := false
	notice := func(context.Context) string {
		if shown {
			return ""
		}
		shown = true
		return "reload the extension"
	}
	fail := false
	a := withNotice(adapter{
		run: func(context.Context, json.RawMessage) (string, error) {
			if fail {
				return "", errors.New("boom")
			}
			return "ok", nil
		},
		runImages: func(context.Context, json.RawMessage) (string, []agent.ToolResultImage, error) {
			return "img", nil, nil
		},
	}, notice)

	fail = true
	if _, err := a.run(context.Background(), nil); err == nil {
		t.Fatal("error lost")
	}
	if shown {
		t.Fatal("notice consumed by a failed call")
	}
	fail = false
	out, _ := a.run(context.Background(), nil)
	if out != "ok\n\nNote: reload the extension" {
		t.Errorf("first result = %q", out)
	}
	out, _, _ = a.runImages(context.Background(), nil)
	if out != "img" {
		t.Errorf("notice repeated: %q", out)
	}
}
