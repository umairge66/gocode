package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/tools/browser"
)

func TestToolHeaderBrowser(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		verb, args  string
	}{
		{"computer", `{"action":"type","tabId":1,"text":"Umair Saeed"}`, "Type", `"Umair Saeed"`},
		{"computer", `{"action":"key","text":"Enter"}`, "Key", "Enter"},
		{"computer", `{"action":"left_click","coordinate":[412,318]}`, "Click", "412, 318"},
		{"computer", `{"action":"left_click","ref":"ref_12"}`, "Click", "ref_12"},
		{"computer", `{"action":"scroll","coordinate":[1,2],"scroll_direction":"down","scroll_amount":3}`, "Scroll", "down ×3"},
		{"computer", `{"action":"screenshot"}`, "Screenshot", ""},
		{"computer", `{"action":"wait","duration":2}`, "Wait", "2s"},
		{"navigate", `{"url":"https://www.google.com/search?q=x&sca_esv=abc"}`, "Navigate", "google.com/search"},
		{"read_page", `{"tabId":1}`, "Read page", ""},
		{"screenshot", `{"windowId":1}`, "Screenshot", ""},
		{"tabs_context", `{}`, "Tabs", ""},
		{"tabs_create", `{}`, "New tab", ""},
		{"find", `{"query":"start button"}`, "Find", `"start button"`},
		{"browser_batch", `{"actions":[{"name":"navigate","input":{}},{"name":"computer","input":{"action":"type"}},{"name":"computer","input":{"action":"key"}}]}`, "Browser", "3 steps: navigate, type, key"},
		{"TodoWrite", `{"todos":[]}`, "Update Todos", ""},
		{"Bash", `{"command":"go test ./..."}`, "Bash", "go test ./..."},
	} {
		verb, args := toolHeader(tc.name, json.RawMessage(tc.input))
		if verb != tc.verb || args != tc.args {
			t.Errorf("toolHeader(%s, %s) = %q, %q; want %q, %q", tc.name, tc.input, verb, args, tc.verb, tc.args)
		}
	}
}

func TestDisplayToolResultBrowser(t *testing.T) {
	page := "<untrusted-web-content source=\"https://www.google.com/search?q=Umair+Saeed&sca_esv=f2142\">\nSkip to main content\nAll\n</untrusted-web-content>\n\n" + browser.UntrustedTextNotice
	got := displayToolResult("read_page", json.RawMessage(`{}`), page)
	if got != "Page: google.com/search\nSkip to main content\nAll" {
		t.Errorf("read_page result = %q", got)
	}

	shot := "Screenshot of the active tab (image/png, ~144453 bytes).\n\n" + browser.UntrustedImageNotice
	if got := displayToolResult("screenshot", nil, shot); got != "Captured screenshot" {
		t.Errorf("screenshot result = %q", got)
	}

	if got := displayToolResult("computer", json.RawMessage(`{"action":"type","text":"Umair Saeed"}`), "computer: type done."); got != "Typed 11 characters" {
		t.Errorf("type result = %q", got)
	}
	if got := displayToolResult("computer", json.RawMessage(`{"action":"key","text":"Enter"}`), "computer: key done."); got != "Pressed Enter" {
		t.Errorf("key result = %q", got)
	}

	// The model-facing notice never reaches the transcript.
	if strings.Contains(displayToolResult("find", nil, "<untrusted-web-content source=\"https://a.b/\">\nx\n</untrusted-web-content>\n\n"+browser.UntrustedTextNotice), "IMPORTANT") {
		t.Error("find result still shows the untrusted-content notice")
	}

	// Non-browser results are untouched.
	if got := displayToolResult("Bash", nil, "ok\n"); got != "ok\n" {
		t.Errorf("Bash result changed: %q", got)
	}
}
