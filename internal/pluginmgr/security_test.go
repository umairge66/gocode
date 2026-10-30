package pluginmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/umairge66/gocode/internal/pluginfetch"
	"github.com/umairge66/gocode/internal/plugins"
)

// writeEscapeMarketplace builds a local marketplace whose single entry uses
// source, plus a sibling "outside" plugin directory the entry may try to reach.
func writeEscapeMarketplace(t *testing.T, source string) (mktRoot, outside string) {
	t.Helper()
	base := t.TempDir()
	mktRoot = filepath.Join(base, "mkt")
	outside = filepath.Join(base, "outside")
	writeFile(t, mktRoot, ".claude-plugin/marketplace.json", `{"name":"esc","owner":{"name":"x"},"plugins":[{"name":"evil","source":"`+source+`"}]}`)
	writeFile(t, outside, ".claude-plugin/plugin.json", `{"name":"evil","version":"1.0.0"}`)
	writeFile(t, outside, "secret.txt", "TOP-SECRET")
	return mktRoot, outside
}

// A marketplace entry's relative source must stay inside the marketplace tree:
// "../outside" would install (and copy into the cache) an arbitrary directory.
func TestPlanInstallRejectsDotDotEntrySource(t *testing.T) {
	mkt, _ := writeEscapeMarketplace(t, "../outside")
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if plan, err := m.PlanInstall(context.Background(), "evil@esc"); err == nil {
		t.Fatalf("PlanInstall accepted an entry escaping the marketplace (sourceDir=%s)", plan.sourceDir)
	}
}

// A symlink inside the marketplace tree pointing outside it must not be
// followed as the plugin root.
func TestPlanInstallRejectsSymlinkedEntrySource(t *testing.T) {
	mkt, outside := writeEscapeMarketplace(t, "./link")
	if err := os.Symlink(outside, filepath.Join(mkt, "link")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	m := newManager(t, plugins.Policy{}, nil)
	if _, err := m.AddMarketplace(context.Background(), mkt); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if plan, err := m.PlanInstall(context.Background(), "evil@esc"); err == nil {
		t.Fatalf("PlanInstall followed a symlink out of the marketplace (sourceDir=%s)", plan.sourceDir)
	}
}

// remoteMktGit materializes a remote marketplace with the given JSON.
type remoteMktGit struct{ json string }

func (g *remoteMktGit) Run(ctx context.Context, cwd string, args []string) (pluginfetch.GitResult, error) {
	if args[0] == "clone" {
		mustWrite(args[len(args)-1], ".claude-plugin/marketplace.json", g.json)
	}
	return pluginfetch.GitResult{Code: 0}, nil
}

// A REMOTE marketplace must not be able to point a plugin at an arbitrary
// local directory (e.g. a world-writable /tmp dir another user controls).
func TestPlanInstallRejectsLocalSourceFromRemoteMarketplace(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, ".claude-plugin/plugin.json", `{"name":"evil","version":"1.0.0"}`)
	writeFile(t, outside, "hooks/hooks.json", `{"hooks":{}}`)
	src, _ := filepath.Abs(outside)
	g := &remoteMktGit{json: `{"name":"rm","owner":{"name":"x"},"plugins":[{"name":"evil","source":{"source":"directory","path":"` + filepath.ToSlash(src) + `"}}]}`}
	m := newManager(t, plugins.Policy{}, &pluginfetch.Fetcher{Git: g})
	if _, err := m.AddMarketplace(context.Background(), "acme/widget"); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if plan, err := m.PlanInstall(context.Background(), "evil@rm"); err == nil {
		t.Fatalf("remote marketplace installed a local directory source (sourceDir=%s)", plan.sourceDir)
	}
}
