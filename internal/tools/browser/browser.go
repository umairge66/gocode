// Package browser provides gocode's model-facing browser-control tools
// (milestone 2). Each tool resolves a chrome-host control client, relays a
// browser verb (navigate / read_page / get_page_text / tabs_context /
// tabs_create / tabs_close / screenshot) to the MV3 extension through the native
// host, and maps the extension's reply into a tool result.
//
// The verb names, parameters and result shapes follow the gocode in-chrome tool
// surface and the milestone-1 host
// method constants (internal/chromehost.Method*), so the model recognizes them.
// screenshot returns an IMAGE result that flows through the existing structured-
// image delivery path (the data: URL is decoded to base64 + media type), so the
// model actually SEES the capture.
//
// LOCAL IPC ONLY: there is no network, no account/OAuth, no cloud bridge and no
// telemetry here — every call is relayed over the per-user unix control socket
// to the local Chrome. When Chrome is not connected, the client returns
// chromehost.ErrChromeNotConnected and the tool surfaces it as a clear, non-fatal
// "connect the extension" result.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/umairge66/gocode/internal/chromehost"
)

// Client is the chrome-host control seam a browser tool relays through.
// *chromehost.Client satisfies it; tests inject a fake extension backend.
type Client interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Image is one decoded image content block (base64 payload + media type) a tool
// produces, carried onto the tool_result as a structured image through the same
// delivery path fileedit.ReadImage uses.
type Image struct {
	Base64    string
	MediaType string
}

// Result is a browser tool's outcome: the model-facing text plus any image
// blocks (today only screenshot produces images).
type Result struct {
	Text   string
	Images []Image
}

// ErrNoClient is returned when a tool has no chrome-host client wired. It should
// not happen in a correctly assembled session (the dispatcher always injects
// one), but it keeps a misconfiguration from panicking.
var ErrNoClient = errors.New("browser tool has no chrome-host client configured")

// resolve returns the client or ErrNoClient.
func resolve(c Client) (Client, error) {
	if c == nil {
		return nil, ErrNoClient
	}
	return c, nil
}

// --- navigate ---------------------------------------------------------------

// NavigateInput is the navigate verb's parameters. tabId is optional: when
// omitted the host navigates the active tab (creating one if none).
//
// KeepOpen marks the tab to stay open after the session ends (true) or clears
// that mark (false); nil leaves the mark as it is.
type NavigateInput struct {
	URL      string `json:"url"`
	TabID    *Num   `json:"tabId,omitempty"`
	KeepOpen *bool  `json:"keep_open,omitempty"`
}

// NavigateTool navigates a tab to a URL. It is an ACTION (it changes browser
// state on the user's logged-in sites) and is gated like Bash.
//
// With ListTabs, a navigation that picked the tab itself (no tabId) appends
// the session's tab listing and the note that those tabs are the model's to
// close (TabCleanupNote).
type NavigateTool struct {
	Client   Client
	ListTabs bool
}

