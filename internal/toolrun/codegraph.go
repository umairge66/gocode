package toolrun

import (
	"context"
	"encoding/json"
	"os"

	"github.com/umairge66/gocode/internal/graphify"
	pathguard "github.com/umairge66/gocode/internal/pathguard"
)

// CodeGraphAdapter adapts the CodeGraph tool. It is a read tool over the
// working directory: the rule content and path are that directory (Read
// rules and the working-directory check apply, as for Glob and Grep), and
// files a Read deny rule covers are left out of the graph by the tool.
func CodeGraphAdapter(t graphify.Tool) adapter {
	root := func() string {
		if t.Root != "" {
			return t.Root
		}
		wd, _ := os.Getwd()
		return wd
	}
	return adapter{
		permission: func(input json.RawMessage) (string, string, pathguard.Operation, error) {
			if _, err := graphify.ParseToolInput(input); err != nil {
				return "", "", "", err
			}
			return root(), root(), pathguard.OpRead, nil
		},
		run: func(ctx context.Context, input json.RawMessage) (string, error) {
			in, err := graphify.ParseToolInput(input)
			if err != nil {
				return "", err
			}
			return t.Execute(ctx, in)
		},
		description: graphify.ToolDescription,
		inputSchema: graphify.ToolInputSchema,
	}
}
