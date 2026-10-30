package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestCompatible_ListModels_XAIArrayPricing: an xAI-shaped /models response
// whose pricing field is an array still lists every model.
func TestCompatible_ListModels_XAIArrayPricing(t *testing.T) {
	srv, _ := compatRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[`+
			`{"id":"grok-build-0.1","object":"model","created":1,"owned_by":"xai","pricing":[{"unit":"token","prompt":"0.000002"}]},`+
			`{"id":"grok-4","object":"model","created":1,"owned_by":"xai","pricing":[]},`+
			`{"id":"grok-3","object":"model","created":1,"owned_by":"xai","pricing":["odd",3]}]}`)
	})
	models, err := (&Compatible{Spec: lookupSpec("xai"), APIKey: "k", BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
}

// TestCompatible_ListModels_ObjectPricing: an object pricing field is still
// read as a price.
func TestCompatible_ListModels_ObjectPricing(t *testing.T) {
	srv, _ := compatRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"m1","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`)
	})
	models, err := (&Compatible{Spec: lookupSpec("groq"), APIKey: "k", BaseURL: srv.URL}).ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("models = %+v, %v", models, err)
	}
}

// TestListedPricing_Shapes: every pricing shape decodes without error; only
// usable shapes yield a price.
func TestListedPricing_Shapes(t *testing.T) {
	cases := []struct {
		rec   string
		price bool
		in    float64
	}{
		{`{"id":"a","pricing":{"input":"0.000003","output":"0.000015"}}`, true, 3},
		{`{"id":"b","pricing":[{"input":"0.000003","output":"0.000015"}]}`, true, 3},
		{`{"id":"c","pricing":[]}`, false, 0},
		{`{"id":"d","pricing":null}`, false, 0},
		{`{"id":"e","pricing":"free"}`, false, 0},
		{`{"id":"f","pricing":42}`, false, 0},
		{`{"id":"g","pricing":{"input":{"usd":1},"output":"0.000015","input_tiers":"x"}}`, false, 0},
		{`{"id":"h","pricing":{"input":"0.000003","output":"0.000015","input_tiers":{"bad":true}}}`, true, 3},
	}
	for _, tc := range cases {
		var w openAIModelWire
		if err := json.Unmarshal([]byte(tc.rec), &w); err != nil {
			t.Fatalf("%s: %v", tc.rec, err)
		}
		p := w.live().Pricing
		if (p != nil) != tc.price {
			t.Fatalf("%s: price = %+v", tc.rec, p)
		}
		if p != nil && p.Input != tc.in {
			t.Errorf("%s: input = %v", tc.rec, p.Input)
		}
	}
}
