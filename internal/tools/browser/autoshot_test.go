package browser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/chromehost"
)

// A page-changing action asks for a screenshot after itself by default, the
// input's screenshot flag overrides the default, and observing actions never
// send the flag.
func TestComputerAsksForScreenshotAfterAction(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		action string
		flag   *bool
		def    bool
		want   bool
	}{
		{"left_click", nil, true, true},
		{"middle_click", nil, true, true},
		{"type", nil, true, true},
		{"key", nil, true, true},
		{"scroll", nil, true, true},
		{"scroll_to", nil, true, true},
		{"left_click_drag", nil, true, true},
		{"hover", nil, true, true},
		{"left_click", &no, true, false},
		{"left_click", nil, false, false},
		{"left_click", &yes, false, true},
		{"screenshot", &yes, true, false},
		{"zoom", nil, true, false},
		{"wait", nil, true, false},
	}
	for _, tc := range cases {
		c := &fakeClient{byMethod: map[string]json.RawMessage{
			chromehost.MethodComputer: json.RawMessage(`{"ok":true,"dataUrl":"data:image/png;base64,aW1n"}`),
		}}
		in := ComputerInput{Action: tc.action, Coordinate: Nums(1, 2), Screenshot: tc.flag, Region: Nums(0, 0, 5, 5)}
		if _, err := (ComputerTool{Client: c, AutoScreenshot: tc.def}).Execute(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		var sent map[string]any
		_ = json.Unmarshal(c.params, &sent)
		got, has := sent["screenshot"]
		if tc.want && got != true {
			t.Errorf("%s flag=%v def=%v: sent screenshot=%v, want true", tc.action, tc.flag, tc.def, got)
		}
		if !tc.want && has {
			t.Errorf("%s flag=%v def=%v: sent screenshot=%v, want none", tc.action, tc.flag, tc.def, got)
		}
	}
}

func TestComputerAttachesScreenshotAfterAction(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodComputer: json.RawMessage(`{"action":"left_click","ok":true,"delivery":"input",
			"dataUrl":"data:image/jpeg;base64,aW1n","shot":{"width":800,"height":600,"devicePixelRatio":1}}`),
	}}
	res, err := ComputerTool{Client: c, AutoScreenshot: true}.Execute(context.Background(), ComputerInput{Action: "left_click", Coordinate: Nums(1, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "computer: left_click done. Screenshot after the action attached.") || !strings.Contains(res.Text, UntrustedImageNotice) {
		t.Errorf("text:\n%s", res.Text)
	}
	if len(res.Images) != 1 || res.Images[0].MediaType != "image/jpeg" || res.Images[0].Base64 != "aW1n" {
		t.Errorf("images = %+v", res.Images)
	}

	// The delivery-check wording stays; the screenshot line follows it.
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"left_click","ok":true,"delivery":"dom","domReason":"not-delivered","background":true,
		"dataUrl":"data:image/jpeg;base64,aW1n","shot":{"width":800,"height":600}}`)
	res, _ = ComputerTool{Client: c, AutoScreenshot: true}.Execute(context.Background(), ComputerInput{Action: "left_click", Coordinate: Nums(1, 2)})
	if !strings.Contains(res.Text, "Chrome did not deliver real input to the page (the tab is in the background)") ||
		!strings.Contains(res.Text, "Take a screenshot to check the result. Screenshot after the action attached.") || len(res.Images) != 1 {
		t.Errorf("dom delivery text:\n%s", res.Text)
	}

	// No screenshot: the reason is said, the action is still done.
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"type","ok":true,"delivery":"input","screenshotError":"gocode in Chrome is denied on bank.example."}`)
	res, _ = ComputerTool{Client: c, AutoScreenshot: true}.Execute(context.Background(), ComputerInput{Action: "type", Text: "x"})
	if res.Text != "computer: type done. No screenshot after the action: gocode in Chrome is denied on bank.example." || len(res.Images) != 0 {
		t.Errorf("error text = %q images %d", res.Text, len(res.Images))
	}

	// An older extension that ignores the flag: the plain text, no image.
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"key","ok":true,"delivery":"input"}`)
	res, _ = ComputerTool{Client: c, AutoScreenshot: true}.Execute(context.Background(), ComputerInput{Action: "key", Text: "a"})
	if res.Text != "computer: key done." || len(res.Images) != 0 {
		t.Errorf("old extension text = %q", res.Text)
	}
}

// In a batch only the last step asks for a screenshot; its image is reported
// with the step's outcome.
func TestBatchScreenshotOnlyAfterLastStep(t *testing.T) {
	click := json.RawMessage(`{"action":"left_click","coordinate":[1,2],"screenshot":true}`)
	typ := json.RawMessage(`{"action":"type","text":"x"}`)
	shot := json.RawMessage(`{"action":"screenshot"}`)
	off := json.RawMessage(`{"action":"key","text":"a","screenshot":false}`)
	flags := func(actions []BatchAction, def bool) []any {
		var out []any
		for _, a := range setShotFlags(actions, def) {
			var m map[string]any
			_ = json.Unmarshal(a.Input, &m)
			out = append(out, m["screenshot"])
		}
		return out
	}
	eq := func(got []any, want ...any) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		actions []BatchAction
		def     bool
		want    []any
	}{
		{[]BatchAction{{Name: "computer", Input: click}, {Name: "computer", Input: typ}}, true, []any{nil, true}},
		{[]BatchAction{{Name: "computer", Input: click}, {Name: "computer", Input: shot}}, true, []any{nil, nil}},
		{[]BatchAction{{Name: "computer", Input: click}, {Name: "navigate", Input: json.RawMessage(`{"url":"a.example"}`)}}, true, []any{nil, nil}},
		{[]BatchAction{{Name: "computer", Input: typ}, {Name: "computer", Input: off}}, true, []any{nil, nil}},
		{[]BatchAction{{Name: "computer", Input: typ}, {Name: "computer", Input: typ}}, false, []any{nil, nil}},
		{[]BatchAction{{Name: "computer", Input: typ}, {Name: "computer", Input: click}}, false, []any{nil, true}},
	}
	for i, tc := range cases {
		if got := flags(tc.actions, tc.def); !eq(got, tc.want...) {
			t.Errorf("case %d: flags %v, want %v", i, got, tc.want)
		}
	}

	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodBrowserBatch: json.RawMessage(`{"results":[
			{"name":"computer","ok":true,"result":{"action":"left_click","delivery":"input"}},
			{"name":"computer","ok":true,"dataUrl":"data:image/jpeg;base64,aW1n","result":{"action":"type","delivery":"input","shot":{"width":800,"height":600}}}]}`),
	}}
	res, err := BatchTool{Client: c, AutoScreenshot: true}.Execute(context.Background(), BatchInput{Actions: []BatchAction{{Name: "computer", Input: click}, {Name: "computer", Input: typ}}})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Actions []BatchAction `json:"actions"`
	}
	_ = json.Unmarshal(c.params, &sent)
	if len(sent.Actions) != 2 || strings.Contains(string(sent.Actions[0].Input), "screenshot") || !strings.Contains(string(sent.Actions[1].Input), `"screenshot":true`) {
		t.Errorf("sent %s", c.params)
	}
	if !strings.Contains(res.Text, "1. computer: ok\n") || !strings.Contains(res.Text, "2. computer: ok; screenshot after the action attached") || len(res.Images) != 1 {
		t.Errorf("text:\n%s", res.Text)
	}
}
