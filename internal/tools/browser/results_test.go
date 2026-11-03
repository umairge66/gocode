package browser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/umairge66/gocode/internal/chromehost"
)

// The schema declares coordinates and counts as numbers: fractional values
// are rounded, not refused.
func TestComputerInputAcceptsFractionalNumbers(t *testing.T) {
	var in ComputerInput
	err := json.Unmarshal([]byte(`{"action":"left_click_drag","coordinate":[100.6,20.2],"start_coordinate":[1.5,2],"region":[0,0,10.4,5e1],"repeat":2.0,"scroll_amount":3.7}`), &in)
	if err != nil {
		t.Fatal(err)
	}
	if got := []Num{in.Coordinate[0], in.Coordinate[1], in.StartCoordinate[0], in.Region[2], in.Region[3], in.Repeat, in.ScrollAmount}; !equalNums(got, Nums(101, 20, 2, 10, 50, 2, 4)) {
		t.Fatalf("decoded %v", got)
	}
	if err := json.Unmarshal([]byte(`{"coordinate":["a",1]}`), &in); err == nil {
		t.Fatal("a string coordinate was accepted")
	}
	out, _ := json.Marshal(ComputerInput{Action: "left_click", Coordinate: Nums(3, 4)})
	if !strings.Contains(string(out), `"coordinate":[3,4]`) {
		t.Fatalf("encoded %s", out)
	}
}

func equalNums(a, b []Num) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFindRendersElementDetails(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodFind: json.RawMessage(`{"matches":[
			{"ref":"ref_7","role":"textbox","text":"Full name","x":270,"y":72,"tag":"input","type":"text","name":"fullName","value":""},
			{"ref":"ref_8","role":"button","text":"Submit","x":50,"y":900,"tag":"button","offscreen":true,"disabled":true}],"truncated":true}`),
	}}
	res, err := FindTool{Client: c}.Execute(context.Background(), FindInput{Query: "full name", Limit: 5, IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`- ref_7 [textbox] "Full name" at (270, 72) — input type=text, name="fullName", value=""`,
		`- ref_8 [button] "Submit" at (50, 900) — button, disabled, off-screen`,
		"[more matches exist",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing %q in:\n%s", want, res.Text)
		}
	}
	if !strings.Contains(string(c.params), `"limit":5`) || !strings.Contains(string(c.params), `"includeHidden":true`) {
		t.Errorf("params %s", c.params)
	}
}

func TestComputerReportsZoomFrameAndNotes(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodComputer: json.RawMessage(`{"dataUrl":"data:image/png;base64,aW1n","width":800,"height":600,"region":[100,50,300,150],"zoom":4,"imageWidth":800,"imageHeight":400}`),
	}}
	res, err := ComputerTool{Client: c}.Execute(context.Background(), ComputerInput{Action: "zoom", Region: Nums(100, 50, 300, 150)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "Zoomed region (100, 50)-(300, 150) at 4x (800x400 image)") || !strings.Contains(res.Text, "full 800x600 viewport frame") {
		t.Fatalf("zoom text:\n%s", res.Text)
	}
	c.byMethod[chromehost.MethodComputer] = json.RawMessage(`{"action":"left_click","ok":true,"notes":["A JavaScript confirm dialog opened on tab 3 saying \"Sure?\" and gocode dismissed it (answered Cancel)."]}`)
	res, err = ComputerTool{Client: c}.Execute(context.Background(), ComputerInput{Action: "left_click", Coordinate: Nums(1, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "computer: left_click done.") || !strings.Contains(res.Text, "<untrusted-web-content>\n- A JavaScript confirm dialog") {
		t.Fatalf("notes text:\n%s", res.Text)
	}
}

// A computer action says how the input reached the page: real input is
// "done.", DOM events sent from inside a background tab say so and why, and
// an input the page could not be checked for is not called done.
func TestComputerReportsHowInputArrived(t *testing.T) {
	cases := []struct {
		action, raw string
		want        []string
		not         string
	}{
		{"left_click", `{"action":"left_click","ok":true,"delivery":"input","background":true}`,
			[]string{"computer: left_click done."}, "DOM events"},
		{"left_click", `{"action":"left_click","ok":true,"delivery":"dom","domReason":"not-delivered","background":true}`,
			[]string{"computer: left_click done with DOM events, not real input: Chrome did not deliver real input to the page (the tab is in the background)", "event.isTrusted is false", "Take a screenshot"}, ""},
		{"key", `{"action":"key","ok":true,"delivery":"dom","domReason":"not-delivered","keysCancelled":true}`,
			[]string{"computer: key done with DOM events, not real input: Chrome did not deliver real input to the page, so", "cancelled some of the key presses"}, "background)"},
		{"scroll", `{"action":"scroll","ok":true,"delivery":"dom","domReason":"background-scroll","background":true,"scrolled":false}`,
			[]string{"computer: scroll done with DOM events, not real input: the tab is in the background, where a real mouse wheel can make Chrome switch the user's tabs", "nothing scrolled"}, ""},
		{"left_click_drag", `{"action":"left_click_drag","ok":true,"delivery":"dom","domReason":"not-delivered","drag":"drag-and-drop"}`,
			[]string{"computer: left_click_drag done with DOM events, not real input (as a drag-and-drop):"}, ""},
		{"left_click", `{"action":"left_click","ok":true,"delivery":"unconfirmed"}`,
			[]string{"computer: left_click sent, not confirmed: gocode could not check that the page received it"}, " done"},
		{"left_click_drag", `{"action":"left_click_drag","ok":true,"drag":"mouse drag"}`,
			[]string{"computer: left_click_drag done (as a mouse drag)."}, ""},
	}
	for _, tc := range cases {
		c := &fakeClient{byMethod: map[string]json.RawMessage{chromehost.MethodComputer: json.RawMessage(tc.raw)}}
		res, err := ComputerTool{Client: c}.Execute(context.Background(), ComputerInput{Action: tc.action, Coordinate: Nums(1, 2)})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range tc.want {
			if !strings.Contains(res.Text, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.raw, w, res.Text)
			}
		}
		if tc.not != "" && strings.Contains(res.Text, tc.not) {
			t.Errorf("%s: unexpected %q in:\n%s", tc.raw, tc.not, res.Text)
		}
	}

	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodBrowserBatch: json.RawMessage(`{"results":[
			{"name":"computer","ok":true,"result":{"action":"left_click","delivery":"input"}},
			{"name":"computer","ok":true,"result":{"action":"left_click","delivery":"dom","domReason":"not-delivered","background":true}}]}`),
	}}
	res, err := BatchTool{Client: c}.Execute(context.Background(), BatchInput{Actions: make([]BatchAction, 2)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. computer: ok\n", "2. computer: ok: done with DOM events, not real input: Chrome did not deliver real input to the page (the tab is in the background)"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("batch: missing %q in:\n%s", want, res.Text)
		}
	}
}

