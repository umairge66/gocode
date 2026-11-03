package toolrun

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/umairge66/gocode/internal/agent"
	"github.com/umairge66/gocode/internal/protocol"
	"github.com/umairge66/gocode/internal/tools/browser"
	"github.com/umairge66/gocode/internal/tools/filetools"
	"github.com/umairge66/gocode/internal/tools/interact"
)

// toolInputSchemas holds the JSON input schemas for the dispatchable tools whose
// packages do not expose one themselves (exec / filetools / interact). The fileedit
// and web tools carry their own InputSchema() and are wired from there. These
// schemas mirror the tools' input structs.
var toolInputSchemas = map[protocol.Tool]json.RawMessage{
	protocol.ToolBash: json.RawMessage(`{"type":"object","properties":{` +
		`"command":{"type":"string","description":"The shell command to execute."},` +
		`"timeout":{"type":"number","description":"Optional timeout in milliseconds."},` +
		`"description":{"type":"string","description":"Optional short description of what the command does."},` +
		`"run_in_background":{"type":"boolean","description":"Set to true to run this command in the background."},` +
		`"dangerouslyDisableSandbox":{"type":"boolean","description":"Set this to true to dangerously override sandbox mode and run commands without sandboxing."}` +
		`},"required":["command"]}`),
	// Glob and Grep advertise every parameter the tools accept, with full
	// descriptions. A thin schema hides options such
	// as -i, so the model falls back to `grep -r -i` in Bash.
	protocol.ToolGlob: json.RawMessage(`{"type":"object","properties":{` +
		`"pattern":{"type":"string","description":"The glob pattern to match files against"},` +
		`"path":{"type":"string","description":"The directory to search in. If not specified, the current working directory will be used. IMPORTANT: Omit this field to use the default directory. DO NOT enter \"undefined\" or \"null\" - simply omit it for the default behavior. Must be a valid directory path if provided."}` +
		`},"required":["pattern"]}`),
	protocol.ToolGrep: json.RawMessage(`{"type":"object","properties":{` +
		`"pattern":{"type":"string","description":"The regular expression pattern to search for in file contents"},` +
		`"path":{"type":"string","description":"File or directory to search in (rg PATH). Defaults to current working directory."},` +
		`"glob":{"type":"string","description":"Glob pattern to filter files (e.g. \"*.js\", \"*.{ts,tsx}\") - maps to rg --glob"},` +
		`"output_mode":{"type":"string","enum":["content","files_with_matches","count"],"description":"Output mode: \"content\" shows matching lines (supports -A/-B/-C context, -n line numbers, head_limit), \"files_with_matches\" shows file paths (supports head_limit), \"count\" shows match counts (supports head_limit). Defaults to \"files_with_matches\"."},` +
		`"-B":{"type":"number","description":"Number of lines to show before each match (rg -B). Requires output_mode: \"content\", ignored otherwise."},` +
		`"-A":{"type":"number","description":"Number of lines to show after each match (rg -A). Requires output_mode: \"content\", ignored otherwise."},` +
		`"-C":{"type":"number","description":"Alias for context."},` +
		`"context":{"type":"number","description":"Number of lines to show before and after each match (rg -C). Requires output_mode: \"content\", ignored otherwise."},` +
		`"-n":{"type":"boolean","description":"Show line numbers in output (rg -n). Requires output_mode: \"content\", ignored otherwise. Defaults to true."},` +
		`"-i":{"type":"boolean","description":"Case insensitive search (rg -i)"},` +
		`"-o":{"type":"boolean","description":"Print only the matched (non-empty) parts of each matching line, one match per output line (rg -o / --only-matching). Requires output_mode: \"content\", ignored otherwise. Defaults to false."},` +
		`"type":{"type":"string","description":"File type to search (rg --type). Common types: js, py, rust, go, java, etc. More efficient than include for standard file types."},` +
		`"head_limit":{"type":"number","description":"Limit output to first N lines/entries, equivalent to \"| head -N\". Works across all output modes: content (limits output lines), files_with_matches (limits file paths), count (limits count entries). Defaults to 250 when unspecified. Pass 0 for unlimited (use sparingly — large result sets waste context)."},` +
		`"offset":{"type":"number","description":"Skip first N lines/entries before applying head_limit, equivalent to \"| tail -n +N | head -N\". Works across all output modes. Defaults to 0."},` +
		`"multiline":{"type":"boolean","description":"Enable multiline mode where . matches newlines and patterns can span lines (rg -U --multiline-dotall). Default: false."}` +
		`},"required":["pattern"]}`),
	protocol.ToolWrite: json.RawMessage(`{"type":"object","properties":{` +
		`"file_path":{"type":"string","description":"The absolute path of the file to write."},` +
		`"content":{"type":"string","description":"The full file content to write."}` +
		`},"required":["file_path","content"]}`),
	protocol.ToolNotebookEdit: json.RawMessage(`{"type":"object","properties":{` +
		`"notebook_path":{"type":"string","description":"The absolute path to the Jupyter notebook to edit."},` +
		`"cell_id":{"type":"string","description":"The ID of the cell to edit."},` +
		`"new_source":{"type":"string","description":"The new source for the cell."},` +
		`"cell_type":{"type":"string","enum":["code","markdown"],"description":"The cell type; required when edit_mode is insert."},` +
		`"edit_mode":{"type":"string","enum":["replace","insert","delete"],"description":"The edit mode; defaults to replace."}` +
		`},"required":["notebook_path","new_source"]}`),
	protocol.ToolTodoWrite: json.RawMessage(`{"type":"object","properties":{` +
		`"todos":{"type":"array","description":"The updated todo list","items":{"type":"object","properties":{` +
		`"content":{"type":"string","minLength":1,"description":"The task, in imperative form."},` +
		`"status":{"type":"string","enum":["pending","in_progress","completed"]},` +
		`"activeForm":{"type":"string","minLength":1,"description":"The task, in present continuous form."}` +
		`},"required":["content","status","activeForm"]}}` +
		`},"required":["todos"]}`),
	// Browser-control tools (milestone 2, internal/tools/browser). Schemas mirror
	// the gocode in-chrome tool params and the milestone-1 extension verb shapes.
	protocol.ToolBrowserNavigate: json.RawMessage(`{"type":"object","properties":{` +
		`"url":{"type":"string","description":"The URL to navigate to, with or without protocol (defaults to https://). Use \"back\" or \"forward\" (with a tabId) to go through the tab's history."},` +
		`"tabId":{"type":"number","description":"A gocode tab to navigate; omit to use gocode's current tab (a new one is opened in the gocode group if there is none)."},` +
		`"keep_open":{"type":"boolean","description":"` + browser.KeepOpenDescription + `"}` +
		`},"required":["url"]}`),
	protocol.ToolBrowserReadPage: json.RawMessage(`{"type":"object","properties":{` +
		`"tabId":{"type":"number","description":"A gocode tab to read; omit to read gocode's current tab."},` +
		`"offset":{"type":"number","description":"Character to start from, to read a long page in parts (the reply gives the next offset). Default 0."},` +
		`"max_chars":{"type":"number","description":"Most characters to return (default 40000)."}` +
		`}}`),
	protocol.ToolBrowserGetPageText: json.RawMessage(`{"type":"object","properties":{` +
		`"tabId":{"type":"number","description":"A gocode tab to read; omit to read gocode's current tab."},` +
		`"offset":{"type":"number","description":"Character to start from, to read a long page in parts (the reply gives the next offset). Default 0."},` +
		`"max_chars":{"type":"number","description":"Most characters to return (default 40000)."}` +
		`}}`),
	protocol.ToolBrowserTabsContext: json.RawMessage(`{"type":"object","properties":{}}`),
	protocol.ToolBrowserTabsCreate: json.RawMessage(`{"type":"object","properties":{` +
		`"url":{"type":"string","description":"Optional URL to open in the new tab."},` +
		`"keep_open":{"type":"boolean","description":"` + browser.KeepOpenDescription + `"}` +
		`}}`),
	protocol.ToolBrowserTabsClose: json.RawMessage(`{"type":"object","properties":{` +
		`"tabId":{"type":"number","description":"The id of the gocode tab to close."}` +
		`},"required":["tabId"]}`),
	protocol.ToolBrowserScreenshot: json.RawMessage(`{"type":"object","properties":{` +
		`"tabId":{"type":"number","description":"A gocode tab to capture; omit to capture gocode's current tab."},` +
		`"windowId":{"type":"number","description":"Ignored; the screenshot is always of a gocode tab."},` +
		`"save_to_disk":{"type":"boolean","description":"` + saveToDiskDescription + `"},` +
		`"save_to":{"type":"string","description":"` + saveToDescription + `"}` +
		`}}`),
	// Browser interaction tools (milestone 3, chrome.debugger / CDP). Schemas
	// mirror the gocode in-chrome tool params: computer action vocab +
	// coordinate/text/scroll/ref fields; find
	// {query,tabId}; form_input {ref,value,tabId}; browser_batch {actions}.
	protocol.ToolBrowserComputer: json.RawMessage(`{"type":"object","properties":{` +
		`"action":{"type":"string","enum":["left_click","right_click","middle_click","double_click","triple_click","type","key","scroll","left_click_drag","hover","mouse_move","scroll_to","screenshot","zoom","wait"],"description":"The gesture to perform."},` +
		`"coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) viewport pixels. Required for click/scroll/hover; the end position for left_click_drag."},` +
		`"start_coordinate":{"type":"array","items":{"type":"number"},"minItems":2,"maxItems":2,"description":"(x, y) starting position for left_click_drag."},` +
		`"text":{"type":"string","description":"The text to type (type action) or the key(s) to press (key action): space-separated keys (e.g. \"Backspace Backspace Delete\"), chords joined with + (e.g. \"ctrl+a\", \"shift+Tab\", \"alt+x\", \"ctrl+shift+t\"), numeric keypad keys as Numpad0-Numpad9, NumpadEnter, NumpadAdd, NumpadSubtract, NumpadMultiply, NumpadDivide, NumpadDecimal. Use \"cmd\" on Mac and \"ctrl\" on Windows/Linux for shortcuts. Page zoom shortcuts (e.g. \"cmd+=\", \"ctrl+-\", \"cmd+0\") are not supported and return an error - use the zoom action instead."},` +
		`"scroll_direction":{"type":"string","enum":["up","down","left","right"],"description":"Direction to scroll. Required for scroll."},` +
		`"scroll_amount":{"type":"number","minimum":1,"maximum":10,"description":"Scroll wheel ticks (default 3)."},` +
		`"duration":{"type":"number","minimum":0,"maximum":30,"description":"Seconds to wait (at most 30). Required for wait."},` +
		`"region":{"type":"array","items":{"type":"number"},"minItems":4,"maxItems":4,"description":"(x0,y0,x1,y1) viewport region to capture for zoom, magnified for a closer look. Required for zoom."},` +
		`"scale":{"type":"number","minimum":0.1,"maximum":1,"description":"For screenshot and zoom: shrink the returned image to this fraction (default 1). Coordinates for clicks stay in the full viewport frame, never in the image's own pixels."},` +
		`"ref":{"type":"string","description":"Element reference id from find; alternative to coordinate for clicks (the element is scrolled into view first), required for scroll_to."},` +
		`"modifiers":{"type":"string","description":"Modifier keys for click actions: \"ctrl\", \"shift\", \"alt\", \"cmd\" (or \"meta\"), \"win\" (or \"windows\"), combined with + (e.g. \"ctrl+shift\")."},` +
		`"repeat":{"type":"number","minimum":1,"maximum":100,"description":"Number of times to repeat the key sequence (key action only, 1-100, default 1)."},` +
		`"tabId":{"type":"number","description":"A gocode tab to act on; omit to use gocode's current tab."},` +
		`"screenshot":{"type":"boolean","description":"For clicks, type, key, scroll, scroll_to, left_click_drag and hover: return a screenshot of the page after the action (default true, unless turned off in the settings). Set false to skip it when you will not need to see the result."},` +
		`"save_to_disk":{"type":"boolean","description":"` + saveToDiskDescription + `"},` +
		`"save_to":{"type":"string","description":"` + saveToDescription + `"}` +
		`},"required":["action"]}`),
	protocol.ToolBrowserFind: json.RawMessage(`{"type":"object","properties":{` +
		`"query":{"type":"string","description":"What to find: a label, text, placeholder or name, optionally with the kind of element (e.g. \"search bar\", \"add to cart button\", \"email field\", \"country dropdown\", \"input fields\")."},` +
		`"limit":{"type":"number","minimum":1,"maximum":100,"description":"At most this many matches (default 20)."},` +
		`"includeHidden":{"type":"boolean","description":"Also return elements that are not visible (default false)."},` +
		`"tabId":{"type":"number","description":"A gocode tab to search; omit to use gocode's current tab."}` +
		`},"required":["query"]}`),
	protocol.ToolBrowserFormInput: json.RawMessage(`{"type":"object","properties":{` +
		`"ref":{"type":"string","description":"Element reference id from read_page or find (e.g. \"ref_1\")."},` +
		`"value":{"type":["string","boolean","number"],"description":"The value to set (boolean for checkboxes, option value/text for selects, string/number otherwise)."},` +
		`"tabId":{"type":"number","description":"A gocode tab to act on; omit to use gocode's current tab."}` +
		`},"required":["ref","value"]}`),
	protocol.ToolBrowserBatch: json.RawMessage(`{"type":"object","properties":{` +
		`"actions":{"type":"array","minItems":1,"description":"Browser sub-commands to run sequentially; stops on the first error. browser_batch cannot be nested.","items":{"type":"object","properties":{` +
		`"name":{"type":"string","description":"Tool name (e.g. computer, navigate, find, form_input)."},` +
		`"input":{"type":"object","description":"That tool's input, the same shape you'd pass when calling it directly."}` +
		`},"required":["name","input"]}}` +
		`},"required":["actions"]}`),
	protocol.ToolAskUserQuestion: json.RawMessage(`{"type":"object","properties":{` +
		`"questions":{"type":"array","minItems":1,"maxItems":4,"description":"Questions to ask the user (1-4 questions)","items":{"type":"object","properties":{` +
		`"question":{"type":"string","description":"The complete question to ask the user. Should be clear, specific, and end with a question mark."},` +
		`"header":{"type":"string","description":"Very short label displayed as a chip/tag (max 12 chars)."},` +
		`"options":{"type":"array","minItems":2,"maxItems":4,"description":"The available choices for this question.","items":{"type":"object","properties":{` +
		`"label":{"type":"string","description":"The display text for this option (1-5 words)."},` +
		`"description":{"type":"string","description":"Explanation of what this option means."},` +
		`"preview":{"type":"string","description":"Optional preview content shown when this option is focused."}` +
		`},"required":["label"]}},` +
		`"multiSelect":{"type":"boolean","description":"Allow selecting multiple options."}` +
		`},"required":["question","header"]}}` +
		`},"required":["questions"]}`),
}

