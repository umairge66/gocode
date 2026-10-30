package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fullScreenModel builds a full-screen Model with the ANSI-free renderer sized
// to w x h, ready for windowed-layout assertions.
func fullScreenModel(w, h int) *Model {
	m := NewModel(w)
	m.SetRenderer(PlainRenderer{})
	m.SetFullScreen(true)
	m.state.Width = w
	m.state.Height = h
	return m
}

// appendUsers pushes n short user turns ("msg0".."msg{n-1}") through the Model.
func appendUsers(m *Model, n int) {
	var mod tea.Model = m
	for i := 0; i < n; i++ {
		mod, _ = mod.Update(AppendUserMsg{Text: "msg" + itoa(i)})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- full-screen layout: pinned-bottom composer --------------------------

// TestFullScreenPinsComposerAtBottom checks the View renders exactly the
// terminal height and keeps the composer on the LAST row regardless of how long
// the transcript grows.
func TestFullScreenPinsComposerAtBottom(t *testing.T) {
	const h = 12
	m := fullScreenModel(80, h)
	for _, n := range []int{0, 3, 40, 500} {
		m2 := fullScreenModel(80, h)
		appendUsers(m2, n)
		lines := strings.Split(m2.View().Content, "\n")
		if len(lines) != h {
			t.Fatalf("n=%d: View = %d lines, want exactly %d", n, len(lines), h)
		}
		// The composer prompt (PlainRenderer draws "> _") is pinned to the bottom
		// region. The always-on mode line ("… mode on (shift+tab to cycle)") now
		// renders directly below it, so the composer is within the last two rows.
		if !strings.Contains(lines[h-1], ">") && !strings.Contains(lines[h-2], ">") {
			t.Fatalf("n=%d: composer not pinned to the bottom region:\n%s", n, m2.View().Content)
		}
	}
	_ = m
}

// TestStickToBottomShowsLatest checks that with many turns the newest is visible
// (follow mode) and the oldest has scrolled off the top of the pane.
func TestStickToBottomShowsLatest(t *testing.T) {
	m := fullScreenModel(80, 10)
	appendUsers(m, 40)
	view := m.View().Content
	if !strings.Contains(view, "msg39") {
		t.Fatalf("latest turn should be visible in follow mode:\n%s", view)
	}
	if strings.Contains(view, "msg0 ") || strings.Contains(view, "> msg0") {
		t.Fatalf("oldest turn should have scrolled off the top:\n%s", view)
	}
}

// TestPgUpScrollsAndEndReturns checks PgUp scrolls the transcript up (offset
// grows older, leaving follow mode) with the input still pinned, and End jumps
// back to the bottom (follow mode).
func TestPgUpScrollsAndEndReturns(t *testing.T) {
	m := fullScreenModel(80, 10)
	appendUsers(m, 40)
	if !m.follow {
		t.Fatal("should start following the bottom")
	}

	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyPgUp))
	got := mod.(*Model)
	if got.follow {
		t.Fatal("PgUp should leave follow mode")
	}
	view := got.View().Content
	// PgUp scrolled away from the bottom: the latest turn is no longer visible,
	// and the composer stays pinned in the bottom region (the always-on mode line
	// now sits below it, so check the last two rows).
	if strings.Contains(view, "msg39") {
		t.Fatalf("PgUp should scroll away from the latest turn:\n%s", view)
	}
	vl := strings.Split(view, "\n")
	if !strings.Contains(vl[len(vl)-1], ">") && !strings.Contains(vl[len(vl)-2], ">") {
		t.Fatalf("composer must stay pinned while scrolled up:\n%s", view)
	}

	// End returns to the bottom.
	mod, _ = mod.Update(keyType(tea.KeyEnd))
	got = mod.(*Model)
	if !got.follow {
		t.Fatal("End should return to follow (bottom) mode")
	}
	if !strings.Contains(got.View().Content, "msg39") {
		t.Fatalf("End should show the latest turn again:\n%s", got.View().Content)
	}
}

