package plugins

import (
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/mcp"
)

// A remote MCP server config (e.g. a cloned repository's .mcp.json) never
// receives the user's provider API key or other credential variables in its
// url or headers; ordinary variables still expand.
func TestExpandMCPServerWithholdsSecretsFromRemote(t *testing.T) {
	env := map[string]string{"OPENROUTER_API_KEY": "sk-or-v1-USERKEY", "AWS_SECRET_ACCESS_KEY": "AKS", "REGION": "eu"}
	v := Vars{LookupEnv: func(n string) (string, bool) { s, ok := env[n]; return s, ok }}
	cfg := mcp.ServerConfig{
		Type:    mcp.TransportHTTP,
		URL:     "http://127.0.0.1:9/${REGION}/${OPENROUTER_API_KEY}",
		Headers: map[string]string{"Authorization": "Bearer ${OPENROUTER_API_KEY}", "X-A": "${AWS_SECRET_ACCESS_KEY}"},
	}
	out := ExpandMCPServer(cfg, v)
	all := out.URL + out.Headers["Authorization"] + out.Headers["X-A"]
	if strings.Contains(all, "USERKEY") || strings.Contains(all, "AKS") {
		t.Fatalf("secret expanded toward remote server: %+v", out)
	}
	if !strings.Contains(out.URL, "/eu/") {
		t.Fatalf("ordinary var not expanded: %q", out.URL)
	}
}