// bashDescription is the Bash tool description: the standard opening plus
// the guidance steering the model to the dedicated file tools instead of
// find/grep/cat/head/tail/sed/awk/echo through the shell.
const bashDescription = "Executes a given bash command and returns its output.\n\n" +
	"The working directory persists between commands, but shell state does not. The shell environment is initialized from the user's profile (bash or zsh).\n\n" +
	"IMPORTANT: Avoid using this tool to run `find`, `grep`, `cat`, `head`, `tail`, `sed`, `awk`, or `echo` commands, unless explicitly instructed or after you have verified that a dedicated tool cannot accomplish your task. Instead, use the appropriate dedicated tool as this will provide a much better experience for the user:\n\n" +
	" - File search: Use Glob (NOT find or ls)\n" +
	" - Content search: Use Grep (NOT grep or rg)\n" +
	" - Read files: Use Read (NOT cat/head/tail)\n" +
	" - Edit files: Use Edit (NOT sed/awk)\n" +
	" - Write files: Use Write (NOT echo >/cat <<EOF)\n" +
	" - Communication: Output text directly (NOT echo/printf)\n" +
	"While the Bash tool can do similar things, it\u2019s better to use the built-in tools as they provide a better user experience and make it easier to review tool calls and give permission.\n\n" +
	"You can use the `run_in_background` parameter to run the command in the background. Only use this if you don't need the result immediately and are OK being notified when the command completes later. You do not need to check the output right away - you'll be notified when it finishes. You do not need to use '&' at the end of the command when using this parameter. To stop a background command, use the TaskStop tool with its ID."