func (t NavigateTool) Execute(ctx context.Context, in NavigateInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.URL) == "" {
		return Result{}, errors.New("navigate requires a url")
	}
	raw, err := c.Call(ctx, chromehost.MethodNavigate, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		TabID int    `json:"tabId"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal(raw, &r)
	text := fmt.Sprintf("Navigated tab %d to %s", r.TabID, r.URL)
	if t.ListTabs && in.TabID == nil && !IsHistoryNavigation(in.URL) {
		if tabs, terr := (TabsContextTool{Client: c}).Execute(ctx, struct{}{}); terr == nil {
			text += "\n\nTab context:\n" + tabs.Text + "\n" + TabCleanupNote(chromehost.MethodTabsClose)
		}
	}
	return Result{Text: text}, nil
}

// --- read_page / get_page_text ----------------------------------------------

// ReadPageInput is the read_page / get_page_text parameters. tabId is optional:
// when omitted the host reads the active tab.
type ReadPageInput struct {
	TabID *Num `json:"tabId,omitempty"`
	// Offset is the character to start from (0 = the start), for reading a
	// long page in parts; MaxChars caps one reply (default PageTextChunk).
	Offset   *Num `json:"offset,omitempty"`
	MaxChars *Num `json:"max_chars,omitempty"`
}

// PageTextChunk is how much page text one read_page / get_page_text reply
// carries by default: small enough to stay inline instead of being saved to a
// file, with the next offset given when the page is longer.
const PageTextChunk = 40000

// ReadPageTool reads the text of a page. Method selects the host verb
// (read_page or get_page_text, which the extension treats identically). It is a
// READ (observes page content) and leans auto-allow like Read.
type ReadPageTool struct {
	Client Client
	Method string
}

func (t ReadPageTool) Execute(ctx context.Context, in ReadPageInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	method := t.Method
	if method == "" {
		method = chromehost.MethodReadPage
	}
	if in.MaxChars == nil {
		n := Num(PageTextChunk)
		in.MaxChars = &n
	}
	raw, err := c.Call(ctx, method, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		TabID     int    `json:"tabId"`
		URL       string `json:"url"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
		Total     *int   `json:"total"`
		Offset    int    `json:"offset"`
		Next      int    `json:"next"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.Text == "" {
		if r.Total != nil && *r.Total > 0 {
			return Result{Text: fmt.Sprintf("(no text at offset %d: the page has %d characters)", r.Offset, *r.Total)}, nil
		}
		return Result{Text: "(the page has no readable text)"}, nil
	}
	text := wrapUntrusted(tabSource(r.URL, r.TabID), r.Text)
	switch {
	case r.Truncated && r.Total != nil:
		text += fmt.Sprintf("\n[characters %d–%d of %d; call again with offset %d for the rest]", r.Offset, r.Next, *r.Total, r.Next)
	case r.Truncated:
		// An older extension that cannot page.
		text += "\n[page text truncated]"
	case r.Offset > 0 && r.Total != nil:
		text += fmt.Sprintf("\n[characters %d–%d of %d: end of the page]", r.Offset, r.Next, *r.Total)
	}
	return Result{Text: text}, nil
}

// --- tabs_context -----------------------------------------------------------

// TabsContextTool lists the open tabs. It is a READ and leans auto-allow like
// Read.
type TabsContextTool struct{ Client Client }

type tabInfo struct {
	ID     int    `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

func (t TabsContextTool) Execute(ctx context.Context, _ struct{}) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	raw, err := c.Call(ctx, chromehost.MethodTabsContext, nil)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		Tabs []tabInfo `json:"tabs"`
	}
	_ = json.Unmarshal(raw, &r)
	if len(r.Tabs) == 0 {
		return Result{Text: "No open tabs."}, nil
	}
	// Tab titles (and URLs) are set by the pages themselves, so the listing is
	// fenced as untrusted content.
	var b strings.Builder
	for _, tb := range r.Tabs {
		marker := " "
		if tb.Active {
			marker = "*"
		}
		fmt.Fprintf(&b, "%s [%d] %s — %s\n", marker, tb.ID, tb.Title, tb.URL)
	}
	header := fmt.Sprintf("%d open tab(s) (titles are set by the pages):\n", len(r.Tabs))
	return Result{Text: header + wrapUntrusted("", strings.TrimRight(b.String(), "\n"))}, nil
}

// --- tabs_create ------------------------------------------------------------

// TabsCreateInput is the tabs_create parameters. KeepOpen marks the new tab
// to stay open after the session ends.
type TabsCreateInput struct {
	URL      string `json:"url,omitempty"`
	KeepOpen *bool  `json:"keep_open,omitempty"`
}

// TabsCreateTool opens a new tab (optionally at a URL). It is an ACTION and is
// gated like Bash.
type TabsCreateTool struct{ Client Client }

