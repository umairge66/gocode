package provider

import "testing"

func TestRegion(t *testing.T) {
	if !IsRegionLike("us-east5") || !IsRegionLike("europe-west1") {
		t.Error("expected region-like")
	}
	if IsRegionLike("x") || IsRegionLike("has space") {
		t.Error("unexpected region-like")
	}
}

func TestVertexEndpoint(t *testing.T) {
	if VertexEndpoint("global") != "https://aiplatform.googleapis.com" {
		t.Error("global endpoint wrong")
	}
	if VertexEndpoint("us") != "https://aiplatform.us.rep.googleapis.com" {
		t.Error("us endpoint wrong")
	}
	if VertexEndpoint("us-east5") != "https://us-east5-aiplatform.googleapis.com" {
		t.Error("regional endpoint wrong")
	}
}