// TestScrolledUpPreservedOnAppend checks that when the user has scrolled up, new
// content appended does NOT yank them to the bottom — the offset is preserved.
func TestScrolledUpPreservedOnAppend(t *testing.T) {
	m := fullScreenModel(80, 10)
	appendUsers(m, 40)
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyPgUp))
	got := mod.(*Model)
	off := got.scrollOffset
	if got.follow || off == 0 {
		t.Fatalf("precondition: should be scrolled up, follow=%v off=%d", got.follow, off)
	}
	// New output arrives while scrolled up.
	mod, _ = mod.Update(AppendAssistantMsg{Text: "a new streaming answer line"})
	got = mod.(*Model)
	if got.follow {
		t.Fatal("appending content must not force follow mode when scrolled up")
	}
	if got.scrollOffset != off {
		t.Fatalf("scroll offset should be preserved on append: %d -> %d", off, got.scrollOffset)
	}
}

// TestSubmitJumpsToBottom checks submitting a line returns to follow (bottom).
func TestSubmitJumpsToBottom(t *testing.T) {
	m := fullScreenModel(80, 10)
	appendUsers(m, 40)
	var mod tea.Model = m
	mod, _ = mod.Update(keyType(tea.KeyPgUp))
	if mod.(*Model).follow {
		t.Fatal("precondition: should be scrolled up")
	}
	mod, _ = mod.Update(keyRunes("hello"))
	mod, _ = mod.Update(keyType(tea.KeyEnter))
	if !mod.(*Model).follow {
		t.Fatal("submit should jump back to the bottom (follow mode)")
	}
}

// TestBannerIsFirstTranscriptItem checks the welcome banner heads the scrollable
// transcript (it is not committed away): after scrolling to the top it shows.
func TestBannerIsFirstTranscriptItem(t *testing.T) {
	m := fullScreenModel(80, 24)
	var mod tea.Model = m
	mod, _ = mod.Update(SetBannerMsg{Banner: &Banner{AppName: "gocode", Version: "9.9.9"}})
	mod, _ = mod.Update(AppendSystemMsg{Text: "first notice"})

	// The banner is the first item of RenderTranscript.
	tr := mod.(*Model).renderer.RenderTranscript(mod.(*Model).state)
	joined := strings.Join(tr, "\n")
	bannerIdx := strings.Index(joined, "gocode v9.9.9")
	noticeIdx := strings.Index(joined, "first notice")
	if bannerIdx < 0 || noticeIdx < 0 || bannerIdx > noticeIdx {
		t.Fatalf("banner should head the transcript above the first item:\n%s", joined)
	}
}

// TestToolCallStaysInTranscript checks a tool-call block stays in the transcript
// across its START and END (no commit/drop), and both are rendered.
func TestToolCallStaysInTranscript(t *testing.T) {
	m := NewModel(80)
	m.SetRenderer(PlainRenderer{})
	var mod tea.Model = m

	mod, _ = mod.Update(AppendToolCallMsg{Tool: ToolCall{ID: "t1", Name: "Read", Args: "main.go"}})
	if len(mod.(*Model).State().Transcript) != 1 {
		t.Fatalf("tool block should be in the transcript, got %d", len(mod.(*Model).State().Transcript))
	}
	if !strings.Contains(mod.(*Model).View().Content, "Read(main.go)") {
		t.Fatalf("in-flight tool call should render:\n%s", mod.(*Model).View().Content)
	}
	mod, _ = mod.Update(UpdateToolCallMsg{Tool: ToolCall{ID: "t1", Name: "Read", Args: "main.go", ResultText: "package main"}})
	if len(mod.(*Model).State().Transcript) != 1 {
		t.Fatalf("ended tool block should stay in the transcript, got %d", len(mod.(*Model).State().Transcript))
	}
	view := mod.(*Model).View().Content
	if !strings.Contains(view, "Read(main.go)") || !strings.Contains(view, "package main") {
		t.Fatalf("ended tool block should render header + result:\n%s", view)
	}
}