func (t TabsCreateTool) Execute(ctx context.Context, in TabsCreateInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	raw, err := c.Call(ctx, chromehost.MethodTabsCreate, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		TabID int    `json:"tabId"`
		URL   string `json:"url"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.URL == "" {
		return Result{Text: fmt.Sprintf("Created tab %d", r.TabID)}, nil
	}
	return Result{Text: fmt.Sprintf("Created tab %d at %s", r.TabID, r.URL)}, nil
}

// --- tabs_close -------------------------------------------------------------

// TabsCloseInput is the tabs_close parameters.
type TabsCloseInput struct {
	TabID Num `json:"tabId"`
}

// TabsCloseTool closes a tab by id. It is an ACTION and is gated like Bash.
type TabsCloseTool struct{ Client Client }

func (t TabsCloseTool) Execute(ctx context.Context, in TabsCloseInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	raw, err := c.Call(ctx, chromehost.MethodTabsClose, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		Closed int `json:"closed"`
	}
	_ = json.Unmarshal(raw, &r)
	id := r.Closed
	if id == 0 {
		id = int(in.TabID)
	}
	return Result{Text: fmt.Sprintf("Closed tab %d", id)}, nil
}

// --- screenshot -------------------------------------------------------------

// ScreenshotInput is the screenshot parameters. windowId is optional (the host
// captures the active window when omitted).
type ScreenshotInput struct {
	WindowID *int `json:"windowId,omitempty"`
	TabID    *Num `json:"tabId,omitempty"`
	// SaveToDisk also saves the capture into the session's private screenshot
	// directory and reports the path (see ScreenshotSaver). Not sent to the
	// extension.
	SaveToDisk bool `json:"save_to_disk,omitempty"`
	// SaveTo also writes the capture to this absolute path. The session checks
	// it like a Write before the call runs. Not sent to the extension.
	SaveTo string `json:"save_to,omitempty"`
}

// ScreenshotTool captures the visible tab and returns an IMAGE result. It is a
// READ (it only observes the rendered page) and leans auto-allow like Read; the
// decoded PNG flows through the structured-image delivery path so the model sees
// it.
type ScreenshotTool struct {
	Client Client
	// Saver handles save_to_disk (nil -> DefaultScreenshotSaver).
	Saver *ScreenshotSaver
}

func (t ScreenshotTool) Execute(ctx context.Context, in ScreenshotInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	saveToDisk, saveTo := in.SaveToDisk, in.SaveTo
	in.SaveToDisk, in.SaveTo = false, ""
	raw, err := c.Call(ctx, chromehost.MethodScreenshot, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		DataURL string `json:"dataUrl"`
	}
	_ = json.Unmarshal(raw, &r)
	img, err := decodeDataURL(r.DataURL)
	if err != nil {
		return Result{}, err
	}
	nBytes := base64.StdEncoding.DecodedLen(len(img.Base64))
	text := fmt.Sprintf("Screenshot of the gocode tab (%s, ~%d bytes).\n\n%s", img.MediaType, nBytes, UntrustedImageNotice)
	text = appendLines(text, saveImages(t.Saver, []Image{img}, saveToDisk, saveTo))
	return Result{Text: text, Images: []Image{img}}, nil
}

// saveImages applies save_to (an explicit path, already permission-checked by
// the caller) and save_to_disk to captured images and returns the result
// lines to append.
func saveImages(saver *ScreenshotSaver, images []Image, saveToDisk bool, saveTo string) []string {
	var lines []string
	if saveTo != "" && len(images) > 0 {
		if err := WriteImageFile(saveTo, images[0]); err != nil {
			lines = append(lines, fmt.Sprintf("Note: save_to failed — %v. The image is included inline above.", err))
		} else {
			lines = append(lines, SavedPathPrefix+saveTo)
		}
	}
	if saveToDisk {
		if saver == nil {
			saver = DefaultScreenshotSaver
		}
		lines = append(lines, saver.SaveImages(images)...)
	}
	return lines
}

// --- computer (milestone 3, CDP input) --------------------------------------

// ComputerInput is the computer verb's parameters. action selects the gesture;
// the other fields are action-specific (coordinate/start_coordinate for mouse
// gestures, text for type/key, scroll_direction/scroll_amount for scroll, ref
// as an alternative element target, region (and scale) for zoom, scale for
// screenshot, duration for wait). tabId targets a specific tab (omit for the
// active tab). Shapes follow the gocode `computer` tool. Pixel values and
// counts are numbers in the schema; a fractional one is rounded (Num).
type ComputerInput struct {
	Action          string  `json:"action"`
	Coordinate      []Num   `json:"coordinate,omitempty"`
	StartCoordinate []Num   `json:"start_coordinate,omitempty"`
	Text            string  `json:"text,omitempty"`
	Duration        float64 `json:"duration,omitempty"`
	ScrollDirection string  `json:"scroll_direction,omitempty"`
	ScrollAmount    Num     `json:"scroll_amount,omitempty"`
	Region          []Num   `json:"region,omitempty"`
	// Scale shrinks a screenshot / zoom image to this fraction (0.1-1).
	Scale     float64 `json:"scale,omitempty"`
	Ref       string  `json:"ref,omitempty"`
	Modifiers string  `json:"modifiers,omitempty"`
	// Repeat presses the key sequence of a key action this many times (1-100).
	Repeat Num  `json:"repeat,omitempty"`
	TabID  *Num `json:"tabId,omitempty"`
	// SaveToDisk / SaveTo: see ScreenshotInput. Only screenshot / zoom use them;
	// neither is sent to the extension.
	SaveToDisk bool   `json:"save_to_disk,omitempty"`
	SaveTo     string `json:"save_to,omitempty"`
	// Screenshot asks a page-changing action (AutoScreenshotAction) for a
	// screenshot of the page after it (true) or none (false); nil takes the
	// tool's default (ComputerTool.AutoScreenshot). The extension receives it
	// only as true.
	Screenshot *bool `json:"screenshot,omitempty"`
}

// screenshotActions are the computer actions that return an image rather than a
// plain acknowledgement (screenshot of the viewport, or zoom of a region).
func isScreenshotAction(action string) bool {
	return action == "screenshot" || action == "zoom"
}

// AutoScreenshotAction reports whether a computer action changes the page and
// so can return a screenshot of it after the action (see
// ComputerTool.AutoScreenshot): clicks, typing, keys, scrolling, drags and
// hovering (which is done to see what it reveals).
func AutoScreenshotAction(action string) bool {
	switch action {
	case "left_click", "click", "right_click", "middle_click", "double_click", "triple_click",
		"type", "key", "keypress", "scroll", "scroll_to", "left_click_drag", "hover", "mouse_move":
		return true
	}
	return false
}

// wantShot is whether a computer action asks the extension for a screenshot
// after it: an action that changes the page, with the input's screenshot
// flag, or the default when it has none.
func wantShot(action string, flag *bool, def bool) bool {
	if !AutoScreenshotAction(action) {
		return false
	}
	if flag != nil {
		return *flag
	}
	return def
}

// ComputerTool performs a mouse/keyboard gesture (or a screenshot) in the user's
// real Chrome through chrome.debugger. It is an ACTION (it manipulates input on
// logged-in sites) and is gated like Bash. The screenshot/zoom actions return an
// IMAGE result through the structured-image delivery path so the model sees it.
//
// With AutoScreenshot, a page-changing action (AutoScreenshotAction) also
// returns a screenshot of the page once it settled, through the same image
// path, unless its input says screenshot:false: the model sees what its click
// did without a second call. An input with screenshot:true asks for one even
// when AutoScreenshot is off.
type ComputerTool struct {
	Client Client
	// Saver handles save_to_disk (nil -> DefaultScreenshotSaver).
	Saver *ScreenshotSaver
	// AutoScreenshot is the default of the input's screenshot flag.
	AutoScreenshot bool
}

func (t ComputerTool) Execute(ctx context.Context, in ComputerInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Action) == "" {
		return Result{}, errors.New("computer requires an action")
	}
	if in.Repeat < 0 || in.Repeat > 100 {
		return Result{}, errors.New("repeat must be an integer between 1 and 100")
	}
	saveToDisk, saveTo := in.SaveToDisk, in.SaveTo
	in.SaveToDisk, in.SaveTo = false, ""
	shot := wantShot(in.Action, in.Screenshot, t.AutoScreenshot)
	in.Screenshot = nil
	if shot {
		in.Screenshot = &shot
	}
	raw, err := c.Call(ctx, chromehost.MethodComputer, in)
	if err != nil {
		return Result{}, err
	}
	var notes struct {
		Notes []string `json:"notes"`
	}
	_ = json.Unmarshal(raw, &notes)
	if isScreenshotAction(in.Action) {
		var r captureInfo
		_ = json.Unmarshal(raw, &r)
		img, err := decodeDataURL(r.DataURL)
		if err != nil {
			return Result{}, err
		}
		nBytes := base64.StdEncoding.DecodedLen(len(img.Base64))
		text := fmt.Sprintf("Captured %s (%s, ~%d bytes).", in.Action, img.MediaType, nBytes)
		if note := r.frameNote(); note != "" {
			text += " " + note
		}
		text += "\n\n" + UntrustedImageNotice + pageNotes(notes.Notes, true)
		text = appendLines(text, saveImages(t.Saver, []Image{img}, saveToDisk, saveTo))
		return Result{Text: text, Images: []Image{img}}, nil
	}
	// How the input reached the page: real input reads "done."; DOM events
	// or an unconfirmed send say so (see inputDelivery).
	var r inputDelivery
	_ = json.Unmarshal(raw, &r)
	text := fmt.Sprintf("computer: %s %s.", in.Action, r.outcome())
	if note := r.note(); note != "" {
		text = fmt.Sprintf("computer: %s %s: %s", in.Action, r.outcome(), note)
	}
	// The screenshot the extension took after the action, when asked.
	var after struct {
		DataURL string `json:"dataUrl"`
	}
	_ = json.Unmarshal(raw, &after)
	var images []Image
	if after.DataURL != "" {
		if img, derr := decodeDataURL(after.DataURL); derr == nil {
			images = append(images, img)
		} else {
			r.ScreenshotError = derr.Error()
		}
	}
	text += r.shotLine(len(images) > 0)
	if len(images) > 0 {
		text += "\n\n" + UntrustedImageNotice
	}
	return Result{Text: text + pageNotes(notes.Notes, true), Images: images}, nil
}

// --- find (milestone 3, CDP element lookup) ---------------------------------

// FindInput is the find verb's parameters: a natural-language query plus the
// tab to search. Shapes follow the gocode `find` tool. Limit caps the matches
// (default 20, at most 100); IncludeHidden also returns elements that are not
// visible.
type FindInput struct {
	Query         string `json:"query"`
	TabID         *Num   `json:"tabId,omitempty"`
	Limit         Num    `json:"limit,omitempty"`
	IncludeHidden bool   `json:"includeHidden,omitempty"`
}

// FindTool locates elements matching a query (by label, role or type words,
// placeholder, text) and returns their refs + center coordinates. It is a READ
// (it only observes the DOM) and leans auto-allow like Read.
type FindTool struct{ Client Client }

func (t FindTool) Execute(ctx context.Context, in FindInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return Result{}, errors.New("find requires a query")
	}
	raw, err := c.Call(ctx, chromehost.MethodFind, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		Matches   []match `json:"matches"`
		Truncated bool    `json:"truncated"`
	}
	_ = json.Unmarshal(raw, &r)
	if len(r.Matches) == 0 {
		return Result{Text: fmt.Sprintf("No elements matched %q.", in.Query)}, nil
	}
	// The element text/roles come from the page, so the match list is fenced as
	// untrusted content; the refs and coordinates remain usable for clicks.
	header := fmt.Sprintf("%d match(es) for %q, best first:\n", len(r.Matches), in.Query)
	text := header + wrapUntrusted("", renderMatches(r.Matches))
	if r.Truncated {
		text += "\n[more matches exist — refine the query]"
	}
	return Result{Text: text}, nil
}

// --- form_input (milestone 3, CDP form fill) --------------------------------

// FormInputInput is the form_input verb's parameters: an element ref and the
// value to set (string / boolean / number). Shapes follow the gocode
// `form_input` tool.
type FormInputInput struct {
	Ref   string          `json:"ref"`
	Value json.RawMessage `json:"value"`
	TabID *Num            `json:"tabId,omitempty"`
}

// FormInputTool sets the value of a form field (by ref) and fires input/change
// events. It is an ACTION (it changes page state) and is gated like Bash.
type FormInputTool struct{ Client Client }

func (t FormInputTool) Execute(ctx context.Context, in FormInputInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Ref) == "" {
		return Result{}, errors.New("form_input requires a ref")
	}
	raw, err := c.Call(ctx, chromehost.MethodFormInput, in)
	if err != nil {
		return Result{}, err
	}
	var r struct {
		Ref   string   `json:"ref"`
		Value *string  `json:"value"`
		Notes []string `json:"notes"`
	}
	_ = json.Unmarshal(raw, &r)
	ref := r.Ref
	if ref == "" {
		ref = in.Ref
	}
	if r.Value == nil {
		return Result{Text: fmt.Sprintf("Set %s to %s.", ref, string(in.Value)) + pageNotes(r.Notes, true)}, nil
	}
	// The extension checked that the value took; it reports what the field
	// holds now (as the page shows it).
	return Result{Text: fmt.Sprintf("Set %s to %s; it now holds %q.", ref, string(in.Value), sanitizePageText(*r.Value)) + pageNotes(r.Notes, true)}, nil
}

// --- browser_batch (milestone 3, sequenced sub-commands) --------------------

// BatchAction is one sub-command in a browser_batch: a tool name plus the input
// that tool would receive standalone.
type BatchAction struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// BatchInput is the browser_batch parameters: a sequence of sub-commands run in
// order, stopping on the first error. Shape follows the gocode
// `browser_batch` tool. SaveToDisk (or save_to_disk on a computer item) saves
// every captured image.
type BatchInput struct {
	Actions    []BatchAction `json:"actions"`
	SaveToDisk bool          `json:"save_to_disk,omitempty"`
}

// batchResult is one sub-command's outcome as returned by the extension: the
// tool name, an ok flag or error string, the step's own result (find's refs,
// read_page's text, a javascript value, a new tab's id), notes (a dismissed
// dialog), and (for screenshot/zoom sub-commands) a data URL that is surfaced
// as an interleaved image — or why it was left out.
type batchResult struct {
	Name         string          `json:"name"`
	OK           bool            `json:"ok"`
	Error        string          `json:"error,omitempty"`
	DataURL      string          `json:"dataUrl,omitempty"`
	ImageOmitted string          `json:"imageOmitted,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Notes        []string        `json:"notes,omitempty"`
}

// ApproveFunc asks the user to allow the browser tools on a site the extension
// refused with "needs approval". A nil error means approved.
type ApproveFunc func(ctx context.Context, site string) error

// BatchTool runs a sequence of browser sub-commands in one round-trip (sharing a
// single debugger attach in the extension). It is an ACTION (it can contain
// clicks/typing) and is gated like Bash. Any screenshot/zoom sub-command's image
// is returned interleaved through the structured-image delivery path.
//
// When a step stops on a site that needs approval and Approve grants it, the
// batch resumes at that step (the earlier steps already ran and are not
// repeated). Without Approve the step's error is reported and the batch stops
// (if an action navigates to a domain without permission, the next item's
// check fails and the batch stops).
type BatchTool struct {
	Client  Client
	Approve ApproveFunc
	// Saver handles save_to_disk (nil -> DefaultScreenshotSaver).
	Saver *ScreenshotSaver
	// AutoScreenshot: a last step that is a page-changing computer action
	// returns a screenshot after itself unless its input says
	// screenshot:false (see ComputerTool.AutoScreenshot). Earlier steps never
	// do: only the last page matters to the model.
	AutoScreenshot bool
}

// setShotFlags settles each computer item's screenshot flag: removed from
// every step but the last, and on the last one true when it is a
// page-changing action that wants one (its own flag, else def), else
// removed.
func setShotFlags(actions []BatchAction, def bool) []BatchAction {
	out := make([]BatchAction, len(actions))
	copy(out, actions)
	for i, a := range out {
		if a.Name != "computer" || len(a.Input) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(a.Input, &fields) != nil {
			continue
		}
		var in struct {
			Action     string `json:"action"`
			Screenshot *bool  `json:"screenshot"`
		}
		_ = json.Unmarshal(a.Input, &in)
		_, had := fields["screenshot"]
		want := i == len(out)-1 && wantShot(in.Action, in.Screenshot, def)
		if !had && !want {
			continue
		}
		delete(fields, "screenshot")
		if want {
			fields["screenshot"] = json.RawMessage("true")
		}
		if b, err := json.Marshal(fields); err == nil {
			out[i].Input = b
		}
	}
	return out
}

// stripSaveFlags removes save_to_disk / save_to from each item's input (the
// extension never sees them) and reports whether any computer item asked to
// save.
func stripSaveFlags(actions []BatchAction) ([]BatchAction, bool) {
	out := make([]BatchAction, len(actions))
	wants := false
	for i, a := range actions {
		out[i] = a
		var fields map[string]json.RawMessage
		if len(a.Input) == 0 || json.Unmarshal(a.Input, &fields) != nil {
			continue
		}
		save, has := fields["save_to_disk"]
		_, hasTo := fields["save_to"]
		if !has && !hasTo {
			continue
		}
		if a.Name == "computer" && string(save) == "true" {
			wants = true
		}
		delete(fields, "save_to_disk")
		delete(fields, "save_to")
		if b, err := json.Marshal(fields); err == nil {
			out[i].Input = b
		}
	}
	return out, wants
}

func (t BatchTool) Execute(ctx context.Context, in BatchInput) (Result, error) {
	c, err := resolve(t.Client)
	if err != nil {
		return Result{}, err
	}
	if len(in.Actions) == 0 {
		return Result{}, errors.New("browser_batch requires at least one action")
	}
	actions, wantsSave := stripSaveFlags(in.Actions)
	actions = setShotFlags(actions, t.AutoScreenshot)
	wantsSave = wantsSave || in.SaveToDisk

	var all []batchResult
	callCtx := ctx
	for start := 0; start < len(actions); {
		raw, err := c.Call(callCtx, chromehost.MethodBrowserBatch, BatchInput{Actions: actions[start:]})
		if err != nil {
			if len(all) == 0 {
				return Result{}, err
			}
			all = append(all, batchResult{Name: actions[start].Name, Error: err.Error()})
			break
		}
		var r struct {
			Results []batchResult `json:"results"`
		}
		_ = json.Unmarshal(raw, &r)
		if len(r.Results) == 0 {
			break
		}
		last := r.Results[len(r.Results)-1]
		na, needs := parseNeedsApproval(last.Error)
		if !needs || t.Approve == nil {
			all = append(all, r.Results...)
			break
		}
		// The step that needs approval did not run: keep the steps before it,
		// ask, and resume at it.
		all = append(all, r.Results[:len(r.Results)-1]...)
		if aerr := t.Approve(ctx, na.Site); aerr != nil {
			last.Error = aerr.Error()
			all = append(all, last)
			break
		}
		start += len(r.Results) - 1
		callCtx = WithApprovedSites(callCtx, na.Site)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "browser_batch ran %d of %d step(s):\n", len(all), len(in.Actions))
	var images []Image
	fenced := false
	for i, res := range all {
		switch {
		case res.Error != "":
			fmt.Fprintf(&b, "%d. %s: error: %s\n", i+1, res.Name, res.Error)
		case res.DataURL != "":
			if img, derr := decodeDataURL(res.DataURL); derr == nil {
				images = append(images, img)
				line := "image captured"
				var ci captureInfo
				var after struct {
					Shot json.RawMessage `json:"shot"`
				}
				if json.Unmarshal(res.Result, &after) == nil && len(after.Shot) > 0 {
					// A computer action's screenshot after itself.
					line = "ok; screenshot after the action attached"
					if detail, _ := stepDetail(res.Name, res.Result); detail != "" {
						line = "ok: " + strings.TrimSuffix(detail, ".") + "; screenshot after the action attached"
					}
				} else if json.Unmarshal(res.Result, &ci) == nil {
					if note := ci.frameNote(); note != "" {
						line += ". " + note
					}
				}
				fmt.Fprintf(&b, "%d. %s: %s\n", i+1, res.Name, line)
			} else {
				fmt.Fprintf(&b, "%d. %s: ok\n", i+1, res.Name)
			}
		case res.ImageOmitted != "":
			fmt.Fprintf(&b, "%d. %s: ok (%s)\n", i+1, res.Name, res.ImageOmitted)
		default:
			detail, page := stepDetail(res.Name, res.Result)
			fenced = fenced || page
			switch {
			case detail == "":
				fmt.Fprintf(&b, "%d. %s: ok\n", i+1, res.Name)
			case strings.Contains(detail, "\n"):
				fmt.Fprintf(&b, "%d. %s: ok\n%s\n", i+1, res.Name, detail)
			default:
				fmt.Fprintf(&b, "%d. %s: ok: %s\n", i+1, res.Name, detail)
			}
		}
		if len(res.Notes) > 0 {
			fenced = true
			b.WriteString(strings.TrimPrefix(pageNotes(res.Notes, false), "\n\n") + "\n")
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	if fenced {
		text += "\n\n" + UntrustedTextNotice
	}
	if len(images) > 0 {
		text += "\n\n" + UntrustedImageNotice
		if wantsSave {
			text = appendLines(text, saveImages(t.Saver, images, true, ""))
		}
	}
	return Result{Text: text, Images: images}, nil
}

// WithSiteApproval runs call; when it fails because the extension wants a site
// approved and approve grants it, call runs once more with that site allowed.
func WithSiteApproval(ctx context.Context, approve ApproveFunc, call func(ctx context.Context) (Result, error)) (Result, error) {
	res, err := call(ctx)
	if err == nil || approve == nil {
		return res, err
	}
	na, ok := AsNeedsApproval(err)
	if !ok {
		return res, err
	}
	if aerr := approve(ctx, na.Site); aerr != nil {
		return Result{}, aerr
	}
	return call(WithApprovedSites(ctx, na.Site))
}

// decodeDataURL parses a "data:<mediaType>;base64,<payload>" URL into an Image.
// The payload is kept base64-encoded (validated by a decode round-trip) so it
// can be handed straight to the image content block.
func decodeDataURL(dataURL string) (Image, error) {
	if dataURL == "" {
		return Image{}, errors.New("screenshot returned no image data")
	}
	const prefix = "data:"
	if !strings.HasPrefix(dataURL, prefix) {
		return Image{}, errors.New("screenshot returned a malformed data URL")
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return Image{}, errors.New("screenshot returned a malformed data URL (no payload)")
	}
	meta := dataURL[len(prefix):comma]
	payload := dataURL[comma+1:]
	if !strings.Contains(meta, "base64") {
		return Image{}, errors.New("screenshot data URL is not base64-encoded")
	}
	mediaType := meta
	if i := strings.IndexByte(meta, ';'); i >= 0 {
		mediaType = meta[:i]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return Image{}, fmt.Errorf("screenshot data URL has invalid base64: %w", err)
	}
	return Image{Base64: payload, MediaType: mediaType}, nil
}
