package toolrun

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/browser"
)

func TestBrowserDescriptionsAskTheModelToCloseItsTabs(t *testing.T) {
	want := map[protocol.Tool][]string{
		protocol.ToolBrowserNavigate:    {"A tab opened for you this way is yours to clean up, the same as one from tabs_create: close it with tabs_close once you no longer need it and before finishing your task.", "set keep_open instead of leaving it open"},
		protocol.ToolBrowserTabsCreate:  {"Tabs you create are yours to clean up: close each one with tabs_close as soon as you no longer need it, and close any that remain before finishing your task.", "set keep_open instead of leaving it open"},
		protocol.ToolBrowserTabsClose:   {"Use to clean up tabs you're done with.", "call tabs_context first to get valid ids"},
		protocol.ToolBrowserTabsContext: {"this session's"},
	}
	for tool, parts := range want {
		for _, p := range parts {
			if !strings.Contains(toolDescriptions[tool], p) {
				t.Errorf("%s description lacks %q", tool, p)
			}
		}
	}
}

func TestBrowserSchemasOfferKeepOpen(t *testing.T) {
	for _, tool := range []protocol.Tool{protocol.ToolBrowserNavigate, protocol.ToolBrowserTabsCreate} {
		var schema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(toolInputSchemas[tool], &schema); err != nil {
			t.Fatalf("%s schema: %v", tool, err)
		}
		p, ok := schema.Properties["keep_open"]
		if !ok || p.Type != "boolean" || p.Description != browser.KeepOpenDescription {
			t.Errorf("%s keep_open = %+v (present %v)", tool, p, ok)
		}
		for _, r := range schema.Required {
			if r == "keep_open" {
				t.Errorf("%s: keep_open must be optional", tool)
			}
		}
	}
	if browser.KeepOpenDescription != "Set true when the user asked to see this page or keep it open; it then stays open after the session ends." {
		t.Fatalf("description = %q", browser.KeepOpenDescription)
	}
}
