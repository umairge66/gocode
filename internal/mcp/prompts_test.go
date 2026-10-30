package mcp

import (
	"context"
	"testing"
	"time"
)

func TestClientPrompts(t *testing.T) {
	ft := newFakeTransport()
	c := NewClient(ft, Implementation{}, ClientCapabilities{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := c.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Prompts) != 1 || list.Prompts[0].Name != "review" || len(list.Prompts[0].Arguments) != 2 || !list.Prompts[0].Arguments[0].Required {
		t.Fatalf("prompts = %+v", list.Prompts)
	}
	got, err := c.GetPrompt(ctx, GetPromptParams{Name: "review", Arguments: map[string]string{"file": "a.go", "focus": "bugs"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content.Text != "review a.go for bugs" {
		t.Fatalf("get = %+v", got)
	}
}