// bashDescriptionGrepViaBash is bashDescription for a session whose `grep`
// runs ugrep (WithUgrepShadow): content search happens with `grep` in this
// tool, so grep leaves the list of commands to avoid and the Grep line goes.
var bashDescriptionGrepViaBash = strings.NewReplacer(
	"Avoid using this tool to run `find`, `grep`, `cat`,", "Avoid using this tool to run `find`, `cat`,",
	" - Content search: Use Grep (NOT grep or rg)\n", "",
).Replace(bashDescription)

// toolDescriptions is the description advertised with each of the tools in
// toolInputSchemas.
var toolDescriptions = map[protocol.Tool]string{
	protocol.ToolBash:            bashDescription,
	protocol.ToolGlob:            filetools.GlobTool{}.Description(),
	protocol.ToolGrep:            filetools.GrepTool{}.Description(),
	protocol.ToolWrite:           "Write a file to the local filesystem, overwriting if it exists.",
	protocol.ToolNotebookEdit:    "Edit, insert, or delete a cell in a Jupyter notebook.",
	protocol.ToolTodoWrite:       interact.TodoWritePrompt,
	protocol.ToolAskUserQuestion: interact.AskUserQuestionDescription,

	protocol.ToolBrowserNavigate:    "Navigate a gocode tab to a URL in the user's real Chrome (via the local gocode browser extension). Omit tabId to use this session's current gocode tab; when the session has none, a new tab is opened in its \"gocode\" tab group, and the group's tab list is appended to the result so you have the tab ids. gocode can only use tabs in its own \"gocode\" tab group; the user's other tabs are off limits. A tab opened for you this way is yours to clean up, the same as one from tabs_create: close it with tabs_close once you no longer need it and before finishing your task. If the user asked to see the page or wants it kept open, set keep_open instead of leaving it open; the other tabs gocode opened are closed when the session ends.",
	protocol.ToolBrowserReadPage:    "Read the visible text of a gocode tab (via the local gocode browser extension). Omit tabId to read gocode's current tab.",
	protocol.ToolBrowserGetPageText: "Extract the readable text content of a gocode tab, prioritizing article content (via the local gocode browser extension). Omit tabId to read gocode's current tab.",
	protocol.ToolBrowserTabsContext: "List this session's Chrome tabs (id, url, title, active) — the tabs in its \"gocode\" tab group. The user's other tabs are never listed and cannot be used. Get the context at least once before using the other browser tools so you know what tabs exist.",
	protocol.ToolBrowserTabsCreate:  "Open a new tab (optionally at a URL) in this session's \"gocode\" tab group in the user's real Chrome, via the local gocode browser extension. It becomes the session's current tab. Tabs you create are yours to clean up: close each one with tabs_close as soon as you no longer need it, and close any that remain before finishing your task. If the user asked to see a page or wants it kept open, set keep_open instead of leaving it open; the other tabs gocode opened are closed when the session ends.",
	protocol.ToolBrowserTabsClose:   "Close one of gocode's tabs by its id via the local gocode browser extension. Use to clean up tabs you're done with. Only tabs in gocode's \"gocode\" tab group are closable; call tabs_context first to get valid ids. If you close the group's last tab, Chrome removes the group — the next tab you open starts a fresh one.",
	protocol.ToolBrowserScreenshot:  "Capture a screenshot of gocode's current tab via the local gocode browser extension; returns the image so you can see the page.",

	protocol.ToolBrowserComputer:  computerDescription,
	protocol.ToolBrowserFind:      "Find visible elements on a Chrome tab by label, text, placeholder or name and by kind (\"input\", \"text field\", \"button\", \"link\", \"checkbox\", \"dropdown\", \"form\", ...); a label finds the field it labels. Returns the best matches first (up to 20) with reference ids and center coordinates you can pass to computer/form_input. Refs belong to the current page: after a navigation or reload, run find again. Omit tabId to use gocode's current tab.",
	protocol.ToolBrowserFormInput: "Set the value of a form field in a Chrome tab by its element reference id (from find) and fire input/change events: text, textarea, contenteditable, select (option value or text; comma-separated for a multiple select), checkbox (true/false), radio (true, or the option's value or label), date (YYYY-MM-DD). Fails if the field did not take the value. Omit tabId to use gocode's current tab.",
	protocol.ToolBrowserBatch:     batchDescription,
}

