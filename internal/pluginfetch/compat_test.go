package pluginfetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/fsfacade"
	"github.com/umairge66/gocode/internal/plugins"
)

// claudeGit is a fake git whose clone materializes a Claude Code plugin with
// only a .claude-plugin/plugin.json manifest.
type claudeGit struct{}

func (claudeGit) Run(ctx context.Context, cwd string, args []string) (GitResult, error) {
	if args[0] == "clone" {
		dest := args[len(args)-1]
		if err := os.MkdirAll(filepath.Join(dest, ".claude-plugin"), 0o755); err != nil {
			return GitResult{}, err
		}
		if err := os.WriteFile(filepath.Join(dest, ".claude-plugin", "plugin.json"), []byte(`{"name":"claude-only","x-claude-extra":true}`), 0o644); err != nil {
			return GitResult{}, err
		}
	}
	return GitResult{}, nil
}

func osFS() fsfacade.FS { return fsfacade.OSFS{} }

func TestMarketplaceFromGitHubIsFetchable(t *testing.T) {
	src, err := plugins.MarketplaceFromGitHub("acme/claude-marketplace")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGit{}
	f := &Fetcher{Git: g, HTTP: &fakeDoer{}}
	root := t.TempDir()
	dir, err := plugins.Resolve(src, filepath.Join(root, plugins.CloneFolderName(src)), plugins.Policy{}, f)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(root, "acme-claude-marketplace")
	if dir != want {
		t.Fatalf("dir = %v, want %s", dir, want)
	}
	joined := strings.Join(g.calls[0], " ")
	if !strings.Contains(joined, "clone --depth 1") || !strings.Contains(joined, "https://github.com/acme/claude-marketplace.git") {
		t.Fatalf("clone args = %v", g.calls[0])
	}
}

func TestMarketplaceFromGitHubCommitPin(t *testing.T) {
	pin := strings.Repeat("c", 40)
	src, err := plugins.MarketplaceFromGitHub("acme/mk#" + pin)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGit{headSHA: pin}
	f := &Fetcher{Git: g}
	if _, err := f.Fetch(src, filepath.Join(t.TempDir(), "mk")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var sawRevParse bool
	for _, c := range g.calls {
		sawRevParse = sawRevParse || c[0] == "rev-parse"
	}
	if !sawRevParse {
		t.Fatalf("commit pin not verified: %v", g.calls)
	}
}

func TestFetchGit_ExplicitSHAField(t *testing.T) {
	pin := strings.Repeat("d", 40)
	g := &fakeGit{headSHA: strings.Repeat("e", 40)}
	f := &Fetcher{Git: g}
	src := plugins.Source{Kind: plugins.SourceGitHub, Repo: "acme/p", Ref: "v2", SHA: pin}
	_, err := f.Fetch(src, filepath.Join(t.TempDir(), "p"))
	if err == nil || !strings.Contains(err.Error(), "SHA pin verification failed") {
		t.Fatalf("expected sha pin mismatch, got %v", err)
	}
	bad := plugins.Source{Kind: plugins.SourceGitHub, Repo: "acme/p", SHA: "abc"}
	if _, err := f.Fetch(bad, filepath.Join(t.TempDir(), "p2")); err == nil {
		t.Fatal("expected invalid sha error")
	}
}

func TestFetchURL_GitURLClones(t *testing.T) {
	doer := &fakeDoer{}
	g := &fakeGit{}
	f := &Fetcher{Git: g, HTTP: doer}
	src := plugins.Source{Kind: plugins.SourceURL, URL: "https://gitlab.test/team/plugin.git"}
	if _, err := f.Fetch(src, filepath.Join(t.TempDir(), "p")); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(doer.calls) != 0 || len(g.calls) == 0 || g.calls[0][0] != "clone" {
		t.Fatalf("expected git clone, http=%v git=%v", doer.calls, g.calls)
	}
}

func TestFetchedClaudePluginLoads(t *testing.T) {
	// A materialized tree with only .claude-plugin/plugin.json is a plugin.
	g := claudeGit{}
	f := &Fetcher{Git: g}
	dest := filepath.Join(t.TempDir(), "p")
	dir, err := f.Fetch(plugins.Source{Kind: plugins.SourceGitHub, Repo: "acme/p"}, dest)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plugins.LoadPluginDir(osFS(), dir, "acme", plugins.OriginInstalled)
	if err != nil || p.Name() != "claude-only" {
		t.Fatalf("load fetched claude plugin: %v %+v", err, p)
	}
}