// --- render memoization (perf) ----------------------------------------------

// TestRenderMarkdownMemoized checks renderMarkdown caches its result by
// (src,width,dark): an identical second call does NOT re-run glamour, while a
// changed width does.
func TestRenderMarkdownMemoized(t *testing.T) {
	mdRenderCacheReset()
	const src = "memo-unique-token **bold** and `code`"

	_ = renderMarkdown(src, 60, true)
	after1 := mdRenderComputeCount
	if after1 == 0 {
		t.Fatal("first render should compute (cache miss)")
	}
	_ = renderMarkdown(src, 60, true)
	if mdRenderComputeCount != after1 {
		t.Fatalf("identical render should be memoized (no recompute): count %d -> %d", after1, mdRenderComputeCount)
	}
	// A different width is a different key and recomputes.
	_ = renderMarkdown(src, 61, true)
	if mdRenderComputeCount != after1+1 {
		t.Fatalf("a width change should recompute: count %d, want %d", mdRenderComputeCount, after1+1)
	}
}

// TestNoChangeTickDoesNotRerenderMarkdown checks that rendering the same
// transcript twice (as a spinner tick would) re-runs no glamour — the per-item
// Markdown memo holds across frames even with the whole history rendered.
func TestNoChangeTickDoesNotRerenderMarkdown(t *testing.T) {
	// Start cold: finished replies go through the streaming block cache too, so
	// clear it as well or a repeat run (-count=2) finds the text already cached.
	mdRenderCacheReset()
	blockCacheReset()
	streamCacheReset()
	renderCachesReset()
	lr := NewLipglossRenderer()
	vs := New(80)
	vs = Reduce(vs, StreamAssistant{Text: "# Heading\n\nsome **finalized** prose body", Done: true})

	_ = lr.RenderTranscript(vs)
	after1 := mdRenderComputeCount
	if after1 == 0 {
		t.Fatal("first transcript render should compute")
	}
	// A no-change frame (e.g. a spinner tick) renders the same transcript again.
	_ = lr.RenderTranscript(vs)
	if mdRenderComputeCount != after1 {
		t.Fatalf("a no-change frame must not re-render Markdown: %d -> %d", after1, mdRenderComputeCount)
	}
}

// TestStreamingDeltaReusesCompletedRender checks the streaming path re-renders
// only the tail: a delta that grows ONLY the in-progress tail (completed prefix
// unchanged) re-runs no glamour, and a delta that completes a NEW block renders
// just that one new segment.
func TestStreamingDeltaReusesCompletedRender(t *testing.T) {
	mdRenderCacheReset()
	streamCacheReset()
	lr := NewLipglossRenderer()

	// completed = "# Title\n\ndone para\n", tail = "incomplete tail".
	vs1 := Reduce(New(80), StreamAssistant{Text: "# Title\n\ndone para\n\nincomplete tail", Done: false})
	_ = lr.Render(vs1)
	c1 := mdRenderComputeCount
	if c1 == 0 {
		t.Fatal("first streaming render should compute the completed block")
	}

	// Grow ONLY the tail: completed prefix identical -> cached completed reused,
	// tail stays plain text -> no new glamour.
	vs2 := Reduce(New(80), StreamAssistant{Text: "# Title\n\ndone para\n\nincomplete tail more", Done: false})
	_ = lr.Render(vs2)
	if mdRenderComputeCount != c1 {
		t.Fatalf("growing only the tail re-ran glamour (completed render not reused): %d -> %d", c1, mdRenderComputeCount)
	}

	// Complete a NEW block: only the newly-appended segment is rendered (one
	// additional compute), not the whole buffer.
	vs3 := Reduce(New(80), StreamAssistant{Text: "# Title\n\ndone para\n\nsecond para\n\ntail", Done: false})
	_ = lr.Render(vs3)
	if mdRenderComputeCount != c1+1 {
		t.Fatalf("completing a new block should render just that segment (+1 compute): %d -> %d", c1, mdRenderComputeCount)
	}
}