// The computer and browser_batch descriptions, with the screenshot a
// page-changing action returns after itself (the default) and without it
// (turned off in the settings, see WithBrowserScreenshotAfterActions).
const (
	computerDescriptionBase = "Use a mouse and keyboard to interact with a Chrome tab (click/type/key/scroll/hover/drag), and take screenshots, via chrome.debugger in the user's real browser. Coordinates are viewport pixels; consult a screenshot first. Omit tabId to use gocode's current tab."
	computerDescription     = computerDescriptionBase + " After a click, type, key, scroll, scroll_to, left_click_drag or hover, a screenshot of the page (taken once it settled) is returned with the result, in the same coordinate frame as the screenshot action, so you do not need a separate screenshot call to see what happened; pass screenshot:false to skip it."
	// computerDescriptionNoAutoShot is used when the setting turns the
	// screenshot after actions off.
	computerDescriptionNoAutoShot = computerDescriptionBase + " Pass screenshot:true on a click, type, key, scroll, scroll_to, left_click_drag or hover to also get a screenshot of the page after it."
	batchDescriptionBase          = "Run a sequence of browser tool calls (computer/navigate/find/form_input/...) in ONE round trip; they execute sequentially and stop on the first error, and each step's result (find refs, page text, javascript values, new tab ids, images) is returned. Use when you can predict two or more steps ahead. Cannot be nested."
	batchDescription              = batchDescriptionBase + " When the last step is a computer click, type, key, scroll or similar, a screenshot of the page after it is returned (only for the last step); no extra screenshot step is needed."
	batchDescriptionNoAutoShot    = batchDescriptionBase
)