func TestFormInputReportsTheValueItHolds(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodFormInput: json.RawMessage(`{"ref":"ref_2","ok":true,"value":"United States"}`),
	}}
	res, err := FormInputTool{Client: c}.Execute(context.Background(), FormInputInput{Ref: "ref_2", Value: json.RawMessage(`"us"`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != `Set ref_2 to "us"; it now holds "United States".` {
		t.Fatalf("text = %q", res.Text)
	}
}

// A batch returns what each step produced, not just "ok".
func TestBatchReportsStepResults(t *testing.T) {
	c := &fakeClient{byMethod: map[string]json.RawMessage{
		chromehost.MethodBrowserBatch: json.RawMessage(`{"results":[
			{"name":"find","ok":true,"result":{"matches":[{"ref":"ref_3","role":"button","text":"Go","x":5,"y":6,"tag":"button"}]}},
			{"name":"read_page","ok":true,"result":{"tabId":4,"url":"https://a.example/","text":"` + strings.Repeat("w", maxBatchStepText+10) + `"}},
			{"name":"javascript_tool","ok":true,"result":{"tabId":4,"value":"42"}},
			{"name":"tabs_create","ok":true,"result":{"tabId":9,"url":"https://b.example/"}},
			{"name":"computer","ok":true,"dataUrl":"data:image/png;base64,aW1n","result":{"width":800,"height":600,"scale":0.5,"imageWidth":400,"imageHeight":300}},
			{"name":"computer","ok":true,"imageOmitted":"the image was left out: the batch's images reached the reply size limit"},
			{"name":"computer","ok":true,"result":{"action":"left_click"},"notes":["A JavaScript alert dialog opened on tab 4 and gocode dismissed it."]},
			{"name":"form_input","error":"ref_1 is not on this page"}]}`),
	}}
	res, err := BatchTool{Client: c}.Execute(context.Background(), BatchInput{Actions: make([]BatchAction, 8)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1. find: ok\n<untrusted-web-content>\n- ref_3 [button] \"Go\" at (5, 6)",
		"2. read_page: ok\n<untrusted-web-content source=\"https://a.example/\">",
		"[page text truncated; read_page on its own returns more]",
		"3. javascript_tool: ok\n<untrusted-web-content source=\"tab 4\">\n42\n",
		"4. tabs_create: ok: tab 9 at https://b.example/",
		"5. computer: image captured. Scaled to 0.5 (400x300 image)",
		"6. computer: ok (the image was left out",
		"7. computer: ok\nNotes:\n<untrusted-web-content>\n- A JavaScript alert dialog",
		"8. form_input: error: ref_1 is not on this page",
		UntrustedTextNotice,
		UntrustedImageNotice,
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(res.Text, UntrustedTextNotice) != 1 {
		t.Errorf("text notice appears %d times", strings.Count(res.Text, UntrustedTextNotice))
	}
	if strings.Contains(res.Text, strings.Repeat("w", maxBatchStepText+1)) {
		t.Error("read_page text not clipped")
	}
	if len(res.Images) != 1 {
		t.Errorf("images = %d", len(res.Images))
	}
}
