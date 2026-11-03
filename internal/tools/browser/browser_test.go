package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/chromehost"
)

// fakeClient returns a canned raw reply per method (or a canned error),
// recording the last method/params it saw.
type fakeClient struct {
	byMethod map[string]json.RawMessage
	err      error
	method   string
	params   json.RawMessage
}

func (f *fakeClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	f.method = method
	if params != nil {
		f.params, _ = json.Marshal(params)
	} else {
		f.params = nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.byMethod[method], nil
}

func TestNavigate_MapsResult(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodNavigate: json.RawMessage(`{"tabId":12,"url":"https://go.dev"}`),
	}}
	res, err := NavigateTool{Client: c}.Execute(context.Background(), NavigateInput{URL: "https://go.dev"})
	if err != nil {
		t.Fatal(err)
	}
	if c.method != chromehost.MethodNavigate {
		t.Fatalf("method = %q", c.method)
	}
	if !strings.Contains(res.Text, "12") || !strings.Contains(res.Text, "https://go.dev") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestNavigate_RequiresURL(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{}}
	if _, err := (NavigateTool{Client: c}).Execute(context.Background(), NavigateInput{}); err == nil {
		t.Fatal("want error for empty url")
	}
	if c.method != "" {
		t.Fatalf("client should not have been called, method=%q", c.method)
	}
}

func TestReadPage_MapsTextAndTruncation(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodReadPage: json.RawMessage(`{"tabId":1,"text":"hello world","truncated":true}`),
	}}
	res, err := ReadPageTool{Client: c, Method: chromehost.MethodReadPage}.Execute(context.Background(), ReadPageInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "hello world") || !strings.Contains(res.Text, "truncated") {
		t.Fatalf("text = %q", res.Text)
	}
	if len(res.Images) != 0 {
		t.Fatalf("read_page must not produce images")
	}
}

func TestTabsContext_MapsList(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodTabsContext: json.RawMessage(`{"tabs":[{"id":1,"url":"https://a","title":"A","active":true},{"id":2,"url":"https://b","title":"B"}]}`),
	}}
	res, err := TabsContextTool{Client: c}.Execute(context.Background(), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[1]", "A", "https://a", "[2]", "B", "https://b"} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("text %q missing %q", res.Text, want)
		}
	}
}

func TestTabsCreateAndClose(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodTabsCreate: json.RawMessage(`{"tabId":9,"url":"https://x"}`),
		chromehost.MethodTabsClose:  json.RawMessage(`{"closed":9}`),
	}}
	cr, err := TabsCreateTool{Client: c}.Execute(context.Background(), TabsCreateInput{URL: "https://x"})
	if err != nil || !strings.Contains(cr.Text, "9") {
		t.Fatalf("create: %v text=%q", err, cr.Text)
	}
	cl, err := TabsCloseTool{Client: c}.Execute(context.Background(), TabsCloseInput{TabID: 9})
	if err != nil || !strings.Contains(cl.Text, "9") {
		t.Fatalf("close: %v text=%q", err, cl.Text)
	}
}

func TestScreenshot_DecodesImage(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfakebytes"))
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodScreenshot: json.RawMessage(`{"dataUrl":"data:image/png;base64,` + png + `"}`),
	}}
	res, err := ScreenshotTool{Client: c}.Execute(context.Background(), ScreenshotInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("want 1 image, got %d", len(res.Images))
	}
	if res.Images[0].MediaType != "image/png" {
		t.Fatalf("mediaType = %q", res.Images[0].MediaType)
	}
	if res.Images[0].Base64 != png {
		t.Fatalf("base64 payload not preserved")
	}
	if res.Text == "" {
		t.Fatalf("screenshot should carry a caption")
	}
}

func TestScreenshot_RejectsMalformedDataURL(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodScreenshot: json.RawMessage(`{"dataUrl":"notadataurl"}`),
	}}
	if _, err := (ScreenshotTool{Client: c}).Execute(context.Background(), ScreenshotInput{}); err == nil {
		t.Fatal("want error for malformed data URL")
	}
}

func TestNotConnected_PropagatesSentinel(t *testing.T) {
	c := &fakeClient{err: chromehost.ErrChromeNotConnected}
	_, err := NavigateTool{Client: c}.Execute(context.Background(), NavigateInput{URL: "https://x"})
	if !errors.Is(err, chromehost.ErrChromeNotConnected) {
		t.Fatalf("err = %v, want ErrChromeNotConnected", err)
	}
	// Its message is the clear, user-facing instruction.
	if !strings.Contains(err.Error(), "extension") {
		t.Fatalf("not-connected message = %q", err.Error())
	}
}

func TestNilClient(t *testing.T) {
	if _, err := (TabsContextTool{}).Execute(context.Background(), struct{}{}); err == nil {
		t.Fatal("want ErrNoClient for nil client")
	}
}

func TestReadPage_Paging(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodReadPage: json.RawMessage(`{"tabId":1,"text":"part one","truncated":true,"total":120000,"offset":0,"next":40000}`),
	}}
	res, err := ReadPageTool{Client: c, Method: chromehost.MethodReadPage}.Execute(context.Background(), ReadPageInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "characters 0–40000 of 120000; call again with offset 40000") {
		t.Fatalf("text = %q", res.Text)
	}
	c.byMethod[chromehost.MethodReadPage] = json.RawMessage(`{"tabId":1,"text":"the end","truncated":false,"total":120000,"offset":80000,"next":120000}`)
	res, _ = ReadPageTool{Client: c, Method: chromehost.MethodReadPage}.Execute(context.Background(), ReadPageInput{})
	if !strings.Contains(res.Text, "end of the page") {
		t.Fatalf("last part = %q", res.Text)
	}
	c.byMethod[chromehost.MethodReadPage] = json.RawMessage(`{"tabId":1,"text":"","total":10,"offset":50,"next":50}`)
	res, _ = ReadPageTool{Client: c, Method: chromehost.MethodReadPage}.Execute(context.Background(), ReadPageInput{})
	if !strings.Contains(res.Text, "no text at offset 50") {
		t.Fatalf("past the end = %q", res.Text)
	}
}