// Schemas returns the tool schema (name, description, input schema) of every
// dispatchable tool registered on this Dispatcher that has an input schema. It
// is the single source of truth for what the model is offered: a tool appears
// here exactly when Dispatch can run it.
//
// Order is stable: tools known to the Registry come first in registration
// order, followed by any other registered adapters sorted by name, then the
// external (MCP) provider's tools. A Restrict list filters all of them. With
// deferred loading on (WithToolSearch) the non-core tools come back marked
// Deferred; the agent loop withholds them until ToolSearch loads them.
func (d *Dispatcher) Schemas() []agent.ToolSchema {
	seen := make(map[protocol.Tool]bool, len(d.adapters))
	var out []agent.ToolSchema
	add := func(name protocol.Tool) {
		if seen[name] {
			return
		}
		seen[name] = true
		ad, ok := d.adapters[name]
		if !ok || len(ad.inputSchema) == 0 || !d.permitted(string(name)) {
			return
		}
		desc, schema := ad.description, ad.inputSchema
		if d.compact {
			if h, ok := compactTexts[name]; ok {
				desc, schema = h.description, h.inputSchema
			}
		}
		out = append(out, agent.ToolSchema{
			Name:        string(name),
			Description: desc,
			InputSchema: schema,
		})
	}
	if d.Registry != nil {
		for _, desc := range d.Registry.List() {
			add(desc.Name)
		}
	}
	rest := make([]protocol.Tool, 0)
	for name := range d.adapters {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	for _, name := range rest {
		add(name)
	}
	// External (MCP) tools follow the built-ins, sorted by name: servers
	// connect concurrently, so their own order varies from run to run, and
	// the tools array leads the prompt prefix providers cache.
	ext := d.externalSchemas()
	sort.SliceStable(ext, func(i, j int) bool { return ext[i].Name < ext[j].Name })
	external := make(map[string]bool, len(ext))
	for _, s := range ext {
		external[s.Name] = true
	}
	out = append(out, ext...)
	return d.markDeferred(out, external)
}

// Screenshot-saving parameter descriptions shared by the computer and
// screenshot tools. save_to is the explicit-path variant of save_to_disk,
// checked like a Write.
const (
	saveToDiskDescription = "For screenshot/zoom: also save the image to disk so it can be attached to a message for the user. Returns the saved path in the tool result. Only set this when you intend to share the image — screenshots you're just looking at don't need saving."
	saveToDescription     = "For screenshot/zoom: also write the image (PNG) to this absolute file path. The path is checked like a Write (permission rules and prompts apply). The image is still returned to you."
)
